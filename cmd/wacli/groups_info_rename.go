package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

func newGroupsInfoCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	cmd := &cobra.Command{
		Use:   "info",
		Short: "Fetch group info (live) and update local DB",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" {
				return fmt.Errorf("--jid is required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			return runLiveGroupCommand(flags, sendDelegateRequest{Kind: groupInfoKind, To: jidStr}, func(resp sendDelegateResponse) error {
				return writeGroupInfo(flags, resp.Group)
			})
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "group JID (…@g.us)")
	return cmd
}

// executeGroupInfo fetches one group live and stores it locally.
func executeGroupInfo(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	gjid, err := types.ParseJID(req.To)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	info, err := a.WA().GetGroupInfo(ctx, gjid)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	if info == nil {
		return sendDelegateResponse{}, fmt.Errorf("group info not found for %s", gjid.String())
	}
	_ = persistGroupInfo(ctx, a.DB(), a.WA(), info)
	raw, err := encodeGroupResult(info)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Chat: gjid.String(), Group: raw}, nil
}

// writeGroupInfo prints encoded whatsmeow group info: as-is with --json,
// otherwise as a summary.
func writeGroupInfo(flags *rootFlags, raw json.RawMessage) error {
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
	writeGroupSummary(os.Stdout, info, len(info.Participants))
	return nil
}

// writeGroupSummary prints the human summary of a group. participants is the
// count to show.
func writeGroupSummary(w io.Writer, info *types.GroupInfo, participants int) {
	fmt.Fprintf(w, "JID: %s\nName: %s\nOwner: %s\nType: %s\n",
		info.JID.String(),
		sanitize(info.GroupName.Name),
		info.OwnerJID.String(),
		groupKindLabel(info.IsParent, info.LinkedParentJID.String()),
	)
	if !info.LinkedParentJID.IsEmpty() {
		fmt.Fprintf(w, "Parent: %s\n", info.LinkedParentJID.String())
	}
	fmt.Fprintf(w, "Created: %s\nParticipants: %d\n",
		info.GroupCreated.Local().Format(time.RFC3339),
		participants,
	)
}

func newGroupsRenameCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	var name string
	cmd := &cobra.Command{
		Use:   "rename",
		Short: "Rename group",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" || strings.TrimSpace(name) == "" {
				return fmt.Errorf("--jid and --name are required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				return delegateAfterOpenFailure(ctx, flags, err, sendDelegateRequest{Kind: groupRenameKind, To: jidStr, Name: name}, func(resp sendDelegateResponse) error {
					return writeGroupRenamed(flags, resp.Chat, name)
				})
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}
			gjid, err := renameGroup(ctx, a, jidStr, name)
			if err != nil {
				return err
			}
			return writeGroupRenamed(flags, gjid.String(), name)
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "group JID (…@g.us)")
	cmd.Flags().StringVar(&name, "name", "", "new name")
	return cmd
}

func renameGroup(ctx context.Context, a waStoreApp, rawJID, name string) (types.JID, error) {
	gjid, err := types.ParseJID(rawJID)
	if err != nil {
		return types.JID{}, err
	}
	if err := a.WA().SetGroupName(ctx, gjid, name); err != nil {
		return types.JID{}, err
	}
	refreshGroupInfo(ctx, a, gjid)
	return gjid, nil
}

func writeGroupRenamed(flags *rootFlags, jid, name string) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{"jid": jid, "name": name})
	}
	fmt.Fprintln(os.Stdout, "OK")
	return nil
}

func newGroupsLeaveCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	cmd := &cobra.Command{
		Use:   "leave",
		Short: "Leave a group",
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
				return delegateAfterOpenFailure(ctx, flags, err, sendDelegateRequest{Kind: groupLeaveKind, To: jidStr}, func(resp sendDelegateResponse) error {
					return writeGroupLeft(flags, resp.Chat)
				})
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}
			gjid, err := leaveGroup(ctx, a, jidStr)
			if err != nil {
				return err
			}
			return writeGroupLeft(flags, gjid.String())
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "group JID (…@g.us)")
	return cmd
}

// leaveGroup leaves on WhatsApp, then marks the group left locally.
func leaveGroup(ctx context.Context, a waStoreApp, rawJID string) (types.JID, error) {
	gjid, err := types.ParseJID(rawJID)
	if err != nil {
		return types.JID{}, err
	}
	if err := a.WA().LeaveGroup(ctx, gjid); err != nil {
		return types.JID{}, err
	}
	_ = a.DB().MarkGroupLeft(gjid.String(), time.Now().UTC())
	return gjid, nil
}

func writeGroupLeft(flags *rootFlags, jid string) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{"jid": jid, "left": true})
	}
	fmt.Fprintln(os.Stdout, "OK")
	return nil
}
