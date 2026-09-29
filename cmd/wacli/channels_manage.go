package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

func newChannelsMuteCmd(flags *rootFlags, mute bool) *cobra.Command {
	use, short, kind := "mute", "Mute a followed channel", channelMuteKind
	if !mute {
		use, short, kind = "unmute", "Unmute a followed channel", channelUnmuteKind
	}
	var jidStr string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" {
				return fmt.Errorf("--jid is required")
			}
			return runChannelCommand(flags, sendDelegateRequest{Kind: kind, To: jidStr}, func(ctx context.Context, a waStoreApp) error {
				jid, err := setChannelMute(ctx, a, jidStr, mute)
				if err != nil {
					return err
				}
				return writeChannelMuted(flags, jid.String(), mute)
			}, func(resp sendDelegateResponse) error {
				return writeChannelMuted(flags, resp.Chat, mute)
			})
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "channel JID (...@newsletter)")
	return cmd
}

func setChannelMute(ctx context.Context, a waStoreApp, rawJID string, mute bool) (types.JID, error) {
	jid, err := parseChannelJID(rawJID)
	if err != nil {
		return types.JID{}, err
	}
	if err := a.WA().NewsletterToggleMute(ctx, jid, mute); err != nil {
		return types.JID{}, err
	}
	return jid, nil
}

func writeChannelMuted(flags *rootFlags, jid string, mute bool) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{"jid": jid, "muted": mute})
	}
	action := "Muted"
	if !mute {
		action = "Unmuted"
	}
	fmt.Fprintf(os.Stdout, "%s channel %s.\n", action, jid)
	return nil
}

func newChannelsReactCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	var serverID int
	var reaction string
	cmd := &cobra.Command{
		Use:   "react",
		Short: "React to a channel post (empty --reaction removes yours)",
		Long: "React to a channel post by its server ID, as `channels messages` prints it.\n" +
			"Pass --reaction '' to remove this account's reaction.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" {
				return fmt.Errorf("--jid is required")
			}
			if serverID <= 0 {
				return fmt.Errorf("--server-id is required")
			}
			if !cmd.Flags().Changed("reaction") {
				return fmt.Errorf("--reaction is required (pass --reaction '' to remove a reaction)")
			}
			req := sendDelegateRequest{Kind: channelReactKind, To: jidStr, ServerIDs: []int{serverID}, Reaction: reaction}
			return runChannelCommand(flags, req, func(ctx context.Context, a waStoreApp) error {
				res, err := reactToChannelPost(ctx, a, jidStr, serverID, reaction)
				if err != nil {
					return err
				}
				return writeChannelReaction(flags, res)
			}, func(resp sendDelegateResponse) error {
				var res channelReactionResult
				if err := decodeDelegatedResult(resp, &res); err != nil {
					return err
				}
				return writeChannelReaction(flags, res)
			})
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "channel JID (...@newsletter)")
	cmd.Flags().IntVar(&serverID, "server-id", 0, "server ID of the channel post")
	cmd.Flags().StringVar(&reaction, "reaction", "", "reaction emoji; empty removes this account's reaction")
	return cmd
}

type channelReactionResult struct {
	JID      string `json:"jid"`
	ServerID int    `json:"server_id"`
	Reaction string `json:"reaction"`
	ID       string `json:"id"`
}

func reactToChannelPost(ctx context.Context, a waStoreApp, rawJID string, serverID types.MessageServerID, reaction string) (channelReactionResult, error) {
	jid, err := parseChannelJID(rawJID)
	if err != nil {
		return channelReactionResult{}, err
	}
	if serverID <= 0 {
		return channelReactionResult{}, fmt.Errorf("--server-id is required")
	}
	id, err := a.WA().NewsletterSendReaction(ctx, jid, serverID, reaction)
	if err != nil {
		return channelReactionResult{}, err
	}
	return channelReactionResult{JID: jid.String(), ServerID: serverID, Reaction: reaction, ID: string(id)}, nil
}

func writeChannelReaction(flags *rootFlags, res channelReactionResult) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, res)
	}
	if res.Reaction == "" {
		fmt.Fprintf(os.Stdout, "Removed reaction from post %d in %s (id %s)\n", res.ServerID, res.JID, res.ID)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Reacted %s to post %d in %s (id %s)\n", sanitize(res.Reaction), res.ServerID, res.JID, res.ID)
	return nil
}

func newChannelsMessagesCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	var count int
	var before int
	cmd := &cobra.Command{
		Use:   "messages",
		Short: "Fetch recent channel posts (live)",
		Long: "Fetch a channel's recent posts live from WhatsApp, newest first, with their server IDs,\n" +
			"view counts and reaction counts. Nothing is written to the local store: a channel\n" +
			"post's server ID, views and reactions have no place in the messages table, and\n" +
			"followed channels' new posts already arrive through sync. Page back with --before.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" {
				return fmt.Errorf("--jid is required")
			}
			if count <= 0 {
				return fmt.Errorf("--count must be > 0")
			}
			if before < 0 {
				return fmt.Errorf("--before must be a server ID")
			}
			req := sendDelegateRequest{Kind: channelMessagesKind, To: jidStr, Count: count, BeforeServerID: before}
			return runChannelCommand(flags, req, func(ctx context.Context, a waStoreApp) error {
				res, err := fetchChannelMessages(ctx, a, jidStr, count, before)
				if err != nil {
					return err
				}
				return writeChannelMessages(flags, res)
			}, func(resp sendDelegateResponse) error {
				var res channelMessagesResult
				if err := decodeDelegatedResult(resp, &res); err != nil {
					return err
				}
				return writeChannelMessages(flags, res)
			})
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "channel JID (...@newsletter)")
	cmd.Flags().IntVar(&count, "count", 20, "number of posts to fetch")
	cmd.Flags().IntVar(&before, "before", 0, "only posts older than this server ID")
	return cmd
}

type channelMessagesResult struct {
	JID      string                 `json:"jid"`
	Messages []channelMessageRecord `json:"messages"`
}

// channelMessageRecord summarizes one channel post. Media is described, not
// downloaded.
type channelMessageRecord struct {
	ServerID  int            `json:"server_id"`
	ID        string         `json:"id"`
	Timestamp time.Time      `json:"timestamp"`
	Type      string         `json:"type,omitempty"`
	Views     int            `json:"views,omitempty"`
	Reactions map[string]int `json:"reactions,omitempty"`
	Text      string         `json:"text,omitempty"`
	MediaType string         `json:"media_type,omitempty"`
	Caption   string         `json:"caption,omitempty"`
	Filename  string         `json:"filename,omitempty"`
	MimeType  string         `json:"mime_type,omitempty"`
}

func fetchChannelMessages(ctx context.Context, a waStoreApp, rawJID string, count, before int) (channelMessagesResult, error) {
	jid, err := parseChannelJID(rawJID)
	if err != nil {
		return channelMessagesResult{}, err
	}
	if count <= 0 {
		return channelMessagesResult{}, fmt.Errorf("--count must be > 0")
	}
	msgs, err := a.WA().GetNewsletterMessages(ctx, jid, count, before)
	if err != nil {
		return channelMessagesResult{}, err
	}
	res := channelMessagesResult{JID: jid.String(), Messages: make([]channelMessageRecord, 0, len(msgs))}
	for _, m := range msgs {
		if m == nil {
			continue
		}
		res.Messages = append(res.Messages, channelMessageRecordFrom(jid, m))
	}
	sort.SliceStable(res.Messages, func(i, j int) bool { return res.Messages[i].ServerID > res.Messages[j].ServerID })
	return res, nil
}

func channelMessageRecordFrom(channel types.JID, m *types.NewsletterMessage) channelMessageRecord {
	pm := wa.ParseNewsletterMessage(channel, m)
	row := channelMessageRecord{
		ServerID:  m.MessageServerID,
		ID:        m.MessageID,
		Timestamp: m.Timestamp.UTC(),
		Type:      m.Type,
		Views:     m.ViewsCount,
		Reactions: m.ReactionCounts,
		Text:      pm.Text,
	}
	if pm.Media != nil {
		if row.Text == pm.Media.Caption {
			// The parser also copies a caption into the text.
			row.Text = ""
		}
		row.MediaType = pm.Media.Type
		row.Caption = pm.Media.Caption
		row.Filename = pm.Media.Filename
		row.MimeType = pm.Media.MimeType
	}
	return row
}

func writeChannelMessages(flags *rootFlags, res channelMessagesResult) error {
	if res.Messages == nil {
		res.Messages = []channelMessageRecord{}
	}
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, res)
	}
	return writeChannelMessagesTable(os.Stdout, res.Messages, fullTableOutput(flags.fullOutput))
}

func writeChannelMessagesTable(dst io.Writer, rows []channelMessageRecord, fullOutput bool) error {
	w := newTableWriter(dst)
	fmt.Fprintln(w, "SERVER ID\tTIME\tTYPE\tVIEWS\tREACTIONS\tTEXT")
	for _, r := range rows {
		kind := r.MediaType
		if kind == "" {
			kind = r.Type
		}
		body := r.Text
		if body == "" {
			body = r.Caption
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%s\t%s\n",
			r.ServerID,
			r.Timestamp.Local().Format("2006-01-02 15:04:05"),
			sanitize(kind),
			r.Views,
			tableCell(reactionSummary(r.Reactions), 24, fullOutput),
			tableCell(strings.ReplaceAll(body, "\n", " "), 60, fullOutput),
		)
	}
	return w.Flush()
}

// reactionSummary lists reaction counts, most used first.
func reactionSummary(counts map[string]int) string {
	type pair struct {
		code  string
		count int
	}
	pairs := make([]pair, 0, len(counts))
	for code, n := range counts {
		pairs = append(pairs, pair{code, n})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count != pairs[j].count {
			return pairs[i].count > pairs[j].count
		}
		return pairs[i].code < pairs[j].code
	})
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, fmt.Sprintf("%s %d", p.code, p.count))
	}
	return strings.Join(parts, " ")
}

func newChannelsMarkViewedCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	var serverIDs []int
	cmd := &cobra.Command{
		Use:   "mark-viewed",
		Short: "Count channel posts as viewed by this account",
		Long: "Count channel posts as viewed, which raises their view counters. This is not the\n" +
			"same as marking the channel read on your other devices.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" {
				return fmt.Errorf("--jid is required")
			}
			if err := validateServerIDs(serverIDs); err != nil {
				return err
			}
			req := sendDelegateRequest{Kind: channelMarkViewedKind, To: jidStr, ServerIDs: serverIDs}
			return runChannelCommand(flags, req, func(ctx context.Context, a waStoreApp) error {
				res, err := markChannelPostsViewed(ctx, a, jidStr, serverIDs)
				if err != nil {
					return err
				}
				return writeChannelViewed(flags, res)
			}, func(resp sendDelegateResponse) error {
				var res channelViewedResult
				if err := decodeDelegatedResult(resp, &res); err != nil {
					return err
				}
				return writeChannelViewed(flags, res)
			})
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "channel JID (...@newsletter)")
	cmd.Flags().IntSliceVar(&serverIDs, "server-id", nil, "server ID of a channel post (repeatable or comma-separated)")
	return cmd
}

func validateServerIDs(ids []int) error {
	if len(ids) == 0 {
		return fmt.Errorf("--server-id is required")
	}
	for _, id := range ids {
		if id <= 0 {
			return fmt.Errorf("--server-id must be positive, got %d", id)
		}
	}
	return nil
}

type channelViewedResult struct {
	JID       string `json:"jid"`
	ServerIDs []int  `json:"server_ids"`
	Viewed    bool   `json:"viewed"`
}

func markChannelPostsViewed(ctx context.Context, a waStoreApp, rawJID string, serverIDs []int) (channelViewedResult, error) {
	jid, err := parseChannelJID(rawJID)
	if err != nil {
		return channelViewedResult{}, err
	}
	if err := validateServerIDs(serverIDs); err != nil {
		return channelViewedResult{}, err
	}
	if err := a.WA().NewsletterMarkViewed(ctx, jid, serverIDs); err != nil {
		return channelViewedResult{}, err
	}
	return channelViewedResult{JID: jid.String(), ServerIDs: serverIDs, Viewed: true}, nil
}

func writeChannelViewed(flags *rootFlags, res channelViewedResult) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, res)
	}
	noun := "posts"
	if len(res.ServerIDs) == 1 {
		noun = "post"
	}
	fmt.Fprintf(os.Stdout, "Marked %d %s viewed in %s.\n", len(res.ServerIDs), noun, res.JID)
	return nil
}

func newChannelsCreateCmd(flags *rootFlags) *cobra.Command {
	var name string
	var description string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a channel owned by this account",
		Long: "Create a WhatsApp channel owned by this account and store it as a chat.\n" +
			"Channels are public and wacli cannot delete one; WhatsApp may first require\n" +
			"accepting its channel terms in the phone app.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("--name is required")
			}
			req := sendDelegateRequest{Kind: channelCreateKind, Name: name, Description: description}
			return runChannelCommand(flags, req, func(ctx context.Context, a waStoreApp) error {
				row, err := createChannel(ctx, a, name, description)
				if err != nil {
					return err
				}
				return writeChannelCreated(flags, row)
			}, func(resp sendDelegateResponse) error {
				var row channelRecord
				if err := decodeDelegatedResult(resp, &row); err != nil {
					return err
				}
				return writeChannelCreated(flags, row)
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "channel name")
	cmd.Flags().StringVar(&description, "description", "", "channel description")
	return cmd
}

func createChannel(ctx context.Context, a waStoreApp, name, description string) (channelRecord, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return channelRecord{}, fmt.Errorf("--name is required")
	}
	meta, err := a.WA().CreateNewsletter(ctx, name, strings.TrimSpace(description))
	if err != nil {
		return channelRecord{}, err
	}
	if meta == nil {
		return channelRecord{}, fmt.Errorf("WhatsApp returned no channel")
	}
	row := channelRecordFromMeta(meta)
	if err := persistChannelRecords(a.DB(), []channelRecord{row}); err != nil {
		return channelRecord{}, err
	}
	return row, nil
}

func writeChannelCreated(flags *rootFlags, row channelRecord) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{"created": true, "channel": row})
	}
	fmt.Fprintf(os.Stdout, "Created channel %s (%s).\n", sanitize(row.Name), sanitize(row.JID))
	return nil
}
