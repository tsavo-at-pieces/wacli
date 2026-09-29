package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

func newStatusCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Read, download and mute status updates (stories)",
		Long: "Status updates (stories) synced into the local store, and the account's status settings.\n" +
			"list, show, download and mutes read the local store and take no lock. mute, unmute\n" +
			"and privacy use WhatsApp, and run in a running `sync --follow` when it holds the store.\n" +
			"Post a status with `wacli send status`.",
	}
	cmd.AddCommand(newStatusListCmd(flags))
	cmd.AddCommand(newStatusShowCmd(flags))
	cmd.AddCommand(newStatusDownloadCmd(flags))
	cmd.AddCommand(newStatusMutesCmd(flags))
	cmd.AddCommand(newStatusMuteCmd(flags, true))
	cmd.AddCommand(newStatusMuteCmd(flags, false))
	cmd.AddCommand(newStatusPrivacyCmd(flags))
	return cmd
}

// statusRecord is one stored status update as the status commands print it.
// Media keys stay in the store; downloadable says whether `status download`
// has what it needs.
type statusRecord struct {
	ID              string    `json:"id"`
	Timestamp       time.Time `json:"timestamp"`
	FromMe          bool      `json:"from_me"`
	SenderJID       string    `json:"sender_jid,omitempty"`
	SenderName      string    `json:"sender_name,omitempty"`
	SenderMuted     bool      `json:"sender_muted"`
	Text            string    `json:"text,omitempty"`
	MediaType       string    `json:"media_type,omitempty"`
	MediaCaption    string    `json:"media_caption,omitempty"`
	Filename        string    `json:"filename,omitempty"`
	MimeType        string    `json:"mime_type,omitempty"`
	FileLength      uint64    `json:"file_length,omitempty"`
	Downloadable    bool      `json:"downloadable"`
	BackgroundColor string    `json:"background_color,omitempty"`
	Font            int32     `json:"font,omitempty"`
}

func statusRecordFromStore(s store.StatusMessage, muted map[string]bool) statusRecord {
	return statusRecord{
		ID:              s.MsgID,
		Timestamp:       s.Timestamp,
		FromMe:          s.FromMe,
		SenderJID:       s.SenderJID,
		SenderName:      s.SenderName,
		SenderMuted:     !s.FromMe && s.SenderJID != "" && muted[s.SenderJID],
		Text:            s.Text,
		MediaType:       s.MediaType,
		MediaCaption:    s.MediaCaption,
		Filename:        s.Filename,
		MimeType:        s.MimeType,
		FileLength:      s.FileLength,
		Downloadable:    statusDownloadable(s),
		BackgroundColor: s.BackgroundColor,
		Font:            s.Font,
	}
}

func statusDownloadable(s store.StatusMessage) bool {
	return s.MediaType != "" && s.DirectPath != "" && len(s.MediaKey) > 0
}

func newStatusListCmd(flags *rootFlags) *cobra.Command {
	var from string
	var afterStr string
	var beforeStr string
	var limit int

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List stored status updates, newest first",
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit <= 0 {
				return fmt.Errorf("--limit must be > 0")
			}
			after, err := optionalTimeFlag(afterStr)
			if err != nil {
				return err
			}
			before, err := optionalTimeFlag(beforeStr)
			if err != nil {
				return err
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, false, false)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			senders, err := messageChatJIDFilter(ctx, a, from)
			if err != nil {
				return err
			}
			rows, err := a.DB().ListStatusMessages(store.ListStatusMessagesParams{
				SenderJIDs: senders,
				After:      after,
				Before:     before,
				Limit:      limit,
			})
			if err != nil {
				return err
			}
			muted, err := a.DB().MutedStatusJIDs()
			if err != nil {
				return err
			}
			records := make([]statusRecord, 0, len(rows))
			for _, row := range rows {
				records = append(records, statusRecordFromStore(row, muted))
			}
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, map[string]any{"statuses": records})
			}
			return writeStatusList(os.Stdout, records, fullTableOutput(flags.fullOutput))
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "only statuses from this sender (phone number or JID)")
	cmd.Flags().StringVar(&afterStr, "after", "", "only statuses after time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&beforeStr, "before", "", "only statuses before time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().IntVar(&limit, "limit", 50, "max number of statuses to return")
	return cmd
}

func optionalTimeFlag(raw string) (*time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	t, err := parseTime(raw)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func writeStatusList(dst io.Writer, records []statusRecord, fullOutput bool) error {
	w := newTableWriter(dst)
	fmt.Fprintln(w, "TIME\tFROM\tTYPE\tMUTED\tID\tTEXT")
	for _, r := range records {
		muted := ""
		if r.SenderMuted {
			muted = "muted"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Timestamp.Local().Format("2006-01-02 15:04:05"),
			tableCell(statusSenderLabel(r), 24, fullOutput),
			statusTypeLabel(r),
			muted,
			tableCell(r.ID, 24, fullOutput),
			tableCell(strings.ReplaceAll(statusBody(r), "\n", " "), 60, fullOutput),
		)
	}
	return w.Flush()
}

func statusSenderLabel(r statusRecord) string {
	switch {
	case r.FromMe:
		return "me"
	case r.SenderName != "":
		return r.SenderName
	case r.SenderJID != "":
		return r.SenderJID
	default:
		return "unknown"
	}
}

func statusTypeLabel(r statusRecord) string {
	if r.MediaType != "" {
		return r.MediaType
	}
	return "text"
}

func statusBody(r statusRecord) string {
	if r.Text != "" {
		return r.Text
	}
	return r.MediaCaption
}

func newStatusShowCmd(flags *rootFlags) *cobra.Command {
	var id string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show one stored status update",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(id) == "" {
				return fmt.Errorf("--id is required")
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, false, false)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			row, err := getStatusMessage(a.DB(), id)
			if err != nil {
				return err
			}
			muted, err := a.DB().MutedStatusJIDs()
			if err != nil {
				return err
			}
			record := statusRecordFromStore(row, muted)
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, record)
			}
			return writeStatusDetail(os.Stdout, record)
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "status message ID")
	return cmd
}

func getStatusMessage(db *store.DB, id string) (store.StatusMessage, error) {
	row, err := db.GetStatusMessage(id)
	if errors.Is(err, sql.ErrNoRows) {
		return store.StatusMessage{}, fmt.Errorf("status %s not found in the local store (run `wacli sync` first)", strings.TrimSpace(id))
	}
	return row, err
}

func writeStatusDetail(dst io.Writer, r statusRecord) error {
	fmt.Fprintf(dst, "ID: %s\n", sanitize(r.ID))
	fmt.Fprintf(dst, "Time: %s\n", r.Timestamp.Local().Format(time.RFC3339))
	from := statusSenderLabel(r)
	if !r.FromMe && r.SenderJID != "" && r.SenderName != "" {
		from = fmt.Sprintf("%s (%s)", r.SenderName, r.SenderJID)
	}
	fmt.Fprintf(dst, "From: %s\n", sanitize(from))
	if !r.FromMe {
		fmt.Fprintf(dst, "Sender muted: %t\n", r.SenderMuted)
	}
	fmt.Fprintf(dst, "Type: %s\n", statusTypeLabel(r))
	if r.Text != "" {
		fmt.Fprintf(dst, "Text: %s\n", sanitizeBody(r.Text))
	}
	if r.MediaType != "" {
		if r.MediaCaption != "" && r.MediaCaption != r.Text {
			fmt.Fprintf(dst, "Caption: %s\n", sanitizeBody(r.MediaCaption))
		}
		if r.Filename != "" {
			fmt.Fprintf(dst, "Filename: %s\n", sanitize(r.Filename))
		}
		if r.MimeType != "" {
			fmt.Fprintf(dst, "MIME: %s\n", sanitize(r.MimeType))
		}
		if r.FileLength > 0 {
			fmt.Fprintf(dst, "Size: %d bytes\n", r.FileLength)
		}
		fmt.Fprintf(dst, "Downloadable: %t\n", r.Downloadable)
	}
	if r.BackgroundColor != "" {
		fmt.Fprintf(dst, "Background: %s\n", sanitize(r.BackgroundColor))
	}
	if r.Font != 0 {
		fmt.Fprintf(dst, "Font: %d\n", r.Font)
	}
	return nil
}

// downloadStatusMedia fetches and decrypts media from WhatsApp's CDN without
// a WhatsApp connection. Tests replace it to stay offline.
var downloadStatusMedia = wa.DownloadMediaDirectToFile

func newStatusDownloadCmd(flags *rootFlags) *cobra.Command {
	var id string
	var outputPath string
	cmd := &cobra.Command{
		Use:   "download",
		Short: "Download a status update's media to a file",
		Long: "Download a stored status update's media straight from WhatsApp's media servers.\n" +
			"It needs no WhatsApp connection and no store lock, so it works while sync is running\n" +
			"and in read-only mode. Status media expires from WhatsApp's servers after about a day.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(id) == "" {
				return fmt.Errorf("--id is required")
			}
			if strings.TrimSpace(outputPath) == "" {
				return fmt.Errorf("--output is required")
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, false, false)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			row, err := getStatusMessage(a.DB(), id)
			if err != nil {
				return err
			}
			if !statusDownloadable(row) {
				return fmt.Errorf("status %s has no downloadable media", row.MsgID)
			}
			info := store.MediaDownloadInfo{
				ChatJID:       types.StatusBroadcastJID.String(),
				MsgID:         row.MsgID,
				MediaType:     row.MediaType,
				Filename:      row.Filename,
				MimeType:      row.MimeType,
				DirectPath:    row.DirectPath,
				MediaKey:      row.MediaKey,
				FileSHA256:    row.FileSHA256,
				FileEncSHA256: row.FileEncSHA256,
				FileLength:    row.FileLength,
			}
			target, err := a.ResolveMediaOutputPath(info, outputPath)
			if err != nil {
				return err
			}
			n, err := downloadStatusMedia(ctx, info.DirectPath, info.FileEncSHA256, info.FileSHA256, info.MediaKey, info.FileLength, info.MediaType, target)
			if err != nil {
				return err
			}
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, map[string]any{
					"id":         row.MsgID,
					"sender_jid": row.SenderJID,
					"from_me":    row.FromMe,
					"path":       target,
					"bytes":      n,
					"media_type": row.MediaType,
					"mime_type":  row.MimeType,
					"downloaded": true,
				})
			}
			fmt.Fprintf(os.Stdout, "%s (%d bytes)\n", target, n)
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "status message ID")
	cmd.Flags().StringVar(&outputPath, "output", "", "output file or existing directory")
	return cmd
}

func newStatusMutesCmd(flags *rootFlags) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "mutes",
		Short: "List contacts whose status updates are muted",
		Long: "List status mutes mirrored from WhatsApp app state. Sync records a mute or unmute\n" +
			"made on any device; mutes older than this store appear after WhatsApp resends the\n" +
			"full app state (for example after pairing).",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, false, false)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			mutes, err := a.DB().ListStatusMutes(all)
			if err != nil {
				return err
			}
			if mutes == nil {
				mutes = []store.StatusMute{}
			}
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, map[string]any{"mutes": mutes})
			}
			w := newTableWriter(os.Stdout)
			fmt.Fprintln(w, "JID\tINDEX JID\tMUTED\tUPDATED")
			for _, m := range mutes {
				fmt.Fprintf(w, "%s\t%s\t%t\t%s\n", m.JID, m.IndexJID, m.Muted, m.UpdatedAt.Local().Format("2006-01-02 15:04:05"))
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include unmuted entries")
	return cmd
}

func newStatusMuteCmd(flags *rootFlags, mute bool) *cobra.Command {
	use, short, kind := "mute", "Mute a contact's status updates", statusMuteKind
	if !mute {
		use, short, kind = "unmute", "Unmute a contact's status updates", statusUnmuteKind
	}
	var target string
	var pick int
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long: short + ".\n\nThis changes only what this account sees; the contact is not told. When sync has\n" +
			"mirrored an earlier mute for the contact, the same identity (phone or LID) is used.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(target) == "" {
				return fmt.Errorf("--jid is required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				return delegateAfterOpenFailure(ctx, flags, err, sendDelegateRequest{Kind: kind, To: target, Pick: pick}, func(resp sendDelegateResponse) error {
					return writeStatusMuteResult(flags, statusMuteResult{JID: resp.Chat, Muted: mute})
				})
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			removePersistenceHandler, err := a.AddChatStatePersistenceHandler(ctx)
			if err != nil {
				return err
			}
			defer func() {
				// As for chat state: persist app-state events until the socket
				// is closed, then let App.Close drain them.
				a.WA().Disconnect()
				removePersistenceHandler()
			}()
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}
			res, err := setContactStatusMute(ctx, a, target, recipientOptions{pick: pick, asJSON: flags.asJSON}, mute)
			if err != nil {
				return err
			}
			return writeStatusMuteResult(flags, res)
		},
	}
	cmd.Flags().StringVar(&target, "jid", "", "contact phone number, JID, or name")
	cmd.Flags().IntVar(&pick, "pick", 0, "when --jid is ambiguous, pick the Nth match (1-indexed)")
	return cmd
}

// statusMuteApp mutes status updates and records the result: *app.App, or a
// test fake.
type statusMuteApp interface {
	waStoreApp
	MuteStatus(context.Context, types.JID, bool) error
}

type statusMuteResult struct {
	JID   string
	Muted bool
}

// setContactStatusMute resolves the contact and changes its status mute. The
// JID in the app-state index is the one WhatsApp already uses for this contact
// when sync has mirrored it, else the resolved JID.
func setContactStatusMute(ctx context.Context, a statusMuteApp, raw string, opts recipientOptions, mute bool) (statusMuteResult, error) {
	if strings.TrimSpace(raw) == "" {
		return statusMuteResult{}, fmt.Errorf("--jid is required")
	}
	jid, err := resolveRecipient(a, raw, opts)
	if err != nil {
		return statusMuteResult{}, err
	}
	if jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer {
		return statusMuteResult{}, fmt.Errorf("status mute needs a contact (phone number or user JID), got %s", jid)
	}
	target, err := statusMuteIndexJID(ctx, a, jid.ToNonAD())
	if err != nil {
		return statusMuteResult{}, err
	}
	if err := a.MuteStatus(ctx, target, mute); err != nil {
		return statusMuteResult{}, err
	}
	return statusMuteResult{JID: target.String(), Muted: mute}, nil
}

func statusMuteIndexJID(ctx context.Context, a waStoreApp, jid types.JID) (types.JID, error) {
	candidates := []string{jid.String()}
	switch jid.Server {
	case types.DefaultUserServer:
		candidates = append(candidates, a.WA().ResolvePNToLID(ctx, jid).String())
	case types.HiddenUserServer:
		candidates = append(candidates, a.WA().ResolveLIDToPN(ctx, jid).String())
	}
	existing, ok, err := a.DB().FindStatusMute(candidates...)
	if err != nil {
		return types.JID{}, fmt.Errorf("look up status mute: %w", err)
	}
	if !ok {
		return jid, nil
	}
	index, err := types.ParseJID(existing.IndexJID)
	if err != nil || (index.Server != types.DefaultUserServer && index.Server != types.HiddenUserServer) {
		return jid, nil
	}
	return index, nil
}

func writeStatusMuteResult(flags *rootFlags, res statusMuteResult) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{"jid": res.JID, "muted": res.Muted})
	}
	action := "status muted"
	if !res.Muted {
		action = "status unmuted"
	}
	fmt.Fprintf(os.Stdout, "%s: %s\n", action, res.JID)
	return nil
}

func newStatusPrivacyCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "privacy",
		Short: "Show who receives this account's status updates (live)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := flags.requireWritable(); err != nil {
				return err
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				return delegateAfterOpenFailure(ctx, flags, err, sendDelegateRequest{Kind: statusPrivacyKind}, func(resp sendDelegateResponse) error {
					var rows []statusPrivacyRecord
					if err := decodeDelegatedResult(resp, &rows); err != nil {
						return err
					}
					return writeStatusPrivacy(flags, rows)
				})
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}
			rows, err := fetchStatusPrivacy(ctx, a)
			if err != nil {
				return err
			}
			return writeStatusPrivacy(flags, rows)
		},
	}
}

// statusPrivacyRecord is one status audience list. The default one applies
// to new status updates.
type statusPrivacyRecord struct {
	Type    string   `json:"type"`
	Default bool     `json:"default"`
	List    []string `json:"list,omitempty"`
}

func fetchStatusPrivacy(ctx context.Context, a waStoreApp) ([]statusPrivacyRecord, error) {
	lists, err := a.WA().GetStatusPrivacy(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]statusPrivacyRecord, 0, len(lists))
	for _, l := range lists {
		row := statusPrivacyRecord{Type: string(l.Type), Default: l.IsDefault}
		for _, jid := range l.List {
			row.List = append(row.List, jid.String())
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func writeStatusPrivacy(flags *rootFlags, rows []statusPrivacyRecord) error {
	if rows == nil {
		rows = []statusPrivacyRecord{}
	}
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{"privacy": rows})
	}
	for _, row := range rows {
		label := sanitize(row.Type)
		if row.Default {
			label += " (default)"
		}
		switch row.Type {
		case string(types.StatusPrivacyTypeContacts):
			fmt.Fprintf(os.Stdout, "%s: all contacts\n", label)
		case string(types.StatusPrivacyTypeBlacklist):
			fmt.Fprintf(os.Stdout, "%s: all contacts except %d\n", label, len(row.List))
		case string(types.StatusPrivacyTypeWhitelist):
			fmt.Fprintf(os.Stdout, "%s: only %d contacts\n", label, len(row.List))
		default:
			fmt.Fprintf(os.Stdout, "%s: %d contacts\n", label, len(row.List))
		}
		for _, jid := range row.List {
			fmt.Fprintf(os.Stdout, "  %s\n", sanitize(jid))
		}
	}
	return nil
}
