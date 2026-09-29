package store

import (
	"context"
	"fmt"
	"strings"
)

// PendingMediaFilter selects stored media that has downloadable metadata but
// no local copy, as ListPendingMediaDownloads does, with optional filters.
type PendingMediaFilter struct {
	// ChatJID, when non-empty, scopes to one chat.
	ChatJID string
	// MediaTypes, when non-empty, keeps only these stored media types
	// (image, video, gif, audio, document, sticker). Use MediaTypeFilter to
	// expand user input.
	MediaTypes []string
	// BeforeUnix keeps only messages older than this (seconds) when BeforeSet.
	BeforeUnix int64
	BeforeSet  bool
	// Limit caps the rows returned (<= 0 means no limit).
	Limit int
}

// retryableMediaTypes are the stored media types that can be downloaded.
var retryableMediaTypes = []string{"image", "video", "gif", "audio", "document", "sticker"}

// MediaTypeFilter turns a comma-separated list of media types into the stored
// types it selects. "video" includes GIFs, which WhatsApp sends as looping
// video; "gif" selects only them.
func MediaTypeFilter(list string) ([]string, error) {
	var types []string
	seen := map[string]bool{}
	add := func(t string) {
		if !seen[t] {
			seen[t] = true
			types = append(types, t)
		}
	}
	for _, raw := range strings.Split(list, ",") {
		t := strings.ToLower(strings.TrimSpace(raw))
		if t == "" {
			continue
		}
		known := false
		for _, k := range retryableMediaTypes {
			if t == k {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("unknown media type %q (want %s)", raw, strings.Join(retryableMediaTypes, ", "))
		}
		add(t)
		if t == "video" {
			add("gif")
		}
	}
	if len(types) == 0 {
		return nil, fmt.Errorf("no media type given (want %s)", strings.Join(retryableMediaTypes, ", "))
	}
	return types, nil
}

// ListPendingMedia returns pending media matching f, newest first, with the
// same pending conditions and order as ListPendingMediaDownloads.
func (d *DB) ListPendingMedia(ctx context.Context, f PendingMediaFilter) ([]PendingMediaDownload, error) {
	query := `SELECT m.chat_jid, m.msg_id
FROM messages m
WHERE COALESCE(m.media_type,'') != ''
  AND COALESCE(m.direct_path,'') != ''
  AND m.media_key IS NOT NULL AND length(m.media_key) > 0
  AND COALESCE(m.local_path,'') = ''
  AND m.deleted_at IS NULL
  AND m.media_unavailable_at IS NULL`
	var args []any
	if chat := strings.TrimSpace(f.ChatJID); chat != "" {
		query += "\n  AND m.chat_jid = ?"
		args = append(args, chat)
	}
	if f.BeforeSet {
		query += "\n  AND m.ts < ?"
		args = append(args, f.BeforeUnix)
	}
	if len(f.MediaTypes) > 0 {
		query += "\n  AND lower(m.media_type) IN (?" + strings.Repeat(",?", len(f.MediaTypes)-1) + ")"
		for _, t := range f.MediaTypes {
			args = append(args, strings.ToLower(strings.TrimSpace(t)))
		}
	}
	query += "\nORDER BY m.ts DESC, m.rowid DESC"
	if f.Limit > 0 {
		query += "\nLIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []PendingMediaDownload
	for rows.Next() {
		var p PendingMediaDownload
		if err := rows.Scan(&p.ChatJID, &p.MsgID); err != nil {
			return nil, err
		}
		pending = append(pending, p)
	}
	return pending, rows.Err()
}
