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

func newGroupsCommunityCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "community",
		Short: "Manage WhatsApp Communities (parent groups and their linked groups)",
	}
	cmd.AddCommand(newCommunitySubgroupsCmd(flags))
	cmd.AddCommand(newCommunityParticipantsCmd(flags))
	cmd.AddCommand(newCommunityLinkCmd(flags, "link"))
	cmd.AddCommand(newCommunityLinkCmd(flags, "unlink"))
	return cmd
}

func newCommunitySubgroupsCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	cmd := &cobra.Command{
		Use:   "subgroups",
		Short: "List the groups linked to a community (live)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" {
				return fmt.Errorf("--jid is required")
			}
			if err := requireLiveRead(flags); err != nil {
				return err
			}
			if _, err := parseGroupJID(jidStr); err != nil {
				return err
			}
			return runLiveGroupCommand(flags, sendDelegateRequest{Kind: communitySubgroupsKind, To: jidStr}, func(resp sendDelegateResponse) error {
				return writeCommunitySubgroups(flags, resp)
			})
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "community (parent group) JID (…@g.us)")
	return cmd
}

// executeCommunitySubgroups lists a community's linked groups as whatsmeow
// reports them.
func executeCommunitySubgroups(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	parent, err := parseGroupJID(req.To)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	subgroups, err := a.WA().GetSubGroups(ctx, parent)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	if subgroups == nil {
		subgroups = []*types.GroupLinkTarget{}
	}
	raw, err := encodeGroupResult(subgroups)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Chat: parent.String(), Result: raw}, nil
}

func writeCommunitySubgroups(flags *rootFlags, resp sendDelegateResponse) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, groupResultJSON(resp.Result))
	}
	var subgroups []*types.GroupLinkTarget
	if err := decodeGroupResult(resp.Result, &subgroups); err != nil {
		return err
	}
	fullOutput := fullTableOutput(flags.fullOutput)
	w := newTableWriter(os.Stdout)
	fmt.Fprintln(w, "NAME\tJID\tDEFAULT")
	for _, g := range subgroups {
		if g == nil {
			continue
		}
		name := g.GroupName.Name
		if name == "" {
			name = g.JID.String()
		}
		isDefault := "-"
		if g.IsDefaultSubGroup {
			isDefault = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", tableCell(name, 40, fullOutput), g.JID.String(), isDefault)
	}
	return w.Flush()
}

// communityParticipantEntry is one community member, with a phone number
// when the session can resolve the member's LID.
type communityParticipantEntry struct {
	JID         string `json:"jid"`
	PhoneNumber string `json:"phone_number,omitempty"`
}

func newCommunityParticipantsCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	cmd := &cobra.Command{
		Use:   "participants",
		Short: "List the participants across a community's linked groups (live)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" {
				return fmt.Errorf("--jid is required")
			}
			if err := requireLiveRead(flags); err != nil {
				return err
			}
			if _, err := parseGroupJID(jidStr); err != nil {
				return err
			}
			return runLiveGroupCommand(flags, sendDelegateRequest{Kind: communityParticipantsKind, To: jidStr}, func(resp sendDelegateResponse) error {
				if flags.asJSON {
					return out.WriteJSON(os.Stdout, groupResultJSON(resp.Result))
				}
				var entries []communityParticipantEntry
				if err := decodeGroupResult(resp.Result, &entries); err != nil {
					return err
				}
				w := newTableWriter(os.Stdout)
				fmt.Fprintln(w, "JID\tPHONE")
				for _, e := range entries {
					phone := e.PhoneNumber
					if phone == "" {
						phone = "-"
					}
					fmt.Fprintf(w, "%s\t%s\n", e.JID, phone)
				}
				return w.Flush()
			})
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "community (parent group) JID (…@g.us)")
	return cmd
}

// executeCommunityParticipants lists the members across a community's linked
// groups.
func executeCommunityParticipants(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	parent, err := parseGroupJID(req.To)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	members, err := a.WA().GetLinkedGroupsParticipants(ctx, parent)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	entries := make([]communityParticipantEntry, 0, len(members))
	for _, m := range members {
		entry := communityParticipantEntry{JID: m.String()}
		if pn := a.WA().ResolveLIDToPN(ctx, m); pn.Server == types.DefaultUserServer {
			entry.PhoneNumber = "+" + pn.User
		}
		entries = append(entries, entry)
	}
	raw, err := encodeGroupResult(entries)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Chat: parent.String(), Result: raw}, nil
}

func newCommunityLinkCmd(flags *rootFlags, action string) *cobra.Command {
	kind, short, jsonKey := communityLinkKind, "Link an existing group into a community", "linked"
	if action == "unlink" {
		kind, short, jsonKey = communityUnlinkKind, "Remove a linked group from a community (the group itself stays)", "unlinked"
	}
	var parentStr, childStr string
	cmd := &cobra.Command{
		Use:   action,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(parentStr) == "" || strings.TrimSpace(childStr) == "" {
				return fmt.Errorf("--parent and --child are required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			if _, _, err := parseCommunityLink(parentStr, childStr); err != nil {
				return err
			}
			return runLiveGroupCommand(flags, sendDelegateRequest{Kind: kind, To: childStr, LinkedParent: parentStr}, func(resp sendDelegateResponse) error {
				if flags.asJSON {
					return out.WriteJSON(os.Stdout, map[string]any{"parent": resp.Target, "child": resp.Chat, jsonKey: true})
				}
				fmt.Fprintln(os.Stdout, "OK")
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&parentStr, "parent", "", "community (parent group) JID (…@g.us)")
	cmd.Flags().StringVar(&childStr, "child", "", "group JID to "+action+" (…@g.us)")
	return cmd
}

func parseCommunityLink(rawParent, rawChild string) (types.JID, types.JID, error) {
	parent, err := parseGroupJID(rawParent)
	if err != nil {
		return types.JID{}, types.JID{}, fmt.Errorf("parse --parent: %w", err)
	}
	child, err := parseGroupJID(rawChild)
	if err != nil {
		return types.JID{}, types.JID{}, fmt.Errorf("parse --child: %w", err)
	}
	if parent == child {
		return types.JID{}, types.JID{}, fmt.Errorf("--parent and --child must be different groups")
	}
	return parent, child, nil
}

// executeCommunityLink links or unlinks req.To (the child) and the community
// req.LinkedParent, then stores the child's live info so its local parent
// matches.
func executeCommunityLink(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	parent, child, err := parseCommunityLink(req.LinkedParent, req.To)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	if req.Kind == communityUnlinkKind {
		err = a.WA().UnlinkGroup(ctx, parent, child)
	} else {
		err = a.WA().LinkGroup(ctx, parent, child)
	}
	if err != nil {
		return sendDelegateResponse{}, err
	}
	refreshGroupInfo(ctx, a, child)
	return sendDelegateResponse{OK: true, Chat: child.String(), Target: parent.String()}, nil
}
