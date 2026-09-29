package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// Group and community kinds delegated to a same-store `sync --follow`, beside
// the ones in send_ipc_manage.go. Every command has its own kind, so a sync
// process that predates one rejects it before doing anything.
const (
	groupInfoKind            = "group_info"
	groupTopicKind           = "group_topic"
	groupAnnounceOnlyKind    = "group_announce_only"
	groupLockedKind          = "group_locked"
	groupJoinApprovalKind    = "group_join_approval"
	groupMemberAddModeKind   = "group_member_add_mode"
	groupPhotoSetKind        = "group_photo_set"
	groupPhotoRemoveKind     = "group_photo_remove"
	groupRequestsListKind    = "group_requests_list"
	groupRequestsApproveKind = "group_requests_approve"
	groupRequestsRejectKind  = "group_requests_reject"
	groupInviteLinkGetKind   = "group_invite_link_get"
	groupInviteInfoKind      = "group_invite_info"
	groupsPruneKind          = "groups_prune"

	communitySubgroupsKind    = "community_subgroups"
	communityLinkKind         = "community_link"
	communityUnlinkKind       = "community_unlink"
	communityParticipantsKind = "community_participants"
)

type groupKindExecutor func(context.Context, waStoreApp, sendDelegateRequest) (sendDelegateResponse, error)

// groupKindExecutors hold the command core for each kind. The sync process
// that owns the store and a direct run call the same function and print its
// response the same way, so the two paths cannot drift apart.
var groupKindExecutors = map[string]groupKindExecutor{
	groupInfoKind:            executeGroupInfo,
	groupTopicKind:           executeGroupTopic,
	groupAnnounceOnlyKind:    executeGroupToggle,
	groupLockedKind:          executeGroupToggle,
	groupJoinApprovalKind:    executeGroupToggle,
	groupMemberAddModeKind:   executeGroupMemberAddMode,
	groupPhotoSetKind:        executeGroupPhotoSet,
	groupPhotoRemoveKind:     executeGroupPhotoRemove,
	groupRequestsListKind:    executeGroupRequestsList,
	groupRequestsApproveKind: executeGroupRequestsAction,
	groupRequestsRejectKind:  executeGroupRequestsAction,
	groupInviteLinkGetKind:   executeGroupInviteLinkGet,
	groupInviteInfoKind:      executeGroupInviteInfo,
	groupsPruneKind:          executeGroupsPrune,

	communitySubgroupsKind:    executeCommunitySubgroups,
	communityLinkKind:         executeCommunityLink,
	communityUnlinkKind:       executeCommunityLink,
	communityParticipantsKind: executeCommunityParticipants,
}

// executeDelegatedGroupKind runs one of the kinds above. ok is false for any
// other kind, which the caller rejects as unsupported.
func executeDelegatedGroupKind(ctx context.Context, a waStoreApp, req sendDelegateRequest) (resp sendDelegateResponse, ok bool, err error) {
	run, ok := groupKindExecutors[req.Kind]
	if !ok {
		return sendDelegateResponse{}, false, nil
	}
	resp, err = run(ctx, a, req)
	return resp, true, err
}

// runLiveGroupCommand runs req's kind against WhatsApp: in this process when
// it gets the store lock, otherwise in the same-store `sync --follow` that
// holds it. Callers check read-only mode first.
func runLiveGroupCommand(flags *rootFlags, req sendDelegateRequest, write func(sendDelegateResponse) error) error {
	ctx, cancel := withTimeout(context.Background(), flags)
	defer cancel()

	a, lk, err := newApp(ctx, flags, true, false)
	if err != nil {
		return delegateAfterOpenFailure(ctx, flags, err, req, write)
	}
	defer closeApp(a, lk)

	if err := a.EnsureAuthed(ctx); err != nil {
		return err
	}
	if err := a.Connect(ctx, false, nil); err != nil {
		return err
	}
	resp, ok, err := executeDelegatedGroupKind(ctx, a, req)
	if !ok {
		return fmt.Errorf("unsupported send kind %q", req.Kind)
	}
	if err != nil {
		return err
	}
	return write(resp)
}

// requireLiveRead stops a command that only reads from WhatsApp in read-only
// mode. Read-only mode never connects, and a running sync process must not
// connect on its behalf either, so the check comes before both paths.
func requireLiveRead(flags *rootFlags) error {
	if flags.isReadOnly() {
		return fmt.Errorf("read-only mode: command would connect to WhatsApp")
	}
	return nil
}

// encodeGroupResult encodes a command result for sendDelegateResponse.
func encodeGroupResult(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode result: %w", err)
	}
	return raw, nil
}

// decodeGroupResult decodes an encoded result for human output. A missing
// result decodes as JSON null.
func decodeGroupResult(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("decode result: %w", err)
	}
	return nil
}

// groupResultJSON is what --json prints for an encoded result: the value as
// the direct command encoded it, or null when there is none.
func groupResultJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}
