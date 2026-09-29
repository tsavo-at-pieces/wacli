package store

import (
	"fmt"
	"strings"
	"time"
)

// SetContactBookName records the name a WhatsApp contact is saved under. The
// row for jid is created if needed; rows for the other identities of the same
// person are only updated when they already exist. Unlike UpsertContact, the
// saved names are replaced exactly, including with empty values.
func (d *DB) SetContactBookName(jid, phone, firstName, fullName string, others ...string) error {
	jid = strings.TrimSpace(jid)
	if jid == "" {
		return fmt.Errorf("jid is required")
	}
	now := nowUTC().Unix()
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		INSERT INTO contacts(jid, phone, first_name, full_name, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET
			phone=COALESCE(NULLIF(excluded.phone,''), contacts.phone),
			first_name=excluded.first_name,
			full_name=excluded.full_name,
			updated_at=excluded.updated_at`,
		jid, nullString(phone), nullString(firstName), nullString(fullName), now); err != nil {
		return err
	}
	for _, other := range others {
		if other = strings.TrimSpace(other); other == "" || other == jid {
			continue
		}
		if _, err := tx.Exec(`UPDATE contacts SET first_name = ?, full_name = ?, updated_at = ? WHERE jid = ?`,
			nullString(firstName), nullString(fullName), now, other); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ClearContactBookName forgets the saved WhatsApp contact names on existing
// rows, keeping push names, business names and local metadata.
func (d *DB) ClearContactBookName(jids ...string) error {
	now := nowUTC().Unix()
	for _, jid := range jids {
		if _, err := d.sql.Exec(`UPDATE contacts SET first_name = NULL, full_name = NULL, updated_at = ? WHERE jid = ?`, now, jid); err != nil {
			return err
		}
	}
	return nil
}

func migrateContactBlocks(d *DB) error {
	if _, err := d.sql.Exec(`
		CREATE TABLE IF NOT EXISTS contact_blocks (
			jid TEXT PRIMARY KEY,
			blocked_at INTEGER NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("create contact_blocks table: %w", err)
	}
	return nil
}

// SetContactsBlocked marks identities as blocked or unblocked on WhatsApp, as
// last observed by wacli.
func (d *DB) SetContactsBlocked(jids []string, blocked bool, at time.Time) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, jid := range jids {
		if jid = strings.TrimSpace(jid); jid == "" {
			continue
		}
		if blocked {
			_, err = tx.Exec(`INSERT INTO contact_blocks(jid, blocked_at) VALUES (?, ?) ON CONFLICT(jid) DO NOTHING`, jid, unix(at))
		} else {
			_, err = tx.Exec(`DELETE FROM contact_blocks WHERE jid = ?`, jid)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplaceContactBlocks replaces the local block list with a full list fetched
// from WhatsApp.
func (d *DB) ReplaceContactBlocks(jids []string, at time.Time) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	del := `DELETE FROM contact_blocks`
	if len(jids) > 0 {
		del += ` WHERE jid NOT IN (` + contactBlockPlaceholders(len(jids)) + `)`
	}
	if _, err := tx.Exec(del, contactBlockArgs(jids)...); err != nil {
		return err
	}
	for _, jid := range jids {
		if _, err := tx.Exec(`INSERT INTO contact_blocks(jid, blocked_at) VALUES (?, ?) ON CONFLICT(jid) DO NOTHING`, jid, unix(at)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AnyContactBlocked reports whether any of a person's identities is on the
// local copy of the block list. A store opened read-only before the table
// existed reports false.
func (d *DB) AnyContactBlocked(jids []string) (bool, error) {
	exists, err := d.tableExists("contact_blocks")
	if err != nil || !exists {
		return false, err
	}
	if len(jids) == 0 {
		return false, nil
	}
	var found int
	err = d.sql.QueryRow(`SELECT COUNT(*) FROM contact_blocks WHERE jid IN (`+contactBlockPlaceholders(len(jids))+`)`, contactBlockArgs(jids)...).Scan(&found)
	return found > 0, err
}

func contactBlockPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func contactBlockArgs(values []string) []any {
	args := make([]any, len(values))
	for i, v := range values {
		args[i] = v
	}
	return args
}
