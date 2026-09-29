package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

func newChannelsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "channels",
		Short: "Manage WhatsApp channels",
	}
	cmd.AddCommand(newChannelsListCmd(flags))
	cmd.AddCommand(newChannelsInfoCmd(flags))
	cmd.AddCommand(newChannelsJoinCmd(flags))
	cmd.AddCommand(newChannelsLeaveCmd(flags))
	cmd.AddCommand(newChannelsMuteCmd(flags, true))
	cmd.AddCommand(newChannelsMuteCmd(flags, false))
	cmd.AddCommand(newChannelsReactCmd(flags))
	cmd.AddCommand(newChannelsMessagesCmd(flags))
	cmd.AddCommand(newChannelsMarkViewedCmd(flags))
	cmd.AddCommand(newChannelsCreateCmd(flags))
	return cmd
}

// runChannelCommand runs a live channel command directly, or in a same-store
// `sync --follow` that holds the lock. direct runs connected; delegated prints
// the sync process's reply.
func runChannelCommand(flags *rootFlags, req sendDelegateRequest, direct func(context.Context, waStoreApp) error, delegated func(sendDelegateResponse) error) error {
	if err := flags.requireWritable(); err != nil {
		return err
	}
	ctx, cancel := withTimeout(context.Background(), flags)
	defer cancel()

	a, lk, err := newApp(ctx, flags, true, false)
	if err != nil {
		return delegateAfterOpenFailure(ctx, flags, err, req, delegated)
	}
	defer closeApp(a, lk)

	if err := a.EnsureAuthed(ctx); err != nil {
		return err
	}
	if err := a.Connect(ctx, false, nil); err != nil {
		return err
	}
	return direct(ctx, a)
}

func newChannelsListCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List subscribed channels (live) and update local chats",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runChannelCommand(flags, sendDelegateRequest{Kind: channelsListKind}, func(ctx context.Context, a waStoreApp) error {
				rows, err := listChannels(ctx, a)
				if err != nil {
					return err
				}
				return writeChannelsList(flags, rows)
			}, func(resp sendDelegateResponse) error {
				var rows []channelRecord
				if err := decodeDelegatedResult(resp, &rows); err != nil {
					return err
				}
				return writeChannelsList(flags, rows)
			})
		},
	}
	return cmd
}

// listChannels fetches subscribed channels and stores them as chats.
func listChannels(ctx context.Context, a waStoreApp) ([]channelRecord, error) {
	list, err := a.WA().GetSubscribedNewsletters(ctx)
	if err != nil {
		return nil, err
	}
	rows := channelRecords(list)
	if err := persistChannelRecords(a.DB(), rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func writeChannelsList(flags *rootFlags, rows []channelRecord) error {
	if rows == nil {
		rows = []channelRecord{}
	}
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, rows)
	}

	w := newTableWriter(os.Stdout)
	fmt.Fprintln(w, "NAME\tJID\tROLE\tSTATE\tSUBSCRIBERS\tDESCRIPTION")
	fullOutput := fullTableOutput(flags.fullOutput)
	for _, row := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n",
			tableCell(row.Name, 40, fullOutput),
			row.JID,
			row.Role,
			row.State,
			row.Subscribers,
			tableCell(strings.ReplaceAll(row.Description, "\n", " "), 50, fullOutput),
		)
	}
	_ = w.Flush()
	return nil
}

func newChannelsInfoCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	cmd := &cobra.Command{
		Use:   "info",
		Short: "Fetch channel info (live) and update local chats",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" {
				return fmt.Errorf("--jid is required")
			}
			return runChannelCommand(flags, sendDelegateRequest{Kind: channelInfoKind, To: jidStr}, func(ctx context.Context, a waStoreApp) error {
				row, err := fetchChannelInfo(ctx, a, jidStr)
				if err != nil {
					return err
				}
				return writeChannelInfo(flags, row)
			}, func(resp sendDelegateResponse) error {
				var row channelRecord
				if err := decodeDelegatedResult(resp, &row); err != nil {
					return err
				}
				return writeChannelInfo(flags, row)
			})
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "channel JID (...@newsletter)")
	return cmd
}

// fetchChannelInfo fetches one channel's metadata and stores it as a chat.
func fetchChannelInfo(ctx context.Context, a waStoreApp, rawJID string) (channelRecord, error) {
	jid, err := parseChannelJID(rawJID)
	if err != nil {
		return channelRecord{}, err
	}
	meta, err := a.WA().GetNewsletterInfo(ctx, jid)
	if err != nil {
		return channelRecord{}, err
	}
	if meta == nil {
		return channelRecord{}, fmt.Errorf("channel not found")
	}
	row := channelRecordFromMeta(meta)
	if err := persistChannelRecords(a.DB(), []channelRecord{row}); err != nil {
		return channelRecord{}, err
	}
	return row, nil
}

func writeChannelInfo(flags *rootFlags, row channelRecord) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, row)
	}

	fmt.Fprintf(os.Stdout, "JID: %s\nName: %s\nDescription: %s\nState: %s\nSubscribers: %d\n",
		sanitize(row.JID),
		sanitize(row.Name),
		sanitize(row.Description),
		sanitize(row.State),
		row.Subscribers,
	)
	if row.Role != "" {
		fmt.Fprintf(os.Stdout, "Role: %s\nMute: %s\n", sanitize(row.Role), sanitize(row.Mute))
	}
	return nil
}

func newChannelsJoinCmd(flags *rootFlags) *cobra.Command {
	var invite string
	cmd := &cobra.Command{
		Use:   "join",
		Short: "Join a channel via invite link or code",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(invite) == "" {
				return fmt.Errorf("--invite is required")
			}
			return runChannelCommand(flags, sendDelegateRequest{Kind: channelJoinKind, InviteCode: invite}, func(ctx context.Context, a waStoreApp) error {
				row, err := joinChannel(ctx, a, invite)
				if err != nil {
					return err
				}
				return writeChannelJoined(flags, row)
			}, func(resp sendDelegateResponse) error {
				var row channelRecord
				if err := decodeDelegatedResult(resp, &row); err != nil {
					return err
				}
				return writeChannelJoined(flags, row)
			})
		},
	}
	cmd.Flags().StringVar(&invite, "invite", "", "invite link or code, e.g. https://whatsapp.com/channel/...")
	return cmd
}

// joinChannel follows the channel an invite names and stores it as a chat.
func joinChannel(ctx context.Context, a waStoreApp, invite string) (channelRecord, error) {
	invite = strings.TrimSpace(invite)
	if invite == "" {
		return channelRecord{}, fmt.Errorf("--invite is required")
	}
	meta, err := a.WA().GetNewsletterInfoWithInvite(ctx, invite)
	if err != nil {
		return channelRecord{}, err
	}
	if meta == nil {
		return channelRecord{}, fmt.Errorf("could not resolve channel from invite")
	}
	if err := a.WA().FollowNewsletter(ctx, meta.ID); err != nil {
		return channelRecord{}, err
	}
	row := channelRecordFromMeta(meta)
	if err := persistChannelRecords(a.DB(), []channelRecord{row}); err != nil {
		return channelRecord{}, err
	}
	return row, nil
}

func writeChannelJoined(flags *rootFlags, row channelRecord) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{"joined": true, "channel": row})
	}
	fmt.Fprintf(os.Stdout, "Joined channel %s (%s).\n", sanitize(row.Name), sanitize(row.JID))
	return nil
}

func newChannelsLeaveCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	cmd := &cobra.Command{
		Use:   "leave",
		Short: "Leave (unfollow) a channel",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" {
				return fmt.Errorf("--jid is required")
			}
			return runChannelCommand(flags, sendDelegateRequest{Kind: channelLeaveKind, To: jidStr}, func(ctx context.Context, a waStoreApp) error {
				jid, err := leaveChannel(ctx, a, jidStr)
				if err != nil {
					return err
				}
				return writeChannelLeft(flags, jid.String())
			}, func(resp sendDelegateResponse) error {
				return writeChannelLeft(flags, resp.Chat)
			})
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "channel JID (...@newsletter)")
	return cmd
}

func leaveChannel(ctx context.Context, a waStoreApp, rawJID string) (types.JID, error) {
	jid, err := parseChannelJID(rawJID)
	if err != nil {
		return types.JID{}, err
	}
	if err := a.WA().UnfollowNewsletter(ctx, jid); err != nil {
		return types.JID{}, err
	}
	return jid, nil
}

func writeChannelLeft(flags *rootFlags, jid string) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{"left": true, "jid": jid})
	}
	fmt.Fprintf(os.Stdout, "Left channel %s.\n", jid)
	return nil
}

type channelRecord struct {
	JID         string `json:"jid"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Role        string `json:"role,omitempty"`
	Mute        string `json:"mute,omitempty"`
	State       string `json:"state,omitempty"`
	Subscribers int    `json:"subscribers,omitempty"`
}

func channelRecords(list []*types.NewsletterMetadata) []channelRecord {
	rows := make([]channelRecord, 0, len(list))
	for _, meta := range list {
		if meta == nil {
			continue
		}
		rows = append(rows, channelRecordFromMeta(meta))
	}
	return rows
}

func channelRecordFromMeta(meta *types.NewsletterMetadata) channelRecord {
	row := channelRecord{
		JID:         meta.ID.String(),
		Name:        wa.NewsletterName(meta),
		Description: strings.TrimSpace(meta.ThreadMeta.Description.Text),
		State:       string(meta.State.Type),
		Subscribers: meta.ThreadMeta.SubscriberCount,
	}
	if row.Name == "" {
		row.Name = row.JID
	}
	if meta.ViewerMeta != nil {
		row.Role = string(meta.ViewerMeta.Role)
		row.Mute = string(meta.ViewerMeta.Mute)
	}
	return row
}

type channelRecordStore interface {
	UpsertChat(jid, kind, name string, lastTS time.Time) error
}

func persistChannelRecords(db channelRecordStore, rows []channelRecord) error {
	now := time.Now().UTC()
	for _, row := range rows {
		if err := db.UpsertChat(row.JID, "newsletter", row.Name, now); err != nil {
			return fmt.Errorf("persist channel %s: %w", row.JID, err)
		}
	}
	return nil
}

func parseChannelJID(raw string) (types.JID, error) {
	jid, err := types.ParseJID(strings.TrimSpace(raw))
	if err != nil {
		return types.JID{}, err
	}
	if jid.Server != types.NewsletterServer {
		return types.JID{}, fmt.Errorf("JID must be a channel (...@newsletter)")
	}
	return jid, nil
}
