package store

import (
	"strings"
	"time"
)

// AudioMessage is the minimal view of a stored audio message that
// `media transcribe --pending` needs to decide whether it can be transcribed.
type AudioMessage struct {
	ChatJID   string
	MsgID     string
	Timestamp time.Time
	// Downloadable is true when the row still has the CDN metadata a direct
	// download needs and the phone has not reported the media gone.
	Downloadable bool
}

type ListAudioMessagesParams struct {
	ChatJIDs []string
	After    *time.Time
	Before   *time.Time
}

// ListAudioMessages returns visible (not deleted or purged) audio messages,
// newest first. It only reads, so it works on a read-only store.
func (d *DB) ListAudioMessages(p ListAudioMessagesParams) ([]AudioMessage, error) {
	query := `
		SELECT m.chat_jid, m.msg_id, m.ts,
			CASE WHEN COALESCE(m.direct_path, '') != '' AND length(COALESCE(m.media_key, '')) > 0 AND m.media_unavailable_at IS NULL THEN 1 ELSE 0 END
		FROM messages m
		WHERE m.deleted_at IS NULL AND LOWER(COALESCE(m.media_type, '')) = 'audio'`
	var args []any
	query, args = appendStringFilter(query, args, "m.chat_jid", "", p.ChatJIDs)
	if p.After != nil {
		query += " AND m.ts > ?"
		args = append(args, unix(*p.After))
	}
	if p.Before != nil {
		query += " AND m.ts < ?"
		args = append(args, unix(*p.Before))
	}
	query += " ORDER BY m.ts DESC, m.rowid DESC"
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AudioMessage
	for rows.Next() {
		var m AudioMessage
		var ts int64
		var downloadable int
		if err := rows.Scan(&m.ChatJID, &m.MsgID, &ts, &downloadable); err != nil {
			return nil, err
		}
		m.Timestamp = fromUnix(ts)
		m.Downloadable = downloadable != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// SearchAudioMessagesByID returns visible audio messages whose ID is in
// msgIDs and that pass the same filters as SearchMessages (chat, sender,
// time, media, forwarded, starred, type). Query and Limit are ignored; rows
// come back newest first. `messages search` uses it to add messages whose
// transcript matched.
func (d *DB) SearchAudioMessagesByID(msgIDs []string, p SearchMessagesParams) ([]Message, error) {
	ids := uniqueNonEmptyStrings(msgIDs)
	var out []Message
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		query := `
			SELECT ` + messageSelectColumns("") + `
			FROM messages m
			LEFT JOIN chats c ON c.jid = m.chat_jid
			LEFT JOIN starred s ON s.chat_jid = m.chat_jid AND s.msg_id = m.msg_id
			WHERE m.deleted_at IS NULL AND LOWER(COALESCE(m.media_type, '')) = 'audio'
				AND m.msg_id IN (` + strings.TrimRight(strings.Repeat("?,", end-start), ",") + `)`
		args := make([]any, 0, end-start)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		query, args = applyMessageFilters(query, args, p)
		query += " ORDER BY m.ts DESC, m.rowid DESC"
		msgs, err := d.scanMessages(query, args...)
		if err != nil {
			return nil, err
		}
		out = append(out, msgs...)
	}
	return out, nil
}
