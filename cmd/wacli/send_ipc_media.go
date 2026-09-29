package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

// mediaDownloadApp is what downloading one message's media needs: *app.App,
// connected, in production.
type mediaDownloadApp interface {
	DB() *store.DB
	WA() app.WAClient
	ResolveMediaOutputPath(store.MediaDownloadInfo, string) (string, error)
}

// delegatedJobApp is what the job kinds need from the running sync process.
type delegatedJobApp interface {
	mediaDownloadApp
	RetryMedia(context.Context, app.RetryMediaOptions) (app.MediaRetryResult, error)
	BackfillMedia(context.Context, app.BackfillMediaOptions) (app.BackfillMediaResult, error)
	BackfillHistoryConnected(context.Context, app.BackfillOptions) (app.BackfillResult, error)
}

// executeDelegatedJob runs a job kind inside the sync process that owns the
// store, with the same command core as the direct run.
func executeDelegatedJob(ctx context.Context, a delegatedJobApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	var args delegateJobArgs
	if req.Job != nil {
		args = *req.Job
	}
	ctx = withAppOperationEvents(ctx)
	switch req.Kind {
	case mediaDownloadKind:
		info, target, err := loadMediaDownload(a, req.Chat, req.ID, args.Output)
		if err != nil {
			return sendDelegateResponse{}, err
		}
		res, err := downloadMediaTo(ctx, a, info, target)
		if err != nil {
			return sendDelegateResponse{}, err
		}
		return delegateJobResult(res)
	case mediaRetryKind:
		res, err := a.RetryMedia(ctx, args.retryOptions(req.Chat, req.ID))
		if err != nil {
			return sendDelegateResponse{}, err
		}
		return delegateJobResult(res)
	case mediaBackfillKind:
		res, err := a.BackfillMedia(ctx, app.BackfillMediaOptions{ChatJID: req.Chat, Limit: args.Limit, Workers: args.Workers})
		if err != nil {
			return sendDelegateResponse{}, err
		}
		return delegateJobResult(res)
	case historyBackfillKind:
		res, err := a.BackfillHistoryConnected(ctx, args.historyOptions(req.Chat))
		if err != nil {
			return sendDelegateResponse{}, err
		}
		return delegateJobResult(res)
	default:
		return sendDelegateResponse{}, fmt.Errorf("unsupported send kind %q", req.Kind)
	}
}

// mediaDownloadResult is a recorded media download.
type mediaDownloadResult struct {
	Chat         string    `json:"chat"`
	ID           string    `json:"id"`
	Path         string    `json:"path"`
	Bytes        int64     `json:"bytes"`
	MediaType    string    `json:"media_type"`
	MimeType     string    `json:"mime_type"`
	DownloadedAt time.Time `json:"downloaded_at"`
}

func writeMediaDownloadResult(flags *rootFlags, r mediaDownloadResult) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{
			"chat":          r.Chat,
			"id":            r.ID,
			"path":          r.Path,
			"bytes":         r.Bytes,
			"media_type":    r.MediaType,
			"mime_type":     r.MimeType,
			"downloaded":    true,
			"downloaded_at": r.DownloadedAt.Format(time.RFC3339Nano),
		})
	}
	fmt.Fprintf(os.Stdout, "%s (%d bytes)\n", r.Path, r.Bytes)
	return nil
}

// loadMediaDownload reads a message's download metadata and where its media
// goes. It needs no connection.
func loadMediaDownload(a interface {
	DB() *store.DB
	ResolveMediaOutputPath(store.MediaDownloadInfo, string) (string, error)
}, chat, id, output string) (store.MediaDownloadInfo, string, error) {
	info, err := a.DB().GetMediaDownloadInfo(chat, id)
	if err != nil {
		return store.MediaDownloadInfo{}, "", err
	}
	if info.MediaType == "" || info.DirectPath == "" || len(info.MediaKey) == 0 {
		return store.MediaDownloadInfo{}, "", fmt.Errorf("message has no downloadable media metadata (run `wacli sync` first)")
	}
	target, err := a.ResolveMediaOutputPath(info, output)
	if err != nil {
		return store.MediaDownloadInfo{}, "", err
	}
	return info, target, nil
}

// downloadMediaTo downloads over the connected client and records the local
// path and download time.
func downloadMediaTo(ctx context.Context, a mediaDownloadApp, info store.MediaDownloadInfo, target string) (mediaDownloadResult, error) {
	bytes, err := a.WA().DownloadMediaToFile(ctx, info.DirectPath, info.FileEncSHA256, info.FileSHA256, info.MediaKey, info.FileLength, info.MediaType, "", target)
	if err != nil {
		return mediaDownloadResult{}, err
	}
	now := time.Now().UTC()
	if err := a.DB().MarkMediaDownloaded(info.ChatJID, info.MsgID, target, now); err != nil {
		return mediaDownloadResult{}, fmt.Errorf("record media download: %w", err)
	}
	return mediaDownloadResult{
		Chat:         info.ChatJID,
		ID:           info.MsgID,
		Path:         target,
		Bytes:        bytes,
		MediaType:    info.MediaType,
		MimeType:     info.MimeType,
		DownloadedAt: now,
	}, nil
}

// downloadThroughSync hands a media download to a same-store `sync --follow`.
// Without one, the lock error names the path that needs no lock.
func downloadThroughSync(ctx context.Context, flags *rootFlags, lockErr error, chat, id, output string) error {
	output, err := absoluteOutputPath(output)
	if err != nil {
		return err
	}
	req := sendDelegateRequest{Kind: mediaDownloadKind, Chat: chat, ID: id, Job: &delegateJobArgs{Output: output}}
	resp, delegated, err := tryDelegateJob(ctx, flags, lockErr, req, max(flags.timeout, 0))
	if !delegated {
		if lock.IsLocked(err) {
			return fmt.Errorf("%w; to download while sync is running, use --read-only with --output PATH (it takes no store lock, and does not record local_path)", err)
		}
		return err
	}
	if err != nil {
		return err
	}
	var res mediaDownloadResult
	if err := decodeDelegateJobResult(resp, &res); err != nil {
		return err
	}
	return writeMediaDownloadResult(flags, res)
}

// absoluteOutputPath resolves --output against this process's directory, not
// the sync process's, keeping a trailing separator that marks a directory.
func absoluteOutputPath(output string) (string, error) {
	if strings.TrimSpace(output) == "" || filepath.IsAbs(output) {
		return output, nil
	}
	abs, err := filepath.Abs(output)
	if err != nil {
		return "", fmt.Errorf("resolve --output: %w", err)
	}
	if strings.HasSuffix(output, string(os.PathSeparator)) {
		abs += string(os.PathSeparator)
	}
	return abs, nil
}

func retryJobArgs(opts app.RetryMediaOptions) *delegateJobArgs {
	return &delegateJobArgs{
		MediaTypes: opts.MediaTypes,
		BeforeUnix: opts.BeforeUnix,
		BeforeSet:  opts.BeforeSet,
		Limit:      opts.Limit,
		Batch:      opts.BatchSize,
		WaitMS:     durationMillis(opts.Wait),
	}
}

func (args delegateJobArgs) retryOptions(chat, id string) app.RetryMediaOptions {
	return app.RetryMediaOptions{
		ChatJID:    chat,
		MsgID:      id,
		MediaTypes: args.MediaTypes,
		BeforeUnix: args.BeforeUnix,
		BeforeSet:  args.BeforeSet,
		Limit:      args.Limit,
		BatchSize:  args.Batch,
		Wait:       millisDuration(args.WaitMS, 0),
	}
}

func writeMediaRetryResult(flags *rootFlags, res app.MediaRetryResult) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, res)
	}
	fmt.Fprintf(os.Stdout, "Requested: %d  Recovered: %d  Not on phone: %d  No response: %d  Failed: %d\n",
		res.Requested, res.Recovered, res.NotOnPhone, res.NoResponse, res.Failed)
	for _, o := range res.Outcomes {
		line := fmt.Sprintf("  %-13s %s/%s", o.Status, o.ChatJID, o.MsgID)
		if o.Status == "recovered" {
			line += fmt.Sprintf("  (%d bytes) %s", o.Bytes, o.Path)
		} else if o.Detail != "" {
			line += "  " + o.Detail
		}
		fmt.Fprintln(os.Stdout, line)
	}
	return nil
}

func writeMediaBackfillResult(flags *rootFlags, res app.BackfillMediaResult) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{
			"pending":    res.Pending,
			"attempted":  res.Attempted,
			"downloaded": res.Downloaded,
			"skipped":    res.Skipped,
			"failed":     res.Failed,
		})
	}
	fmt.Fprintf(os.Stdout, "Pending: %d  Attempted: %d  Downloaded: %d  Skipped: %d  Failed: %d\n",
		res.Pending, res.Attempted, res.Downloaded, res.Skipped, res.Failed)
	return nil
}
