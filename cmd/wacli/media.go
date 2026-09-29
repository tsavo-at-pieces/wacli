package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
)

func newMediaCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "media",
		Short: "Media download and transcription",
	}
	cmd.AddCommand(newMediaDownloadCmd(flags))
	cmd.AddCommand(newMediaTranscribeCmd(flags))
	cmd.AddCommand(newMediaBackfillCmd(flags))
	cmd.AddCommand(newMediaRetryCmd(flags))
	return cmd
}

func newMediaRetryCmd(flags *rootFlags) *cobra.Command {
	var chat string
	var id string
	var mediaType string
	var limit int
	var batch int
	var wait time.Duration
	var before string

	cmd := &cobra.Command{
		Use:   "retry",
		Short: "Recover expired media by asking the phone to re-upload it",
		Long: "For media that expired off WhatsApp's CDN, ask the primary device (phone)\n" +
			"to re-upload it via the media-retry protocol, then download it. Receipts are\n" +
			"sent in batches with a second attempt for non-responders; media the phone no\n" +
			"longer holds is marked so it is not retried again. Only works while the phone\n" +
			"is online and still has the media.\n\n" +
			"--chat with --id retries exactly one message, even one already marked\n" +
			"unavailable or recorded as downloaded. --type limits the pending scan to media\n" +
			"types. While `sync --follow` runs for the same store, the retry runs in it.",
		Example: "  wacli media retry --type audio --limit 20\n" +
			"  wacli media retry --chat 15550000001@s.whatsapp.net --id ABC123",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := mediaRetryOptions(cmd, chat, id, mediaType, before, limit, batch, wait)
			if err != nil {
				return err
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			ctx, cancel := mediaBulkContext(cmd, flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				req := sendDelegateRequest{Kind: mediaRetryKind, Chat: opts.ChatJID, ID: opts.MsgID, Job: retryJobArgs(opts)}
				return delegateJobAfterOpenFailure(ctx, flags, err, req, bulkJobBudget(mediaBulkTimeoutEnabled(cmd, flags), flags), func(resp sendDelegateResponse) error {
					var res app.MediaRetryResult
					if err := decodeDelegateJobResult(resp, &res); err != nil {
						return err
					}
					return writeMediaRetryResult(flags, res)
				})
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}

			res, err := a.RetryMedia(ctx, opts)
			if err != nil {
				return err
			}
			return writeMediaRetryResult(flags, res)
		},
	}

	cmd.Flags().StringVar(&chat, "chat", "", "limit retry to a single chat JID")
	cmd.Flags().StringVar(&id, "id", "", "retry exactly this message (requires --chat)")
	cmd.Flags().StringVar(&mediaType, "type", "", "only retry these media types, comma-separated: image, video (includes gif), gif, audio, document, sticker")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum number of messages to retry (0 = all pending)")
	cmd.Flags().IntVar(&batch, "batch", 32, "number of retry receipts to send per batch")
	cmd.Flags().DurationVar(&wait, "wait", 30*time.Second, "how long to wait for the phone per attempt")
	cmd.Flags().StringVar(&before, "before", "", "only retry media older than this date (YYYY-MM-DD)")
	return cmd
}

// mediaRetryOptions validates the retry flags before the store is touched.
func mediaRetryOptions(cmd *cobra.Command, chat, id, mediaType, before string, limit, batch int, wait time.Duration) (app.RetryMediaOptions, error) {
	if limit < 0 {
		return app.RetryMediaOptions{}, fmt.Errorf("--limit must be >= 0")
	}
	if batch <= 0 {
		return app.RetryMediaOptions{}, fmt.Errorf("--batch must be > 0")
	}
	if wait <= 0 {
		return app.RetryMediaOptions{}, fmt.Errorf("--wait must be > 0")
	}
	beforeUnix, err := parseMediaRetryBefore(before)
	if err != nil {
		return app.RetryMediaOptions{}, err
	}
	opts := app.RetryMediaOptions{
		ChatJID:    strings.TrimSpace(chat),
		MsgID:      strings.TrimSpace(id),
		BeforeUnix: beforeUnix,
		BeforeSet:  strings.TrimSpace(before) != "",
		Limit:      limit,
		BatchSize:  batch,
		Wait:       wait,
	}
	if opts.MsgID != "" {
		if opts.ChatJID == "" {
			return app.RetryMediaOptions{}, fmt.Errorf("--id requires --chat")
		}
		for _, name := range []string{"type", "before", "limit"} {
			if flag := cmd.Flags().Lookup(name); flag != nil && flag.Changed {
				return app.RetryMediaOptions{}, fmt.Errorf("--%s cannot be combined with --id, which selects one message", name)
			}
		}
	}
	if flag := cmd.Flags().Lookup("type"); flag != nil && flag.Changed {
		types, err := store.MediaTypeFilter(mediaType)
		if err != nil {
			return app.RetryMediaOptions{}, fmt.Errorf("--type: %w", err)
		}
		opts.MediaTypes = types
	}
	return opts, nil
}

func parseMediaRetryBefore(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return 0, fmt.Errorf("--before must be YYYY-MM-DD: %w", err)
	}
	return parsed.Unix(), nil
}

func newMediaBackfillCmd(flags *rootFlags) *cobra.Command {
	var chat string
	var limit int
	var workers int

	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Download media for already-synced messages missing a local copy",
		Long: "Fetch media for messages already stored in the local database that have\n" +
			"downloadable metadata but no local file yet. Unlike `sync --download-media`,\n" +
			"which only downloads media for messages arriving during the sync, this scans\n" +
			"existing rows and downloads them over a single connection.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 0 {
				return fmt.Errorf("--limit must be >= 0")
			}
			if workers < 0 {
				return fmt.Errorf("--workers must be >= 0")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}

			ctx, cancel := mediaBulkContext(cmd, flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				req := sendDelegateRequest{Kind: mediaBackfillKind, Chat: chat, Job: &delegateJobArgs{Limit: limit, Workers: workers}}
				return delegateJobAfterOpenFailure(ctx, flags, err, req, bulkJobBudget(mediaBulkTimeoutEnabled(cmd, flags), flags), func(resp sendDelegateResponse) error {
					var res app.BackfillMediaResult
					if err := decodeDelegateJobResult(resp, &res); err != nil {
						return err
					}
					return writeMediaBackfillResult(flags, res)
				})
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}

			res, err := a.BackfillMedia(ctx, app.BackfillMediaOptions{
				ChatJID: chat,
				Limit:   limit,
				Workers: workers,
			})
			if err != nil {
				return err
			}
			return writeMediaBackfillResult(flags, res)
		},
	}

	cmd.Flags().StringVar(&chat, "chat", "", "limit backfill to a single chat JID")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum number of media files to download (0 = all)")
	cmd.Flags().IntVar(&workers, "workers", 4, "number of concurrent downloads")
	return cmd
}

func mediaBulkContext(cmd *cobra.Command, flags *rootFlags) (context.Context, context.CancelFunc) {
	ctx, stop := signalContextWithEvents(out.NewEventWriter(os.Stderr, flags.events))
	if !mediaBulkTimeoutEnabled(cmd, flags) {
		return ctx, stop
	}
	timedCtx, cancel := withTimeout(ctx, flags)
	return timedCtx, func() {
		cancel()
		stop()
	}
}

func mediaBulkTimeoutEnabled(cmd *cobra.Command, flags *rootFlags) bool {
	if cmd == nil || flags == nil || flags.timeout <= 0 {
		return false
	}
	if flag := cmd.Flags().Lookup("timeout"); flag != nil && flag.Changed {
		return true
	}
	flag := cmd.InheritedFlags().Lookup("timeout")
	return flag != nil && flag.Changed
}

func newMediaDownloadCmd(flags *rootFlags) *cobra.Command {
	var chat string
	var id string
	var outputPath string

	cmd := &cobra.Command{
		Use:   "download",
		Short: "Download media for a message",
		RunE: func(cmd *cobra.Command, args []string) error {
			if chat == "" || id == "" {
				return fmt.Errorf("--chat and --id are required")
			}
			readOnly := flags.isReadOnly()
			if readOnly {
				if strings.TrimSpace(outputPath) == "" {
					return fmt.Errorf("--output is required in read-only mode")
				}
			} else {
				if err := flags.requireWritable(); err != nil {
					return err
				}
			}

			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, !readOnly, false)
			if err != nil {
				// A `sync --follow` holds the lock for its whole run, so waiting
				// cannot clear it: hand the download to it, or name the flag
				// that needs no lock when it cannot take it.
				if !readOnly && lock.IsLocked(err) {
					return downloadThroughSync(ctx, flags, err, chat, id, outputPath)
				}
				return err
			}
			defer closeApp(a, lk)

			if !readOnly {
				if err := a.EnsureAuthed(ctx); err != nil {
					return err
				}
			}

			info, target, err := loadMediaDownload(a, chat, id, outputPath)
			if err != nil {
				return err
			}

			if readOnly {
				bytes, err := wa.DownloadMediaDirectToFile(ctx, info.DirectPath, info.FileEncSHA256, info.FileSHA256, info.MediaKey, info.FileLength, info.MediaType, target)
				if err != nil {
					return err
				}
				resp := map[string]any{
					"chat":       info.ChatJID,
					"id":         info.MsgID,
					"path":       target,
					"bytes":      bytes,
					"media_type": info.MediaType,
					"mime_type":  info.MimeType,
					"downloaded": true,
					"read_only":  true,
					"recorded":   false,
				}
				if flags.asJSON {
					return out.WriteJSON(os.Stdout, resp)
				}
				fmt.Fprintf(os.Stdout, "%s (%d bytes)\n", target, bytes)
				return nil
			}

			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}
			res, err := downloadMediaTo(ctx, a, info, target)
			if err != nil {
				return err
			}
			return writeMediaDownloadResult(flags, res)
		},
	}

	cmd.Flags().StringVar(&chat, "chat", "", "chat JID")
	cmd.Flags().StringVar(&id, "id", "", "message ID")
	cmd.Flags().StringVar(&outputPath, "output", "", "output file or directory (default: store media dir)")
	_ = cmd.MarkFlagRequired("chat")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}
