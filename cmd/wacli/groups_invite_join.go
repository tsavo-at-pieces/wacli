package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/openclaw/wacli/internal/out"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

func newGroupsInviteCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "invite",
		Short: "Manage group invite links",
	}
	cmd.AddCommand(newGroupsInviteLinkCmd(flags))
	return cmd
}

func newGroupsInviteLinkCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "link",
		Short: "Get or revoke invite links",
	}
	cmd.AddCommand(newGroupsInviteLinkGetCmd(flags))
	cmd.AddCommand(newGroupsInviteLinkRevokeCmd(flags))
	return cmd
}

func newGroupsInviteLinkGetCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Get invite link",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" {
				return fmt.Errorf("--jid is required")
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}
			gjid, err := types.ParseJID(jidStr)
			if err != nil {
				return err
			}
			link, err := a.WA().GetGroupInviteLink(ctx, gjid, false)
			if err != nil {
				return err
			}
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, map[string]any{"jid": gjid.String(), "link": link})
			}
			fmt.Fprintln(os.Stdout, link)
			return nil
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "group JID (…@g.us)")
	return cmd
}

func newGroupsInviteLinkRevokeCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	cmd := &cobra.Command{
		Use:   "revoke",
		Short: "Revoke/reset invite link",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" {
				return fmt.Errorf("--jid is required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				return delegateAfterOpenFailure(ctx, flags, err, sendDelegateRequest{Kind: groupInviteRevokeKind, To: jidStr}, func(resp sendDelegateResponse) error {
					return writeGroupInviteRevoked(flags, resp.Chat, resp.Link)
				})
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}
			gjid, link, err := revokeGroupInviteLink(ctx, a, jidStr)
			if err != nil {
				return err
			}
			return writeGroupInviteRevoked(flags, gjid.String(), link)
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "group JID (…@g.us)")
	return cmd
}

// revokeGroupInviteLink resets the invite link and returns the new one.
func revokeGroupInviteLink(ctx context.Context, a waStoreApp, rawJID string) (types.JID, string, error) {
	gjid, err := types.ParseJID(rawJID)
	if err != nil {
		return types.JID{}, "", err
	}
	link, err := a.WA().GetGroupInviteLink(ctx, gjid, true)
	if err != nil {
		return types.JID{}, "", err
	}
	return gjid, link, nil
}

func writeGroupInviteRevoked(flags *rootFlags, jid, link string) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{"jid": jid, "link": link, "revoked": true})
	}
	fmt.Fprintln(os.Stdout, link)
	return nil
}

func newGroupsJoinCmd(flags *rootFlags) *cobra.Command {
	var code string
	cmd := &cobra.Command{
		Use:   "join",
		Short: "Join group by invite code",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(code) == "" {
				return fmt.Errorf("--code is required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				return delegateAfterOpenFailure(ctx, flags, err, sendDelegateRequest{Kind: groupJoinKind, InviteCode: code}, func(resp sendDelegateResponse) error {
					return writeGroupJoined(flags, resp.Chat)
				})
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}
			jid, err := joinGroup(ctx, a, code)
			if err != nil {
				return err
			}
			return writeGroupJoined(flags, jid.String())
		},
	}
	cmd.Flags().StringVar(&code, "code", "", "invite code (from link)")
	return cmd
}

// joinGroup joins with an invite code and stores the group's live info.
func joinGroup(ctx context.Context, a waStoreApp, code string) (types.JID, error) {
	jid, err := a.WA().JoinGroupWithLink(ctx, code)
	if err != nil {
		return types.JID{}, err
	}
	refreshGroupInfo(ctx, a, jid)
	return jid, nil
}

func writeGroupJoined(flags *rootFlags, jid string) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{"jid": jid, "joined": true})
	}
	fmt.Fprintf(os.Stdout, "Joined: %s\n", jid)
	return nil
}
