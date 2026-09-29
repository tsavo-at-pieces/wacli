package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/types"
)

// Management kinds delegated to a same-store `sync --follow`. Every command
// has its own kind: a sync process that predates one rejects it as an
// unsupported kind before doing anything, instead of running something else.
const (
	chatArchiveKind   = "chat_archive"
	chatUnarchiveKind = "chat_unarchive"
	chatPinKind       = "chat_pin"
	chatUnpinKind     = "chat_unpin"
	chatMuteKind      = "chat_mute"
	chatUnmuteKind    = "chat_unmute"

	groupCreateKind       = "group_create"
	groupLeaveKind        = "group_leave"
	groupRenameKind       = "group_rename"
	groupJoinKind         = "group_join"
	groupInviteRevokeKind = "group_invite_link_revoke"
	groupsRefreshKind     = "groups_refresh"
	// groupParticipantsKindPrefix is followed by add, remove, promote or demote.
	groupParticipantsKindPrefix = "group_participants_"

	contactAliasSetKind = "contact_alias_set"
	contactAliasRmKind  = "contact_alias_rm"
	contactTagsAddKind  = "contact_tags_add"
	contactTagsRmKind   = "contact_tags_rm"
	contactsRefreshKind = "contacts_refresh"

	messageDeleteKind      = "message_delete"
	messageDeleteForMeKind = "message_delete_for_me"
	messageRevokeKind      = "message_revoke"
	messageForwardKind     = "message_forward"
)

// delegatedManagementApp is what the management kinds need from the running
// sync process: *app.App in production, a fake in tests.
type delegatedManagementApp interface {
	delegatedChatStateApp
	delegatedContactApp
	messageMutationApp
}

// executeDelegatedManagement runs a chat, group, contact or message management
// kind inside the sync process that owns the store.
func executeDelegatedManagement(ctx context.Context, a delegatedManagementApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	switch req.Kind {
	case chatArchiveKind, chatUnarchiveKind, chatPinKind, chatUnpinKind, chatMuteKind, chatUnmuteKind:
		return executeDelegatedChatState(ctx, a, req)
	case groupCreateKind:
		return executeDelegatedGroupCreate(ctx, a, req)
	case groupLeaveKind:
		return executeDelegatedGroupLeave(ctx, a, req)
	case groupRenameKind:
		return executeDelegatedGroupRename(ctx, a, req)
	case groupJoinKind:
		return executeDelegatedGroupJoin(ctx, a, req)
	case groupInviteRevokeKind:
		return executeDelegatedGroupInviteRevoke(ctx, a, req)
	case groupsRefreshKind:
		return executeDelegatedGroupsRefresh(ctx, a)
	case groupParticipantsKindPrefix + "add", groupParticipantsKindPrefix + "remove",
		groupParticipantsKindPrefix + "promote", groupParticipantsKindPrefix + "demote":
		return executeDelegatedGroupParticipants(ctx, a, req)
	case contactAliasSetKind, contactAliasRmKind, contactTagsAddKind, contactTagsRmKind:
		return executeDelegatedContactMetadata(ctx, a, req)
	case contactsRefreshKind:
		return executeDelegatedContactsRefresh(ctx, a)
	case messageDeleteKind, messageDeleteForMeKind:
		return executeDelegatedMessageDelete(ctx, a, req)
	case messageRevokeKind:
		return executeDelegatedMessageRevoke(ctx, a, req)
	case messageForwardKind:
		return executeDelegatedMessageForward(ctx, a, req)
	case statusSendKind, statusMuteKind, statusUnmuteKind, statusPrivacyKind,
		channelsListKind, channelInfoKind, channelJoinKind, channelLeaveKind,
		channelMuteKind, channelUnmuteKind, channelReactKind, channelMessagesKind,
		channelMarkViewedKind, channelCreateKind, callRejectKind:
		return executeDelegatedStatusChannelsCalls(ctx, a, req)
	default:
		if resp, ok, err := executeDelegatedGroupKind(ctx, a, req); ok {
			return resp, err
		}
		return sendDelegateResponse{}, fmt.Errorf("unsupported send kind %q", req.Kind)
	}
}

// delegateAfterOpenFailure hands req to a same-store `sync --follow` when
// openErr is that process's store lock, then prints the result with write.
// Without a running sync process it returns openErr unchanged.
func delegateAfterOpenFailure(ctx context.Context, flags *rootFlags, openErr error, req sendDelegateRequest, write func(sendDelegateResponse) error) error {
	resp, delegated, err := tryDelegateSend(ctx, flags, openErr, req)
	if !delegated {
		return err
	}
	if err != nil {
		return explainUnsupportedDelegateKind(err, req.Kind)
	}
	return write(resp)
}

// explainUnsupportedDelegateKind turns the rejection from a sync process that
// predates a kind into the action to take. Unknown kinds are rejected before
// anything runs, so nothing changed.
func explainUnsupportedDelegateKind(err error, kind string) error {
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("unsupported send kind %q", kind)) {
		return err
	}
	return fmt.Errorf("the running sync process does not support this command and did not run it; restart `wacli sync` after upgrading, then run this again: %w", err)
}

type delegatedChatStateApp interface {
	recipientResolverApp
	ArchiveChat(context.Context, types.JID, bool) error
	PinChat(context.Context, types.JID, bool) error
	MuteChat(context.Context, types.JID, bool, time.Duration) error
}

func executeDelegatedChatState(ctx context.Context, a delegatedChatStateApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	var apply func(types.JID) error
	switch req.Kind {
	case chatArchiveKind:
		apply = func(jid types.JID) error { return a.ArchiveChat(ctx, jid, true) }
	case chatUnarchiveKind:
		apply = func(jid types.JID) error { return a.ArchiveChat(ctx, jid, false) }
	case chatPinKind:
		apply = func(jid types.JID) error { return a.PinChat(ctx, jid, true) }
	case chatUnpinKind:
		apply = func(jid types.JID) error { return a.PinChat(ctx, jid, false) }
	case chatMuteKind:
		apply = func(jid types.JID) error { return a.MuteChat(ctx, jid, true, time.Duration(req.MuteDurationNS)) }
	case chatUnmuteKind:
		apply = func(jid types.JID) error { return a.MuteChat(ctx, jid, false, 0) }
	default:
		return sendDelegateResponse{}, fmt.Errorf("unsupported send kind %q", req.Kind)
	}
	// The daemon cannot prompt, so an ambiguous name fails as with --json.
	jid, err := resolveRecipient(a, req.To, recipientOptions{pick: req.Pick, asJSON: true})
	if err != nil {
		return sendDelegateResponse{}, err
	}
	if err := apply(jid); err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Chat: jid.String(), Action: strings.TrimPrefix(req.Kind, "chat_")}, nil
}

func executeDelegatedGroupCreate(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	info, err := createGroup(ctx, a, groupCreateOptions{
		name:         req.Name,
		users:        req.Users,
		announceOnly: req.AnnounceOnly,
		locked:       req.Locked,
		joinApproval: req.JoinApproval,
		community:    req.Community,
		linkedParent: req.LinkedParent,
	})
	if err != nil {
		return sendDelegateResponse{}, err
	}
	raw, err := json.Marshal(info)
	if err != nil {
		return sendDelegateResponse{}, fmt.Errorf("encode created group: %w", err)
	}
	resp := sendDelegateResponse{OK: true, Group: raw}
	if info != nil {
		resp.Chat = info.JID.String()
		resp.Name = info.GroupName.Name
	}
	return resp, nil
}

func executeDelegatedGroupLeave(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	gjid, err := leaveGroup(ctx, a, req.To)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Chat: gjid.String()}, nil
}

func executeDelegatedGroupRename(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	gjid, err := renameGroup(ctx, a, req.To, req.Name)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Chat: gjid.String()}, nil
}

func executeDelegatedGroupJoin(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	gjid, err := joinGroup(ctx, a, req.InviteCode)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Chat: gjid.String()}, nil
}

func executeDelegatedGroupInviteRevoke(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	gjid, link, err := revokeGroupInviteLink(ctx, a, req.To)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Chat: gjid.String(), Link: link}, nil
}

func executeDelegatedGroupsRefresh(ctx context.Context, a waStoreApp) (sendDelegateResponse, error) {
	n, err := refreshJoinedGroups(ctx, a)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Count: n}, nil
}

func executeDelegatedGroupParticipants(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	action := strings.TrimPrefix(req.Kind, groupParticipantsKindPrefix)
	updated, err := changeGroupParticipants(ctx, a, req.To, req.Users, action)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	raw, err := json.Marshal(updated)
	if err != nil {
		return sendDelegateResponse{}, fmt.Errorf("encode participant results: %w", err)
	}
	return sendDelegateResponse{OK: true, Participants: raw}, nil
}

type delegatedContactApp interface {
	DB() *store.DB
	OpenSessionResolver() (app.SessionResolver, error)
}

func executeDelegatedContactMetadata(ctx context.Context, a delegatedContactApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	// A fresh read-only view gives the same identity pairs as the direct
	// command, without the live client's network lookups.
	resolver, err := a.OpenSessionResolver()
	if err != nil {
		return sendDelegateResponse{}, err
	}
	defer resolver.Close()
	if err := applyContactMetadata(a.DB(), req, contactIdentityJIDs(ctx, resolver, req.To)); err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true}, nil
}

func executeDelegatedContactsRefresh(ctx context.Context, a waStoreApp) (sendDelegateResponse, error) {
	n, err := importSessionContacts(ctx, a)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Count: n}, nil
}

func executeDelegatedMessageDelete(ctx context.Context, a messageMutationApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	forMe := req.Kind == messageDeleteForMeKind
	if req.DeleteMedia && !forMe {
		return sendDelegateResponse{}, fmt.Errorf("--delete-media requires --for-me")
	}
	msg, chatJID, err := loadMessageDeleteTarget(ctx, a, req.To, req.ID, forMe)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	res, err := deleteStoredMessage(ctx, a, msg, chatJID, forMe, req.DeleteMedia)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	waitForPostSendRetryReceipts(ctx, millisDuration(req.PostSendWaitMS, 0))
	return sendDelegateResponse{OK: true, Sent: true, To: res.chat, ID: res.id, Target: res.target, DeletedMedia: res.deletedMedia}, nil
}

func executeDelegatedMessageRevoke(ctx context.Context, a messageMutationApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	target, err := loadMessageRevokeTarget(ctx, a, req.To, req.ID)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	sentID, err := revokeMessage(ctx, a, target, req.ID)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	waitForPostSendRetryReceipts(ctx, millisDuration(req.PostSendWaitMS, 0))
	return sendDelegateResponse{OK: true, Sent: true, To: target.chat.String(), ID: string(sentID), Target: req.ID}, nil
}

func executeDelegatedMessageForward(ctx context.Context, a messageMutationApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	plan, err := planMessageForward(ctx, a, req.Chat, req.ID, req.To, recipientOptions{pick: req.Pick, asJSON: true})
	if err != nil {
		return sendDelegateResponse{}, err
	}
	res, err := forwardStoredMessage(ctx, a, plan)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	waitForPostSendRetryReceipts(ctx, millisDuration(req.PostSendWaitMS, 0))
	return sendDelegateResponse{OK: true, Sent: true, To: res.to, ID: res.id, Target: res.source, StoreWarning: res.storeWarning}, nil
}
