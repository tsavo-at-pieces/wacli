package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// StatusMute mirrors one userStatusMute app-state entry: whether this account
// hides a contact's status updates.
type StatusMute struct {
	// JID is the contact as wacli stores people: the phone JID when the
	// session knows it, else the JID WhatsApp used.
	JID string `json:"jid"`
	// IndexJID is the exact JID in WhatsApp's app-state index. Changing the
	// mute again must target this identity for other devices to agree.
	IndexJID  string    `json:"index_jid"`
	Muted     bool      `json:"muted"`
	UpdatedAt time.Time `json:"updated_at"`
}

type SetStatusMuteParams struct {
	JID       string
	IndexJID  string
	Muted     bool
	UpdatedAt time.Time
}

func migrateStatusMutes(d *DB) error {
	if _, err := d.sql.Exec(`
		CREATE TABLE IF NOT EXISTS status_mutes (
			jid TEXT PRIMARY KEY,
			index_jid TEXT NOT NULL,
			muted INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("create status_mutes table: %w", err)
	}
	return nil
}

// SetStatusMute records the latest status mute state for a contact. App-state
// mutations arrive in WhatsApp's patch order, so the last write wins.
func (d *DB) SetStatusMute(p SetStatusMuteParams) error {
	jid := strings.TrimSpace(p.JID)
	indexJID := strings.TrimSpace(p.IndexJID)
	if jid == "" {
		jid = indexJID
	}
	if indexJID == "" {
		indexJID = jid
	}
	if jid == "" {
		return fmt.Errorf("status mute JID is required")
	}
	updated := unix(p.UpdatedAt)
	if updated <= 0 {
		updated = nowUTC().Unix()
	}
	_, err := d.sql.Exec(`
		INSERT INTO status_mutes(jid, index_jid, muted, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET
			index_jid=excluded.index_jid,
			muted=excluded.muted,
			updated_at=excluded.updated_at
	`, jid, indexJID, boolToInt(p.Muted), updated)
	return err
}

// FindStatusMute returns the entry for the first of jids that matches either
// the stored contact JID or the app-state index JID.
func (d *DB) FindStatusMute(jids ...string) (StatusMute, bool, error) {
	values := uniqueNonEmptyStrings(jids)
	if len(values) == 0 {
		return StatusMute{}, false, nil
	}
	ok, err := d.tableExists("status_mutes")
	if err != nil || !ok {
		return StatusMute{}, false, err
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(values)), ",")
	args := make([]any, 0, 2*len(values))
	for _, v := range values {
		args = append(args, v)
	}
	for _, v := range values {
		args = append(args, v)
	}
	row := d.sql.QueryRow(`
		SELECT jid, index_jid, muted, updated_at FROM status_mutes
		WHERE jid IN (`+placeholders+`) OR index_jid IN (`+placeholders+`)
		ORDER BY updated_at DESC, rowid DESC
		LIMIT 1
	`, args...)
	m, err := scanStatusMute(row)
	if errors.Is(err, sql.ErrNoRows) {
		return StatusMute{}, false, nil
	}
	if err != nil {
		return StatusMute{}, false, err
	}
	return m, true, nil
}

// ListStatusMutes returns muted contacts, or every mirrored entry with all.
// A store that predates the table has no entries.
func (d *DB) ListStatusMutes(all bool) ([]StatusMute, error) {
	ok, err := d.tableExists("status_mutes")
	if err != nil || !ok {
		return nil, err
	}
	query := `SELECT jid, index_jid, muted, updated_at FROM status_mutes`
	if !all {
		query += ` WHERE muted != 0`
	}
	query += ` ORDER BY updated_at DESC, jid ASC`
	rows, err := d.sql.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatusMute
	for rows.Next() {
		m, err := scanStatusMute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MutedStatusJIDs returns every identity, stored or indexed, whose status
// updates are muted.
func (d *DB) MutedStatusJIDs() (map[string]bool, error) {
	mutes, err := d.ListStatusMutes(false)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, 2*len(mutes))
	for _, m := range mutes {
		out[m.JID] = true
		out[m.IndexJID] = true
	}
	return out, nil
}

type statusMuteScanner interface {
	Scan(dest ...any) error
}

func scanStatusMute(row statusMuteScanner) (StatusMute, error) {
	var m StatusMute
	var muted int
	var updated int64
	if err := row.Scan(&m.JID, &m.IndexJID, &muted, &updated); err != nil {
		return StatusMute{}, err
	}
	m.Muted = muted != 0
	m.UpdatedAt = fromUnix(updated)
	return m, nil
}
