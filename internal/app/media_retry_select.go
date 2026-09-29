package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/openclaw/wacli/internal/store"
)

// selectRetryMedia picks the messages a retry run asks the phone for. Without
// MsgID or MediaTypes it is the pending scan media retry has always used.
func (a *App) selectRetryMedia(ctx context.Context, opts RetryMediaOptions) ([]store.PendingMediaDownload, error) {
	if msgID := strings.TrimSpace(opts.MsgID); msgID != "" {
		return a.retryTarget(opts.ChatJID, msgID)
	}
	var pending []store.PendingMediaDownload
	var err error
	switch {
	case len(opts.MediaTypes) > 0:
		pending, err = a.db.ListPendingMedia(ctx, store.PendingMediaFilter{
			ChatJID:    opts.ChatJID,
			MediaTypes: opts.MediaTypes,
			BeforeUnix: opts.BeforeUnix,
			BeforeSet:  opts.BeforeSet || opts.BeforeUnix != 0,
			Limit:      opts.Limit,
		})
	case opts.BeforeSet || opts.BeforeUnix != 0:
		pending, err = a.db.ListPendingMediaBefore(ctx, opts.ChatJID, opts.BeforeUnix, opts.Limit)
	default:
		pending, err = a.db.ListPendingMediaDownloads(ctx, opts.ChatJID, opts.Limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list pending media: %w", err)
	}
	return pending, nil
}

// retryTarget checks the one message a caller named. Unlike the pending scan
// it keeps media that already has a local path or was marked unavailable:
// naming a message asks for it again, for example after its file was removed
// or its CDN copy expired before it could be read.
func (a *App) retryTarget(chatJID, msgID string) ([]store.PendingMediaDownload, error) {
	chatJID = strings.TrimSpace(chatJID)
	if chatJID == "" {
		return nil, fmt.Errorf("--id requires --chat")
	}
	msg, err := a.db.GetMessage(chatJID, msgID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("message %s not found in chat %s", msgID, chatJID)
	}
	if err != nil {
		return nil, fmt.Errorf("load message: %w", err)
	}
	if msg.DeletedAt != nil {
		return nil, fmt.Errorf("message %s was deleted; its media cannot be retried", msgID)
	}
	info, err := a.db.GetMediaDownloadInfo(chatJID, msgID)
	if err != nil {
		return nil, fmt.Errorf("load media info: %w", err)
	}
	if strings.TrimSpace(info.MediaType) == "" || strings.TrimSpace(info.DirectPath) == "" || len(info.MediaKey) == 0 {
		return nil, fmt.Errorf("message %s has no downloadable media metadata (run `wacli sync` first)", msgID)
	}
	return []store.PendingMediaDownload{{ChatJID: info.ChatJID, MsgID: info.MsgID}}, nil
}
