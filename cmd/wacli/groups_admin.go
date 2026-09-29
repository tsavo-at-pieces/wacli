package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	appcore "github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

func newGroupsCreateCmd(flags *rootFlags) *cobra.Command {
	var opts groupCreateOptions
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a group",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(opts.name) == "" {
				return fmt.Errorf("--name is required")
			}
			if opts.community && strings.TrimSpace(opts.linkedParent) != "" {
				return fmt.Errorf("--community and --linked-parent cannot be combined")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				return delegateAfterOpenFailure(ctx, flags, err, sendDelegateRequest{
					Kind:         groupCreateKind,
					Name:         opts.name,
					Users:        opts.users,
					AnnounceOnly: opts.announceOnly,
					Locked:       opts.locked,
					JoinApproval: opts.joinApproval,
					Community:    opts.community,
					LinkedParent: opts.linkedParent,
				}, func(resp sendDelegateResponse) error {
					return writeGroupCreated(flags, resp.Group, resp.Chat, resp.Name)
				})
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}
			info, err := createGroup(ctx, a, opts)
			if err != nil {
				return err
			}
			if info == nil {
				return writeGroupCreated(flags, info, "", "")
			}
			return writeGroupCreated(flags, info, info.JID.String(), info.GroupName.Name)
		},
	}
	cmd.Flags().StringVar(&opts.name, "name", "", "group name")
	cmd.Flags().StringSliceVar(&opts.users, "user", nil, "initial participant phone number (+E164 and formatting ok) or JID (repeatable)")
	cmd.Flags().BoolVar(&opts.announceOnly, "announce-only", false, "only admins can send messages")
	cmd.Flags().BoolVar(&opts.locked, "locked", false, "only admins can edit group info")
	cmd.Flags().BoolVar(&opts.joinApproval, "join-approval", false, "require admin approval for new join requests")
	cmd.Flags().BoolVar(&opts.community, "community", false, "create a community parent group")
	cmd.Flags().StringVar(&opts.linkedParent, "linked-parent", "", "community parent group JID for a new subgroup")
	return cmd
}

type groupCreateOptions struct {
	name         string
	users        []string
	announceOnly bool
	locked       bool
	joinApproval bool
	community    bool
	linkedParent string
}

// createGroup creates the group on WhatsApp and stores its live info.
func createGroup(ctx context.Context, a waStoreApp, opts groupCreateOptions) (*types.GroupInfo, error) {
	participants, err := parseGroupUserJIDs(opts.users)
	if err != nil {
		return nil, err
	}
	var parentJID types.JID
	if strings.TrimSpace(opts.linkedParent) != "" {
		parentJID, err = parseGroupJID(opts.linkedParent)
		if err != nil {
			return nil, fmt.Errorf("parse --linked-parent: %w", err)
		}
	}
	info, err := a.WA().CreateGroup(ctx, wa.CreateGroupRequest{
		Name:                   opts.name,
		Participants:           participants,
		IsAnnounce:             opts.announceOnly,
		IsLocked:               opts.locked,
		IsJoinApprovalRequired: opts.joinApproval,
		IsParent:               opts.community,
		LinkedParentJID:        parentJID,
	})
	if err != nil {
		return nil, err
	}
	if info != nil {
		_ = persistGroupInfo(ctx, a.DB(), a.WA(), info)
	}
	return info, nil
}

// writeGroupCreated prints the created group. info is the whatsmeow result
// when run directly, or its JSON encoding from a sync process; jid is empty
// when WhatsApp returned no group info.
func writeGroupCreated(flags *rootFlags, info any, jid, name string) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, info)
	}
	if jid == "" {
		fmt.Fprintln(os.Stdout, "OK")
		return nil
	}
	fmt.Fprintf(os.Stdout, "JID: %s\nName: %s\n", jid, sanitize(name))
	return nil
}

func newGroupsTopicCmd(flags *rootFlags, use string) *cobra.Command {
	var jidStr string
	var text string
	cmd := &cobra.Command{
		Use:   use,
		Short: "Set group topic/description",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" || !cmd.Flags().Changed("text") {
				return fmt.Errorf("--jid and --text are required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			if _, err := parseGroupJID(jidStr); err != nil {
				return err
			}
			return runLiveGroupCommand(flags, sendDelegateRequest{Kind: groupTopicKind, To: jidStr, Topic: text}, func(resp sendDelegateResponse) error {
				if flags.asJSON {
					return out.WriteJSON(os.Stdout, map[string]any{"jid": resp.Chat, "topic": text})
				}
				fmt.Fprintln(os.Stdout, "OK")
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "group JID (…@g.us)")
	cmd.Flags().StringVar(&text, "text", "", "new topic/description text (empty string clears it)")
	return cmd
}

// executeGroupTopic replaces the group description. WhatsApp rejects a change
// whose previous-description ID is not the current one, so it reads the live
// group first and names that ID.
func executeGroupTopic(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	gjid, err := parseGroupJID(req.To)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	current, err := a.WA().GetGroupInfo(ctx, gjid)
	if err != nil {
		return sendDelegateResponse{}, fmt.Errorf("read current group description: %w", err)
	}
	if current == nil {
		return sendDelegateResponse{}, fmt.Errorf("group info not found for %s", gjid.String())
	}
	if err := a.WA().SetGroupTopic(ctx, gjid, current.TopicID, req.Topic); err != nil {
		return sendDelegateResponse{}, err
	}
	refreshGroupInfo(ctx, a, gjid)
	return sendDelegateResponse{OK: true, Chat: gjid.String()}, nil
}

// groupToggle is an on/off group setting.
type groupToggle struct {
	use, short, jsonKey string
	apply               func(context.Context, appcore.WAClient, types.JID, bool) error
}

// groupToggles maps each toggle's kind to its setting.
var groupToggles = map[string]groupToggle{
	groupAnnounceOnlyKind: {
		use: "announce-only", short: "Set whether only admins can send messages", jsonKey: "announce_only",
		apply: func(ctx context.Context, client appcore.WAClient, jid types.JID, on bool) error {
			return client.SetGroupAnnounce(ctx, jid, on)
		},
	},
	groupLockedKind: {
		use: "locked", short: "Set whether only admins can edit group info", jsonKey: "locked",
		apply: func(ctx context.Context, client appcore.WAClient, jid types.JID, on bool) error {
			return client.SetGroupLocked(ctx, jid, on)
		},
	},
}

func newGroupsAnnounceOnlyCmd(flags *rootFlags) *cobra.Command {
	return newGroupsToggleCmd(flags, groupAnnounceOnlyKind)
}

func newGroupsLockedCmd(flags *rootFlags) *cobra.Command {
	return newGroupsToggleCmd(flags, groupLockedKind)
}

func newGroupsToggleCmd(flags *rootFlags, kind string) *cobra.Command {
	toggle := groupToggles[kind]
	var jidStr string
	var on bool
	var off bool
	cmd := &cobra.Command{
		Use:   toggle.use,
		Short: toggle.short,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" {
				return fmt.Errorf("--jid is required")
			}
			enabled, err := parseOnOffFlags(cmd, on, off)
			if err != nil {
				return err
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			if _, err := parseGroupJID(jidStr); err != nil {
				return err
			}
			return runLiveGroupCommand(flags, sendDelegateRequest{Kind: kind, To: jidStr, Enabled: enabled}, func(resp sendDelegateResponse) error {
				if flags.asJSON {
					return out.WriteJSON(os.Stdout, map[string]any{"jid": resp.Chat, toggle.jsonKey: enabled})
				}
				fmt.Fprintln(os.Stdout, "OK")
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "group JID (…@g.us)")
	cmd.Flags().BoolVar(&on, "on", false, "enable setting")
	cmd.Flags().BoolVar(&off, "off", false, "disable setting")
	return cmd
}

// executeGroupToggle turns the kind's setting on or off (req.Enabled).
func executeGroupToggle(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	toggle, ok := groupToggles[req.Kind]
	if !ok {
		return sendDelegateResponse{}, fmt.Errorf("unsupported send kind %q", req.Kind)
	}
	gjid, err := parseGroupJID(req.To)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	if err := toggle.apply(ctx, a.WA(), gjid, req.Enabled); err != nil {
		return sendDelegateResponse{}, err
	}
	refreshGroupInfo(ctx, a, gjid)
	return sendDelegateResponse{OK: true, Chat: gjid.String()}, nil
}

func newGroupsRequestsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "requests",
		Short: "Manage group join requests",
	}
	cmd.AddCommand(newGroupsRequestsListCmd(flags))
	cmd.AddCommand(newGroupsRequestsActionCmd(flags, "approve"))
	cmd.AddCommand(newGroupsRequestsActionCmd(flags, "reject"))
	return cmd
}

// requestListEntry is the JSON/text output shape for a single pending join request.
type requestListEntry struct {
	JID         string    `json:"JID"`
	PhoneNumber string    `json:"phone_number,omitempty"`
	RequestedAt time.Time `json:"RequestedAt"`
}

// resolveRequestEntries converts raw join-request participants to output entries,
// resolving LID JIDs to phone numbers via the provided resolver function.
func resolveRequestEntries(ctx context.Context, requests []types.GroupParticipantRequest, resolve func(context.Context, types.JID) types.JID) []requestListEntry {
	entries := make([]requestListEntry, len(requests))
	for i, req := range requests {
		pn := resolve(ctx, req.JID)
		phone := ""
		if pn.Server == types.DefaultUserServer {
			phone = "+" + pn.User
		}
		entries[i] = requestListEntry{
			JID:         req.JID.String(),
			PhoneNumber: phone,
			RequestedAt: req.RequestedAt,
		}
	}
	return entries
}

func newGroupsRequestsListCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List pending group join requests",
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
			return runLiveGroupCommand(flags, sendDelegateRequest{Kind: groupRequestsListKind, To: jidStr}, func(resp sendDelegateResponse) error {
				if flags.asJSON {
					return out.WriteJSON(os.Stdout, groupResultJSON(resp.Result))
				}
				var entries []requestListEntry
				if err := decodeGroupResult(resp.Result, &entries); err != nil {
					return err
				}
				for _, e := range entries {
					fmt.Fprintf(os.Stdout, "%s\t%s\t%s\n", e.JID, e.RequestedAt.Local().Format("2006-01-02 15:04:05"), e.PhoneNumber)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "group JID (…@g.us)")
	return cmd
}

// executeGroupRequestsList lists pending join requests, with phone numbers
// for requesters whose LID the session can resolve.
func executeGroupRequestsList(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	gjid, err := parseGroupJID(req.To)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	requests, err := a.WA().GetGroupRequestParticipants(ctx, gjid)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	raw, err := encodeGroupResult(resolveRequestEntries(ctx, requests, a.WA().ResolveLIDToPN))
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Chat: gjid.String(), Result: raw}, nil
}

func newGroupsRequestsActionCmd(flags *rootFlags, action string) *cobra.Command {
	kind := groupRequestsApproveKind
	if action == "reject" {
		kind = groupRequestsRejectKind
	}
	var jidStr string
	var users []string
	cmd := &cobra.Command{
		Use:   action,
		Short: action + " group join requests",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" || len(users) == 0 {
				return fmt.Errorf("--jid and at least one --user are required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			if _, err := parseGroupJID(jidStr); err != nil {
				return err
			}
			if _, err := parseGroupUserJIDs(users); err != nil {
				return err
			}
			return runLiveGroupCommand(flags, sendDelegateRequest{Kind: kind, To: jidStr, Users: users}, func(resp sendDelegateResponse) error {
				if flags.asJSON {
					return out.WriteJSON(os.Stdout, groupResultJSON(resp.Participants))
				}
				fmt.Fprintln(os.Stdout, "OK")
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "group JID (…@g.us)")
	cmd.Flags().StringSliceVar(&users, "user", nil, "requesting user phone number (+E164 and formatting ok) or JID (repeatable)")
	return cmd
}

// executeGroupRequestsAction approves or rejects join requests and returns
// WhatsApp's per-user results.
func executeGroupRequestsAction(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	action := wa.GroupParticipantRequestApprove
	if req.Kind == groupRequestsRejectKind {
		action = wa.GroupParticipantRequestReject
	}
	gjid, err := parseGroupJID(req.To)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	jids, err := parseGroupUserJIDs(req.Users)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	updated, err := a.WA().UpdateGroupRequestParticipants(ctx, gjid, jids, action)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	refreshGroupInfo(ctx, a, gjid)
	raw, err := encodeGroupResult(updated)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Chat: gjid.String(), Participants: raw}, nil
}

func parseOnOffFlags(cmd *cobra.Command, on, off bool) (bool, error) {
	onChanged := cmd.Flags().Changed("on")
	offChanged := cmd.Flags().Changed("off")
	if onChanged == offChanged {
		return false, fmt.Errorf("exactly one of --on or --off is required")
	}
	if onChanged {
		if !on {
			return false, fmt.Errorf("--on=false does not select a mode; use --off to disable")
		}
		return true, nil
	}
	if !off {
		return false, fmt.Errorf("--off=false does not select a mode; use --on to enable")
	}
	return false, nil
}

func parseGroupJID(raw string) (types.JID, error) {
	jid, err := types.ParseJID(strings.TrimSpace(raw))
	if err != nil {
		return types.JID{}, err
	}
	if jid.Server != types.GroupServer {
		return types.JID{}, fmt.Errorf("expected group JID ending in @g.us, got %s", jid.String())
	}
	return jid, nil
}

func parseGroupUserJIDs(users []string) ([]types.JID, error) {
	jids := make([]types.JID, 0, len(users))
	for _, user := range users {
		jid, err := wa.ParseUserOrJID(user)
		if err != nil {
			return nil, err
		}
		jids = append(jids, jid)
	}
	return jids, nil
}
