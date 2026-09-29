package main

import (
	"context"
	"encoding/json"
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
	cmd.AddCommand(newGroupsInviteInfoCmd(flags))
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
			if err := requireLiveRead(flags); err != nil {
				return err
			}
			return runLiveGroupCommand(flags, sendDelegateRequest{Kind: groupInviteLinkGetKind, To: jidStr}, func(resp sendDelegateResponse) error {
				if flags.asJSON {
					return out.WriteJSON(os.Stdout, map[string]any{"jid": resp.Chat, "link": resp.Link})
				}
				fmt.Fprintln(os.Stdout, resp.Link)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "group JID (…@g.us)")
	return cmd
}

// executeGroupInviteLinkGet returns the current invite link without
// resetting it.
func executeGroupInviteLinkGet(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	gjid, err := types.ParseJID(req.To)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	link, err := a.WA().GetGroupInviteLink(ctx, gjid, false)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Chat: gjid.String(), Link: link}, nil
}

func newGroupsInviteInfoCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "info <link-or-code>",
		Short: "Preview the group behind an invite link without joining it",
		Long: `Preview the group behind an invite link without joining it.

Accepts a full https://chat.whatsapp.com/... link or the bare invite code.
Nothing is joined and nothing is stored locally.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			code, err := parseGroupInviteCode(args[0])
			if err != nil {
				return err
			}
			if err := requireLiveRead(flags); err != nil {
				return err
			}
			return runLiveGroupCommand(flags, sendDelegateRequest{Kind: groupInviteInfoKind, InviteCode: code}, func(resp sendDelegateResponse) error {
				return writeGroupInvitePreview(flags, resp.Group)
			})
		},
	}
	return cmd
}

// parseGroupInviteCode takes an invite link or bare code and returns the code.
func parseGroupInviteCode(raw string) (string, error) {
	code := strings.TrimSpace(raw)
	for _, scheme := range []string{"https://", "http://"} {
		if len(code) >= len(scheme) && strings.EqualFold(code[:len(scheme)], scheme) {
			code = code[len(scheme):]
			break
		}
	}
	const host = "chat.whatsapp.com/"
	if len(code) >= len(host) && strings.EqualFold(code[:len(host)], host) {
		code = strings.TrimPrefix(code[len(host):], "invite/")
	}
	if i := strings.IndexAny(code, "?#"); i >= 0 {
		code = code[:i]
	}
	code = strings.TrimSuffix(code, "/")
	if code == "" || strings.ContainsAny(code, "/ \t") {
		return "", fmt.Errorf("invalid invite link or code %q", raw)
	}
	return code, nil
}

// executeGroupInviteInfo previews the group behind an invite code. It joins
// nothing and stores nothing: the caller is not a member.
func executeGroupInviteInfo(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	code, err := parseGroupInviteCode(req.InviteCode)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	info, err := a.WA().GetGroupInfoFromLink(ctx, code)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	if info == nil {
		return sendDelegateResponse{}, fmt.Errorf("no group info for invite code %s", code)
	}
	raw, err := encodeGroupResult(info)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Chat: info.JID.String(), Group: raw}, nil
}

// writeGroupInvitePreview prints an invite preview: the whatsmeow group info
// as-is with --json, otherwise a summary. A preview lists few participants
// or none, so the summary shows the group's reported size.
func writeGroupInvitePreview(flags *rootFlags, raw json.RawMessage) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, groupResultJSON(raw))
	}
	var info *types.GroupInfo
	if err := decodeGroupResult(raw, &info); err != nil {
		return err
	}
	if info == nil {
		return fmt.Errorf("no group info in the result")
	}
	participants := info.ParticipantCount
	if n := len(info.Participants); n > participants {
		participants = n
	}
	writeGroupSummary(os.Stdout, info, participants)
	if info.Topic != "" {
		fmt.Fprintf(os.Stdout, "Description: %s\n", sanitize(info.Topic))
	}
	if info.IsJoinApprovalRequired {
		fmt.Fprintln(os.Stdout, "Join approval: required")
	}
	return nil
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
