// Package transcripts stores speech-to-text results for audio messages in a
// sidecar SQLite database (transcripts.db) next to wacli.db.
//
// It is deliberately separate from wacli.db: `wacli media transcribe` runs
// while `sync --follow` holds the store lock and must never write the
// message store, and a transcript is machine output that must never be
// mixed into the human-typed text columns.
package transcripts

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/openclaw/wacli/internal/fsutil"
	"github.com/openclaw/wacli/internal/sqliteutil"
)

// FileName is the sidecar database name inside the store directory.
const FileName = "transcripts.db"

const schemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS transcripts (
	chat_jid TEXT NOT NULL,
	msg_id TEXT NOT NULL,
	text TEXT NOT NULL,
	engine TEXT NOT NULL,
	model TEXT,
	duration_ms INTEGER,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (chat_jid, msg_id)
);
CREATE INDEX IF NOT EXISTS idx_transcripts_msg_id ON transcripts(msg_id);
CREATE TABLE IF NOT EXISTS transcript_failures (
	chat_jid TEXT NOT NULL,
	msg_id TEXT NOT NULL,
	error TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 1,
	last_attempt_at INTEGER NOT NULL,
	PRIMARY KEY (chat_jid, msg_id)
);
`

// sqlite's default host-parameter limit is far above this; chunking keeps
// IN (...) lists bounded for very large archives.
const idChunkSize = 500

// Transcript is one stored speech-to-text result. ChatJID is the canonical
// (phone-number) chat identity the message store uses.
type Transcript struct {
	ChatJID    string
	MsgID      string
	Text       string
	Engine     string
	Model      string
	DurationMS int64
	CreatedAt  time.Time
}

// Key identifies one message.
type Key struct {
	ChatJID string
	MsgID   string
}

// Failure records the last failed attempt for a message, so bulk runs can
// retry never-attempted messages first.
type Failure struct {
	Error         string
	Attempts      int
	LastAttemptAt time.Time
}

// DB is an open transcripts database.
type DB struct {
	sql *sql.DB
}

// PathFor returns the sidecar path for a store directory.
func PathFor(storeDir string) string {
	return filepath.Join(storeDir, FileName)
}

// Open opens path for reading and writing, creating it (mode 0600, WAL) and
// its schema when missing.
func Open(path string) (*DB, error) {
	if err := validatePath(path); err != nil {
		return nil, err
	}
	if err := fsutil.EnsurePrivateFile(path); err != nil {
		return nil, fmt.Errorf("create %s: %w", FileName, err)
	}
	db, err := sql.Open("sqlite3", sqliteutil.FileURI(path, "_busy_timeout=5000&_txlock=immediate"))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", FileName, err)
	}
	d := &DB{sql: db}
	if err := d.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := sqliteutil.ChmodFiles(path, 0o600); err != nil {
		_ = db.Close()
		return nil, err
	}
	return d, nil
}

// OpenReadOnly opens an existing sidecar without creating or migrating it.
// When the file (or its schema) does not exist the error wraps
// fs.ErrNotExist; callers treat that as "no transcripts yet".
func OpenReadOnly(path string) (*DB, error) {
	if err := validatePath(path); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	params := "_busy_timeout=5000&mode=ro&_query_only=1"
	if !sidecarFilesExist(path) {
		params += "&immutable=1"
	}
	db, err := sql.Open("sqlite3", sqliteutil.FileURI(path, params))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", FileName, err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'transcripts'`).Scan(&n); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open read-only %s: %w", FileName, err)
	}
	if n == 0 {
		_ = db.Close()
		return nil, fmt.Errorf("%s has no transcripts table: %w", FileName, fs.ErrNotExist)
	}
	return &DB{sql: db}, nil
}

func validatePath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("transcripts db path is required")
	}
	// Same guard as wacli.db: '?' or '#' would inject SQLite URI parameters.
	if strings.ContainsAny(path, "?#") {
		return fmt.Errorf("transcripts db path must not contain '?' or '#'")
	}
	return nil
}

func sidecarFilesExist(path string) bool {
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			return true
		}
	}
	return false
}

func (d *DB) init() error {
	if _, err := d.sql.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		return fmt.Errorf("set %s journal mode: %w", FileName, err)
	}
	_, _ = d.sql.Exec(`PRAGMA synchronous=NORMAL`)
	var version int
	if err := d.sql.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read %s version: %w", FileName, err)
	}
	if version > schemaVersion {
		return fmt.Errorf("%s schema version %d is newer than this wacli supports (%d)", FileName, version, schemaVersion)
	}
	if _, err := d.sql.Exec(schema); err != nil {
		return fmt.Errorf("create %s schema: %w", FileName, err)
	}
	if version < schemaVersion {
		if _, err := d.sql.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
			return fmt.Errorf("set %s version: %w", FileName, err)
		}
	}
	return nil
}

// Close closes the database.
func (d *DB) Close() error {
	if d == nil || d.sql == nil {
		return nil
	}
	return d.sql.Close()
}

// Upsert stores t, replacing any earlier transcript for the same message, and
// clears a recorded failure for it.
func (d *DB) Upsert(t Transcript) error {
	t.ChatJID = strings.TrimSpace(t.ChatJID)
	t.MsgID = strings.TrimSpace(t.MsgID)
	if t.ChatJID == "" || t.MsgID == "" {
		return fmt.Errorf("chat JID and message ID are required")
	}
	if strings.TrimSpace(t.Engine) == "" {
		return fmt.Errorf("transcript engine is required")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		INSERT INTO transcripts(chat_jid, msg_id, text, engine, model, duration_ms, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_jid, msg_id) DO UPDATE SET
			text = excluded.text,
			engine = excluded.engine,
			model = excluded.model,
			duration_ms = excluded.duration_ms,
			created_at = excluded.created_at
	`, t.ChatJID, t.MsgID, t.Text, t.Engine, nullString(t.Model), nullPositive(t.DurationMS), t.CreatedAt.Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM transcript_failures WHERE chat_jid = ? AND msg_id = ?`, t.ChatJID, t.MsgID); err != nil {
		return err
	}
	return tx.Commit()
}

// ByMessageIDs returns every transcript whose message ID is in ids. Callers
// match chat identity themselves, because a message may be stored under a
// phone-number or LID chat JID.
func (d *DB) ByMessageIDs(ids []string) ([]Transcript, error) {
	var out []Transcript
	for _, chunk := range chunkStrings(uniqueNonEmpty(ids), idChunkSize) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := d.sql.Query(`SELECT `+transcriptColumns+` FROM transcripts WHERE msg_id IN (`+placeholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, err
		}
		list, err := scanTranscripts(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, list...)
	}
	return out, nil
}

// Search returns transcripts containing every whitespace-separated word of
// query, compared case-insensitively (ASCII case folding, like the store's
// LIKE fallback). Wildcards in query are literal.
func (d *DB) Search(query string) ([]Transcript, error) {
	words := searchWords(query)
	if len(words) == 0 {
		return nil, nil
	}
	sqlText := `SELECT ` + transcriptColumns + ` FROM transcripts WHERE 1 = 1`
	args := make([]any, 0, len(words))
	for _, w := range words {
		sqlText += ` AND text LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLIKE(w)+"%")
	}
	sqlText += ` ORDER BY created_at DESC`
	rows, err := d.sql.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	return scanTranscripts(rows)
}

// Keys returns every transcribed message.
func (d *DB) Keys() ([]Key, error) {
	rows, err := d.sql.Query(`SELECT chat_jid, msg_id FROM transcripts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.ChatJID, &k.MsgID); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RecordFailure notes a failed attempt for one message.
func (d *DB) RecordFailure(chatJID, msgID, errText string, at time.Time) error {
	chatJID = strings.TrimSpace(chatJID)
	msgID = strings.TrimSpace(msgID)
	if chatJID == "" || msgID == "" {
		return fmt.Errorf("chat JID and message ID are required")
	}
	if at.IsZero() {
		at = time.Now()
	}
	_, err := d.sql.Exec(`
		INSERT INTO transcript_failures(chat_jid, msg_id, error, attempts, last_attempt_at)
		VALUES(?, ?, ?, 1, ?)
		ON CONFLICT(chat_jid, msg_id) DO UPDATE SET
			error = excluded.error,
			attempts = transcript_failures.attempts + 1,
			last_attempt_at = excluded.last_attempt_at
	`, chatJID, msgID, errText, at.Unix())
	return err
}

// Failures returns recorded failures keyed by message.
func (d *DB) Failures() (map[Key]Failure, error) {
	rows, err := d.sql.Query(`SELECT chat_jid, msg_id, error, attempts, last_attempt_at FROM transcript_failures`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[Key]Failure{}
	for rows.Next() {
		var k Key
		var f Failure
		var at int64
		if err := rows.Scan(&k.ChatJID, &k.MsgID, &f.Error, &f.Attempts, &at); err != nil {
			return nil, err
		}
		f.LastAttemptAt = time.Unix(at, 0).UTC()
		out[k] = f
	}
	return out, rows.Err()
}

// IsNotExist reports whether err means the sidecar has not been created yet.
func IsNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}

const transcriptColumns = `chat_jid, msg_id, text, engine, COALESCE(model, ''), COALESCE(duration_ms, 0), created_at`

func scanTranscripts(rows *sql.Rows) ([]Transcript, error) {
	defer rows.Close()
	var out []Transcript
	for rows.Next() {
		var t Transcript
		var created int64
		if err := rows.Scan(&t.ChatJID, &t.MsgID, &t.Text, &t.Engine, &t.Model, &t.DurationMS, &created); err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

func searchWords(query string) []string {
	var words []string
	for _, w := range strings.Fields(query) {
		if w = strings.Trim(w, `"`); w != "" {
			words = append(words, w)
		}
	}
	return words
}

func escapeLIKE(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func chunkStrings(values []string, size int) [][]string {
	var out [][]string
	for len(values) > 0 {
		n := size
		if len(values) < n {
			n = len(values)
		}
		out = append(out, values[:n])
		values = values[n:]
	}
	return out
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func nullString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func nullPositive(n int64) any {
	if n <= 0 {
		return nil
	}
	return n
}
