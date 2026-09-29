package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Tombstone reasons for messages removed by a whole-chat delete or clear, on
// this account's devices only.
const (
	MessageDeletionReasonWhatsAppDeleteChat = "whatsapp-delete-chat"
	MessageDeletionReasonWhatsAppClearChat  = "whatsapp-clear-chat"
)

// migrateChatActionsAndLists adds chat lock/delete/clear state, WhatsApp lists
// (labels) and favorites, pinned messages, and app-state mirror markers.
func migrateChatActionsAndLists(d *DB) error {
	hasChats, err := d.tableExists("chats")
	if err != nil {
		return err
	}
	if hasChats {
		for _, col := range []struct{ name, ddl string }{
			{"locked", `ALTER TABLE chats ADD COLUMN locked INTEGER NOT NULL DEFAULT 0`},
			{"lock_jid", `ALTER TABLE chats ADD COLUMN lock_jid TEXT`},
			{"deleted_at", `ALTER TABLE chats ADD COLUMN deleted_at INTEGER`},
			{"cleared_at", `ALTER TABLE chats ADD COLUMN cleared_at INTEGER`},
		} {
			has, err := d.tableHasColumn("chats", col.name)
			if err != nil {
				return err
			}
			if has {
				continue
			}
			if _, err := d.sql.Exec(col.ddl); err != nil {
				return fmt.Errorf("add chats.%s column: %w", col.name, err)
			}
		}
	}
	if _, err := d.sql.Exec(`
		CREATE TABLE IF NOT EXISTS chat_lists (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			color INTEGER NOT NULL DEFAULT 0,
			predefined_id INTEGER,
			order_index INTEGER,
			is_active INTEGER,
			list_type INTEGER,
			is_immutable INTEGER,
			mute_end_ms INTEGER,
			deleted INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS chat_list_members (
			list_id TEXT NOT NULL,
			raw_jid TEXT NOT NULL,
			chat_jid TEXT NOT NULL,
			labeled INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (list_id, raw_jid)
		);
		CREATE INDEX IF NOT EXISTS idx_chat_list_members_chat ON chat_list_members(chat_jid);
		CREATE TABLE IF NOT EXISTS chat_favorites (
			raw_jid TEXT PRIMARY KEY,
			chat_jid TEXT NOT NULL,
			position INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS message_pins (
			chat_jid TEXT NOT NULL,
			msg_id TEXT NOT NULL,
			pinned INTEGER NOT NULL,
			pinned_by TEXT,
			changed_at INTEGER NOT NULL,
			expires_at INTEGER,
			PRIMARY KEY (chat_jid, msg_id)
		);
		CREATE TABLE IF NOT EXISTS app_state_mirrors (
			name TEXT PRIMARY KEY,
			synced_at INTEGER NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("create chat list, pin and mirror tables: %w", err)
	}
	return nil
}

// SetChatLocked records WhatsApp's chat lock. rawJID is the JID the lock
// mutation used, so a later unlock can overwrite the same app-state entry.
func (d *DB) SetChatLocked(jid, rawJID string, locked bool) error {
	jid = strings.TrimSpace(jid)
	if jid == "" {
		return fmt.Errorf("chat JID is required")
	}
	_, err := d.sql.Exec(`
		INSERT INTO chats(jid, kind, locked, lock_jid)
		VALUES(?, 'unknown', ?, ?)
		ON CONFLICT(jid) DO UPDATE SET locked = excluded.locked, lock_jid = COALESCE(excluded.lock_jid, chats.lock_jid)
	`, jid, boolToInt(locked), nullIfEmpty(rawJID))
	return err
}

// ChatLockJID returns the JID a stored lock was written with, if any.
func (d *DB) ChatLockJID(jid string) (string, error) {
	var raw sql.NullString
	err := d.sql.QueryRow(`SELECT lock_jid FROM chats WHERE jid = ?`, strings.TrimSpace(jid)).Scan(&raw)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return raw.String, nil
}

// MarkChatDeleted records a WhatsApp delete-chat for chatJIDs (one chat under
// its phone and LID JIDs). Messages up to through become tombstones: hidden
// from list, search and export, still available to `messages show`. The chat
// stays hidden from `chats list` until a message newer than through arrives.
func (d *DB) MarkChatDeleted(chatJIDs []string, through, deletedAt time.Time) (int64, error) {
	return d.tombstoneChat(chatJIDs, through, deletedAt, "deleted_at", MessageDeletionReasonWhatsAppDeleteChat, true)
}

// MarkChatCleared records a WhatsApp clear-chat: messages up to through become
// tombstones, starred ones only when deleteStarred is set. The chat stays listed.
func (d *DB) MarkChatCleared(chatJIDs []string, through, clearedAt time.Time, deleteStarred bool) (int64, error) {
	return d.tombstoneChat(chatJIDs, through, clearedAt, "cleared_at", MessageDeletionReasonWhatsAppClearChat, deleteStarred)
}

func (d *DB) tombstoneChat(chatJIDs []string, through, at time.Time, chatColumn, reason string, includeStarred bool) (int64, error) {
	jids := uniqueNonEmptyStrings(chatJIDs)
	if len(jids) == 0 {
		return 0, fmt.Errorf("chat JID is required")
	}
	if through.IsZero() {
		return 0, fmt.Errorf("chat boundary is required")
	}
	if at.IsZero() {
		at = nowUTC()
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(jids)), ",")
	args := make([]any, 0, len(jids)+4)
	args = append(args, unix(at), reason)
	for _, jid := range jids {
		args = append(args, jid)
	}
	args = append(args, unix(through))

	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	query := `
		UPDATE messages SET
			deleted_for_me = 1,
			deleted_at = COALESCE(deleted_at, ?),
			deletion_reason = CASE WHEN deleted_at IS NULL THEN ? ELSE deletion_reason END
		WHERE chat_jid IN (` + placeholders + `) AND ts <= ? AND deleted_at IS NULL`
	if !includeStarred {
		query += ` AND NOT EXISTS (SELECT 1 FROM starred s WHERE s.chat_jid = messages.chat_jid AND s.msg_id = messages.msg_id)`
	}
	res, err := tx.Exec(query, args...)
	if err != nil {
		return 0, fmt.Errorf("tombstone chat messages: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	chatArgs := make([]any, 0, len(jids)+1)
	chatArgs = append(chatArgs, unix(through))
	for _, jid := range jids {
		chatArgs = append(chatArgs, jid)
	}
	// chatColumn is one of two constants above, never user input.
	if _, err := tx.Exec(`
		UPDATE chats SET
			`+chatColumn+` = max(COALESCE(`+chatColumn+`, 0), ?),
			unread = 0,
			unread_count = 0
		WHERE jid IN (`+placeholders+`)
	`, chatArgs...); err != nil {
		return 0, fmt.Errorf("record chat %s: %w", strings.TrimSuffix(chatColumn, "_at"), err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// MessagePin is the pin-in-chat state of one message.
type MessagePin struct {
	ChatJID   string     `json:"chat_jid"`
	MsgID     string     `json:"msg_id"`
	Pinned    bool       `json:"pinned"`
	PinnedBy  string     `json:"pinned_by,omitempty"`
	ChangedAt time.Time  `json:"changed_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// SetMessagePin records a pin or unpin. PinnedBy is empty for your own pins.
func (d *DB) SetMessagePin(p MessagePin) error {
	chatJID := strings.TrimSpace(p.ChatJID)
	msgID := strings.TrimSpace(p.MsgID)
	if chatJID == "" || msgID == "" {
		return fmt.Errorf("chat JID and message ID are required")
	}
	if p.ChangedAt.IsZero() {
		p.ChangedAt = nowUTC()
	}
	var expires sql.NullInt64
	if p.Pinned && p.ExpiresAt != nil && !p.ExpiresAt.IsZero() {
		expires = sql.NullInt64{Int64: unix(*p.ExpiresAt), Valid: true}
	}
	_, err := d.sql.Exec(`
		INSERT INTO message_pins(chat_jid, msg_id, pinned, pinned_by, changed_at, expires_at)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_jid, msg_id) DO UPDATE SET
			pinned = excluded.pinned,
			pinned_by = excluded.pinned_by,
			changed_at = excluded.changed_at,
			expires_at = excluded.expires_at
		WHERE excluded.changed_at >= message_pins.changed_at
	`, chatJID, msgID, boolToInt(p.Pinned), nullIfEmpty(p.PinnedBy), unix(p.ChangedAt), expires)
	return err
}

// ListPinnedMessages returns messages pinned at now, newest pin first. A pin
// without a known expiry stays listed until it is unpinned.
func (d *DB) ListPinnedMessages(chatJIDs []string, now time.Time) ([]MessagePin, error) {
	query := `SELECT chat_jid, msg_id, pinned, COALESCE(pinned_by,''), changed_at, COALESCE(expires_at,0)
		FROM message_pins WHERE pinned = 1 AND (expires_at IS NULL OR expires_at > ?)`
	args := []any{unix(now)}
	query, args = appendStringFilter(query, args, "chat_jid", "", chatJIDs)
	query += ` ORDER BY changed_at DESC, chat_jid, msg_id`
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessagePin
	for rows.Next() {
		var p MessagePin
		var pinned int
		var changed, expires int64
		if err := rows.Scan(&p.ChatJID, &p.MsgID, &pinned, &p.PinnedBy, &changed, &expires); err != nil {
			return nil, err
		}
		p.Pinned = pinned != 0
		p.ChangedAt = fromUnix(changed)
		p.ExpiresAt = timePointerFromUnix(expires)
		out = append(out, p)
	}
	return out, rows.Err()
}

// chatExtras fills the lock and delete/clear state added after the sqlc
// chat queries.
func (d *DB) chatExtras(c *Chat) error {
	var locked int
	var deletedAt, clearedAt int64
	err := d.sql.QueryRow(`SELECT COALESCE(locked,0), COALESCE(deleted_at,0), COALESCE(cleared_at,0) FROM chats WHERE jid = ?`, c.JID).
		Scan(&locked, &deletedAt, &clearedAt)
	if err != nil {
		return err
	}
	applyChatExtras(c, locked, deletedAt, clearedAt)
	return nil
}

func applyChatExtras(c *Chat, locked int, deletedAt, clearedAt int64) {
	c.Locked = locked != 0
	c.DeletedAt = timePointerFromUnix(deletedAt)
	c.ClearedAt = timePointerFromUnix(clearedAt)
	// A deleted chat reappears once a newer message arrives.
	if c.DeletedAt != nil && c.LastMessageTS.After(*c.DeletedAt) {
		c.DeletedAt = nil
	}
}

// appendChatActionFilters applies the lock, deletion and membership filters.
// Deleted chats are hidden unless asked for: a chat is deleted while no
// message newer than its delete boundary has arrived.
func appendChatActionFilters(q string, args []any, f ChatListFilter) (string, []any) {
	const deleted = `(deleted_at IS NOT NULL AND COALESCE(last_message_ts,0) <= deleted_at)`
	if f.Deleted != nil && *f.Deleted {
		q += ` AND ` + deleted
	} else {
		q += ` AND NOT ` + deleted
	}
	if f.Locked != nil {
		q += ` AND COALESCE(locked,0) = ?`
		args = append(args, boolToInt(*f.Locked))
	}
	if f.JIDs != nil {
		jids := uniqueNonEmptyStrings(f.JIDs)
		if len(jids) == 0 {
			return q + ` AND 0`, args
		}
		q += ` AND jid IN (` + strings.TrimRight(strings.Repeat("?,", len(jids)), ",") + `)`
		for _, jid := range jids {
			args = append(args, jid)
		}
	}
	return q, args
}
