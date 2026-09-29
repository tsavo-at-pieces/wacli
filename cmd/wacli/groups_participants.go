package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

func newGroupsParticipantsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "participants",
		Short: "Manage group participants",
	}
	cmd.AddCommand(newGroupsParticipantsListCmd(flags))
	cmd.AddCommand(newGroupsParticipantsActionCmd(flags, "add"))
	cmd.AddCommand(newGroupsParticipantsActionCmd(flags, "remove"))
	cmd.AddCommand(newGroupsParticipantsActionCmd(flags, "promote"))
	cmd.AddCommand(newGroupsParticipantsActionCmd(flags, "demote"))
	return cmd
}

func newGroupsParticipantsListCmd(flags *rootFlags) *cobra.Command {
	var group string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List participants (local snapshot; sync --refresh-groups to update)",
		Long: `List participants from the last group snapshot saved in the local database.

This command does not connect to WhatsApp. The snapshot can be empty or stale.
Run "wacli sync --once --refresh-groups" to fetch joined groups and replace
their local participant snapshots.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(group) == "" {
				return fmt.Errorf("--jid is required")
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, false, false)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			participants, err := a.DB().ListGroupParticipants(group)
			if err != nil {
				return err
			}

			if flags.asJSON {
				return out.WriteJSON(os.Stdout, participants)
			}

			w := newTableWriter(os.Stdout)
			fmt.Fprintln(w, "USER JID\tROLE\tUPDATED")
			for _, p := range participants {
				updated := "-"
				if !p.UpdatedAt.IsZero() {
					updated = p.UpdatedAt.Local().Format("2006-01-02 15:04:05")
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", p.UserJID, p.Role, updated)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&group, "jid", "", "group JID (…@g.us)")
	return cmd
}

func newGroupsParticipantsActionCmd(flags *rootFlags, action string) *cobra.Command {
	var group string
	var users []string
	cmd := &cobra.Command{
		Use:   action,
		Short: action + " participants",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(group) == "" || len(users) == 0 {
				return fmt.Errorf("--jid and at least one --user are required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				return delegateAfterOpenFailure(ctx, flags, err, sendDelegateRequest{
					Kind:  groupParticipantsKindPrefix + action,
					To:    group,
					Users: users,
				}, func(resp sendDelegateResponse) error {
					return writeGroupParticipantsChanged(flags, resp.Participants)
				})
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}
			updated, err := changeGroupParticipants(ctx, a, group, users, action)
			if err != nil {
				return err
			}
			return writeGroupParticipantsChanged(flags, updated)
		},
	}
	cmd.Flags().StringVar(&group, "jid", "", "group JID (…@g.us)")
	cmd.Flags().StringSliceVar(&users, "user", nil, "user phone number (+E164 and formatting ok) or JID (repeatable)")
	return cmd
}

// changeGroupParticipants applies one participant action and stores the
// group's live info. It returns WhatsApp's per-participant results.
func changeGroupParticipants(ctx context.Context, a waStoreApp, rawJID string, users []string, action string) ([]types.GroupParticipant, error) {
	gjid, err := types.ParseJID(rawJID)
	if err != nil {
		return nil, err
	}
	var jids []types.JID
	for _, u := range users {
		j, err := wa.ParseUserOrJID(u)
		if err != nil {
			return nil, err
		}
		jids = append(jids, j)
	}
	updated, err := a.WA().UpdateGroupParticipants(ctx, gjid, jids, wa.GroupParticipantAction(action))
	if err != nil {
		return nil, err
	}
	refreshGroupInfo(ctx, a, gjid)
	return updated, nil
}

// writeGroupParticipantsChanged prints participant results: the whatsmeow
// slice when run directly, or its JSON encoding from a sync process.
func writeGroupParticipantsChanged(flags *rootFlags, updated any) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, updated)
	}
	fmt.Fprintln(os.Stdout, "OK")
	return nil
}
