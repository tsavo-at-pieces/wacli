package store

import (
	"database/sql"
	"strings"
	"time"
)

type StatusMessage struct {
	RowID           int64
	MsgID           string
	Timestamp       time.Time
	FromMe          bool
	SenderJID       string
	SenderName      string
	Text            string
	MediaType       string
	MediaCaption    string
	Filename        string
	MimeType        string
	DirectPath      string
	MediaKey        []byte
	FileSHA256      []byte
	FileEncSHA256   []byte
	FileLength      uint64
	BackgroundColor string
	Font            int32
}

type UpsertStatusMessageParams struct {
	MsgID           string
	Timestamp       time.Time
	FromMe          bool
	SenderJID       string
	SenderName      string
	Text            string
	MediaType       string
	MediaCaption    string
	Filename        string
	MimeType        string
	DirectPath      string
	MediaKey        []byte
	FileSHA256      []byte
	FileEncSHA256   []byte
	FileLength      uint64
	BackgroundColor string
	Font            int32
}

func (d *DB) UpsertStatusMessage(p UpsertStatusMessageParams) error {
	_, err := d.sql.Exec(`
		INSERT INTO status_messages(
			msg_id, ts, from_me, sender_jid, sender_name, text,
			media_type, media_caption, filename, mime_type, direct_path,
			media_key, file_sha256, file_enc_sha256, file_length,
			background_color, font
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(msg_id) DO UPDATE SET
			ts=excluded.ts,
			from_me=excluded.from_me,
			sender_jid=excluded.sender_jid,
			sender_name=excluded.sender_name,
			text=excluded.text,
			media_type=excluded.media_type,
			media_caption=excluded.media_caption,
			filename=excluded.filename,
			mime_type=excluded.mime_type,
			direct_path=excluded.direct_path,
			media_key=excluded.media_key,
			file_sha256=excluded.file_sha256,
			file_enc_sha256=excluded.file_enc_sha256,
			file_length=excluded.file_length,
			background_color=excluded.background_color,
			font=excluded.font
	`, p.MsgID, unix(p.Timestamp), boolToInt(p.FromMe), nullIfEmpty(p.SenderJID), nullIfEmpty(p.SenderName), nullIfEmpty(p.Text),
		nullIfEmpty(p.MediaType), nullIfEmpty(p.MediaCaption), nullIfEmpty(p.Filename), nullIfEmpty(p.MimeType), nullIfEmpty(p.DirectPath),
		p.MediaKey, p.FileSHA256, p.FileEncSHA256, int64(p.FileLength), nullIfEmpty(p.BackgroundColor), int64(p.Font))
	return err
}

const statusMessageColumns = `rowid, msg_id, ts, from_me,
			COALESCE(sender_jid,''), COALESCE(sender_name,''), COALESCE(text,''),
			COALESCE(media_type,''), COALESCE(media_caption,''), COALESCE(filename,''),
			COALESCE(mime_type,''), COALESCE(direct_path,''), media_key, file_sha256,
			file_enc_sha256, COALESCE(file_length,0), COALESCE(background_color,''), COALESCE(font,0)`

func (d *DB) GetStatusMessage(msgID string) (StatusMessage, error) {
	row := d.sql.QueryRow(`SELECT `+statusMessageColumns+`
		FROM status_messages
		WHERE msg_id = ?
	`, strings.TrimSpace(msgID))
	out, err := scanStatusMessage(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return StatusMessage{}, sql.ErrNoRows
		}
		return StatusMessage{}, err
	}
	return out, nil
}

func scanStatusMessage(row interface{ Scan(dest ...any) error }) (StatusMessage, error) {
	var out StatusMessage
	var ts int64
	var fromMe int
	var font int64
	var fileLength int64
	if err := row.Scan(&out.RowID, &out.MsgID, &ts, &fromMe, &out.SenderJID, &out.SenderName, &out.Text,
		&out.MediaType, &out.MediaCaption, &out.Filename, &out.MimeType, &out.DirectPath,
		&out.MediaKey, &out.FileSHA256, &out.FileEncSHA256, &fileLength, &out.BackgroundColor, &font); err != nil {
		return StatusMessage{}, err
	}
	out.Timestamp = time.Unix(ts, 0).UTC()
	out.FromMe = fromMe != 0
	out.Font = int32(font)
	out.FileLength = uint64(fileLength)
	return out, nil
}

type ListStatusMessagesParams struct {
	// SenderJIDs limits results to these senders (any identity of one person).
	SenderJIDs []string
	After      *time.Time
	Before     *time.Time
	Limit      int
}

// ListStatusMessages returns stored status broadcasts, newest first.
func (d *DB) ListStatusMessages(p ListStatusMessagesParams) ([]StatusMessage, error) {
	if p.Limit <= 0 {
		p.Limit = 50
	}
	query := `SELECT ` + statusMessageColumns + `
		FROM status_messages
		WHERE 1=1`
	var args []any
	query, args = appendStringFilter(query, args, "sender_jid", "", p.SenderJIDs)
	if p.After != nil {
		query += " AND ts > ?"
		args = append(args, unix(*p.After))
	}
	if p.Before != nil {
		query += " AND ts < ?"
		args = append(args, unix(*p.Before))
	}
	query += " ORDER BY ts DESC, rowid DESC LIMIT ?"
	args = append(args, p.Limit)

	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatusMessage
	for rows.Next() {
		s, err := scanStatusMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
