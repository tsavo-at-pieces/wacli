package store

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ChatList is one WhatsApp list (a label at the protocol level), as last seen
// in app state. Optional fields stay nil when WhatsApp left them unset so a
// rewrite can send back exactly what the phone stored.
type ChatList struct {
	ID           string
	Name         string
	Color        int32
	PredefinedID *int32
	OrderIndex   *int32
	IsActive     *bool
	ListType     *int32
	IsImmutable  *bool
	MuteEndMS    *int64
	Deleted      bool
	UpdatedAt    time.Time
}

// ChatListMember is one chat's latest membership state in a list. RawJID is
// the JID in the app-state index; ChatJID is the local chat it belongs to.
type ChatListMember struct {
	ListID    string
	ChatJID   string
	RawJID    string
	Labeled   bool
	UpdatedAt time.Time
}

// FavoriteChat is one entry of the favorites list, in the phone's order.
type FavoriteChat struct {
	ChatJID string
	RawJID  string
}

// Mirror marker names. A collection name records that a full app-state sync
// of it has been stored; favoritesMirror records that the complete favorites
// list has been stored at least once.
const favoritesMirror = "favorites"

// UpsertChatList stores a list definition. The latest app-state value wins.
func (d *DB) UpsertChatList(l ChatList) error {
	id := strings.TrimSpace(l.ID)
	if id == "" {
		return fmt.Errorf("list ID is required")
	}
	if l.UpdatedAt.IsZero() {
		l.UpdatedAt = nowUTC()
	}
	_, err := d.sql.Exec(`
		INSERT INTO chat_lists(id, name, color, predefined_id, order_index, is_active, list_type, is_immutable, mute_end_ms, deleted, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			color = excluded.color,
			predefined_id = excluded.predefined_id,
			order_index = excluded.order_index,
			is_active = excluded.is_active,
			list_type = excluded.list_type,
			is_immutable = excluded.is_immutable,
			mute_end_ms = excluded.mute_end_ms,
			deleted = excluded.deleted,
			updated_at = excluded.updated_at
	`, id, l.Name, l.Color, nullInt32(l.PredefinedID), nullInt32(l.OrderIndex), nullBool(l.IsActive),
		nullInt32(l.ListType), nullBool(l.IsImmutable), nullInt64(l.MuteEndMS), boolToInt(l.Deleted), unix(l.UpdatedAt))
	return err
}

// ListChatLists returns stored lists ordered as the phone orders them.
func (d *DB) ListChatLists(includeDeleted bool) ([]ChatList, error) {
	query := `SELECT id, name, color, predefined_id, order_index, is_active, list_type, is_immutable, mute_end_ms, deleted, updated_at FROM chat_lists`
	if !includeDeleted {
		query += ` WHERE deleted = 0`
	}
	query += ` ORDER BY order_index IS NULL, order_index, CAST(id AS INTEGER), id`
	rows, err := d.sql.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatList
	for rows.Next() {
		l, err := scanChatList(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetChatList returns one stored list, deleted or not.
func (d *DB) GetChatList(id string) (ChatList, error) {
	row := d.sql.QueryRow(`SELECT id, name, color, predefined_id, order_index, is_active, list_type, is_immutable, mute_end_ms, deleted, updated_at FROM chat_lists WHERE id = ?`, strings.TrimSpace(id))
	return scanChatList(row)
}

// NextChatListID returns a numeric list ID no stored list has used, deleted
// lists included, so a new list never overwrites an old definition.
func (d *DB) NextChatListID() (string, error) {
	rows, err := d.sql.Query(`SELECT id FROM chat_lists`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	highest := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		if n, err := strconv.Atoi(strings.TrimSpace(id)); err == nil && n > highest {
			highest = n
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return strconv.Itoa(highest + 1), nil
}

// SetChatListMember stores the latest labeled state of one list index entry.
func (d *DB) SetChatListMember(m ChatListMember) error {
	listID := strings.TrimSpace(m.ListID)
	raw := strings.TrimSpace(m.RawJID)
	chat := strings.TrimSpace(m.ChatJID)
	if listID == "" || raw == "" {
		return fmt.Errorf("list ID and chat JID are required")
	}
	if chat == "" {
		chat = raw
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = nowUTC()
	}
	_, err := d.sql.Exec(`
		INSERT INTO chat_list_members(list_id, raw_jid, chat_jid, labeled, updated_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(list_id, raw_jid) DO UPDATE SET
			chat_jid = excluded.chat_jid,
			labeled = excluded.labeled,
			updated_at = excluded.updated_at
	`, listID, raw, chat, boolToInt(m.Labeled), unix(m.UpdatedAt))
	return err
}

// ChatListMembers returns a list's index entries. labeledOnly drops removed ones.
func (d *DB) ChatListMembers(listID string, labeledOnly bool) ([]ChatListMember, error) {
	query := `SELECT list_id, chat_jid, raw_jid, labeled, updated_at FROM chat_list_members WHERE list_id = ?`
	if labeledOnly {
		query += ` AND labeled = 1`
	}
	query += ` ORDER BY chat_jid, raw_jid`
	rows, err := d.sql.Query(query, strings.TrimSpace(listID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatListMember
	for rows.Next() {
		var m ChatListMember
		var labeled int
		var updated int64
		if err := rows.Scan(&m.ListID, &m.ChatJID, &m.RawJID, &labeled, &updated); err != nil {
			return nil, err
		}
		m.Labeled = labeled != 0
		m.UpdatedAt = fromUnix(updated)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ReplaceFavorites stores the complete favorites list, in order. Favorites
// travel as one app-state value, so each one replaces the previous list.
func (d *DB) ReplaceFavorites(favorites []FavoriteChat, at time.Time) error {
	if at.IsZero() {
		at = nowUTC()
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM chat_favorites`); err != nil {
		return err
	}
	position := 0
	for _, f := range favorites {
		raw := strings.TrimSpace(f.RawJID)
		if raw == "" {
			continue
		}
		chat := strings.TrimSpace(f.ChatJID)
		if chat == "" {
			chat = raw
		}
		if _, err := tx.Exec(`INSERT INTO chat_favorites(raw_jid, chat_jid, position) VALUES(?, ?, ?) ON CONFLICT(raw_jid) DO NOTHING`, raw, chat, position); err != nil {
			return err
		}
		position++
	}
	if err := markAppStateMirrored(tx, favoritesMirror, at); err != nil {
		return err
	}
	return tx.Commit()
}

// ListFavorites returns the stored favorites list in the phone's order.
func (d *DB) ListFavorites() ([]FavoriteChat, error) {
	rows, err := d.sql.Query(`SELECT chat_jid, raw_jid FROM chat_favorites ORDER BY position, raw_jid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FavoriteChat
	for rows.Next() {
		var f FavoriteChat
		if err := rows.Scan(&f.ChatJID, &f.RawJID); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FavoritesKnown reports whether the stored favorites list is complete: a
// favorites value or a full regular_high sync has been stored.
func (d *DB) FavoritesKnown() (bool, error) {
	var n int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM app_state_mirrors WHERE name IN (?, 'regular_high')`, favoritesMirror).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkAppStateMirrored records that a full sync of collection was stored.
func (d *DB) MarkAppStateMirrored(collection string, at time.Time) error {
	collection = strings.TrimSpace(collection)
	if collection == "" {
		return fmt.Errorf("app state collection is required")
	}
	if at.IsZero() {
		at = nowUTC()
	}
	return markAppStateMirrored(d.sql, collection, at)
}

// AppStateMirrored reports whether a full sync of collection has been stored.
func (d *DB) AppStateMirrored(collection string) (bool, error) {
	var n int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM app_state_mirrors WHERE name = ?`, strings.TrimSpace(collection)).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

type sqlExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func markAppStateMirrored(db sqlExecer, name string, at time.Time) error {
	_, err := db.Exec(`
		INSERT INTO app_state_mirrors(name, synced_at) VALUES(?, ?)
		ON CONFLICT(name) DO UPDATE SET synced_at = excluded.synced_at
	`, name, unix(at))
	return err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanChatList(row rowScanner) (ChatList, error) {
	var l ChatList
	var predefined, order, active, listType, immutable, muteEnd sql.NullInt64
	var deleted int
	var updated int64
	if err := row.Scan(&l.ID, &l.Name, &l.Color, &predefined, &order, &active, &listType, &immutable, &muteEnd, &deleted, &updated); err != nil {
		return ChatList{}, err
	}
	l.PredefinedID = int32Ptr(predefined)
	l.OrderIndex = int32Ptr(order)
	l.IsActive = boolPtr(active)
	l.ListType = int32Ptr(listType)
	l.IsImmutable = boolPtr(immutable)
	if muteEnd.Valid {
		v := muteEnd.Int64
		l.MuteEndMS = &v
	}
	l.Deleted = deleted != 0
	l.UpdatedAt = fromUnix(updated)
	return l, nil
}

func nullInt32(v *int32) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*v), Valid: true}
}

func nullInt64(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

func nullBool(v *bool) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(boolToInt(*v)), Valid: true}
}

func int32Ptr(v sql.NullInt64) *int32 {
	if !v.Valid {
		return nil
	}
	n := int32(v.Int64)
	return &n
}

func boolPtr(v sql.NullInt64) *bool {
	if !v.Valid {
		return nil
	}
	b := v.Int64 != 0
	return &b
}

// FindChatList finds a list by ID, or by name ignoring case, surrounding
// space and the direction marks WhatsApp puts around localized list names.
func FindChatList(lists []ChatList, ref string) (ChatList, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ChatList{}, false
	}
	for _, l := range lists {
		if l.ID == ref {
			return l, true
		}
	}
	want := normalizeChatListName(ref)
	for _, l := range lists {
		if normalizeChatListName(l.Name) == want {
			return l, true
		}
	}
	return ChatList{}, false
}

func normalizeChatListName(name string) string {
	return strings.ToLower(strings.TrimSpace(strings.Trim(name, "‎‏‪‫‬⁦⁧⁨⁩ ")))
}
