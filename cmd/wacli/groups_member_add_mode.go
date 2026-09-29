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

func newGroupsMemberAddModeCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	var admins, all bool
	cmd := &cobra.Command{
		Use:   "member-add-mode",
		Short: "Set who can add participants: admins only or all members",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" {
				return fmt.Errorf("--jid is required")
			}
			mode, err := parseMemberAddModeFlags(cmd, admins, all)
			if err != nil {
				return err
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			if _, err := parseGroupJID(jidStr); err != nil {
				return err
			}
			return runLiveGroupCommand(flags, sendDelegateRequest{Kind: groupMemberAddModeKind, To: jidStr, MemberAddMode: string(mode)}, func(resp sendDelegateResponse) error {
				if flags.asJSON {
					return out.WriteJSON(os.Stdout, map[string]any{"jid": resp.Chat, "member_add_mode": string(mode)})
				}
				fmt.Fprintln(os.Stdout, "OK")
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "group JID (…@g.us)")
	cmd.Flags().BoolVar(&admins, "admins", false, "only admins can add participants")
	cmd.Flags().BoolVar(&all, "all", false, "every member can add participants")
	return cmd
}

// parseMemberAddModeFlags requires exactly one of --admins or --all.
func parseMemberAddModeFlags(cmd *cobra.Command, admins, all bool) (types.GroupMemberAddMode, error) {
	adminsSet := cmd.Flags().Changed("admins")
	allSet := cmd.Flags().Changed("all")
	if adminsSet == allSet {
		return "", fmt.Errorf("exactly one of --admins or --all is required")
	}
	if adminsSet {
		if !admins {
			return "", fmt.Errorf("--admins=false does not select a mode; use --all instead")
		}
		return types.GroupMemberAddModeAdmin, nil
	}
	if !all {
		return "", fmt.Errorf("--all=false does not select a mode; use --admins instead")
	}
	return types.GroupMemberAddModeAllMember, nil
}

// executeGroupMemberAddMode sets req.MemberAddMode on the group.
func executeGroupMemberAddMode(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	mode := types.GroupMemberAddMode(req.MemberAddMode)
	if mode != types.GroupMemberAddModeAdmin && mode != types.GroupMemberAddModeAllMember {
		return sendDelegateResponse{}, fmt.Errorf("member add mode must be %q or %q, got %q", types.GroupMemberAddModeAdmin, types.GroupMemberAddModeAllMember, req.MemberAddMode)
	}
	gjid, err := parseGroupJID(req.To)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	if err := a.WA().SetGroupMemberAddMode(ctx, gjid, mode); err != nil {
		return sendDelegateResponse{}, err
	}
	refreshGroupInfo(ctx, a, gjid)
	return sendDelegateResponse{OK: true, Chat: gjid.String()}, nil
}
