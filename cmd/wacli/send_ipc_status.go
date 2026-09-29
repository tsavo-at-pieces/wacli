package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/openclaw/wacli/internal/app"
	"go.mau.fi/whatsmeow/types"
)

// Status, channel and call kinds delegated to a same-store `sync --follow`.
// As with the management kinds, each command has its own kind so an older
// sync process rejects it before doing anything.
const (
	statusSendKind    = "status_send"
	statusMuteKind    = "status_mute"
	statusUnmuteKind  = "status_unmute"
	statusPrivacyKind = "status_privacy"

	channelsListKind      = "channels_list"
	channelInfoKind       = "channel_info"
	channelJoinKind       = "channel_join"
	channelLeaveKind      = "channel_leave"
	channelMuteKind       = "channel_mute"
	channelUnmuteKind     = "channel_unmute"
	channelReactKind      = "channel_react"
	channelMessagesKind   = "channel_messages"
	channelMarkViewedKind = "channel_mark_viewed"
	channelCreateKind     = "channel_create"

	callRejectKind = "call_reject"
)

// delegatedStatusChannelsApp is what the status, channel and call kinds need
// from the running sync process: *app.App in production, a fake in tests.
type delegatedStatusChannelsApp interface {
	statusSendApp
	statusMuteApp
}

var _ delegatedStatusChannelsApp = (*app.App)(nil)

// executeDelegatedStatusChannelsCalls runs a status, channel or call kind
// inside the sync process that owns the store.
func executeDelegatedStatusChannelsCalls(ctx context.Context, m delegatedManagementApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	a, ok := m.(delegatedStatusChannelsApp)
	if !ok {
		return sendDelegateResponse{}, fmt.Errorf("unsupported send kind %q", req.Kind)
	}
	switch req.Kind {
	case statusSendKind:
		return executeDelegatedStatusSend(ctx, a, req)
	case statusMuteKind, statusUnmuteKind:
		res, err := setContactStatusMute(ctx, a, req.To, recipientOptions{pick: req.Pick, asJSON: true}, req.Kind == statusMuteKind)
		if err != nil {
			return sendDelegateResponse{}, err
		}
		return sendDelegateResponse{OK: true, Chat: res.JID}, nil
	case statusPrivacyKind:
		rows, err := fetchStatusAudience(ctx, a)
		return delegatedResult(rows, err)
	case channelsListKind:
		rows, err := listChannels(ctx, a)
		return delegatedResult(rows, err)
	case channelInfoKind:
		row, err := fetchChannelInfo(ctx, a, req.To)
		return delegatedResult(row, err)
	case channelJoinKind:
		row, err := joinChannel(ctx, a, req.InviteCode)
		return delegatedResult(row, err)
	case channelLeaveKind:
		jid, err := leaveChannel(ctx, a, req.To)
		if err != nil {
			return sendDelegateResponse{}, err
		}
		return sendDelegateResponse{OK: true, Chat: jid.String()}, nil
	case channelMuteKind, channelUnmuteKind:
		jid, err := setChannelMute(ctx, a, req.To, req.Kind == channelMuteKind)
		if err != nil {
			return sendDelegateResponse{}, err
		}
		return sendDelegateResponse{OK: true, Chat: jid.String()}, nil
	case channelReactKind:
		res, err := reactToChannelPost(ctx, a, req.To, firstServerID(req.ServerIDs), req.Reaction)
		return delegatedResult(res, err)
	case channelMessagesKind:
		res, err := fetchChannelMessages(ctx, a, req.To, req.Count, req.BeforeServerID)
		return delegatedResult(res, err)
	case channelMarkViewedKind:
		res, err := markChannelPostsViewed(ctx, a, req.To, req.ServerIDs)
		return delegatedResult(res, err)
	case channelCreateKind:
		row, err := createChannel(ctx, a, req.Name, req.Description)
		return delegatedResult(row, err)
	case callRejectKind:
		res, err := rejectCall(ctx, a, req.To, req.ID)
		return delegatedResult(res, err)
	default:
		return sendDelegateResponse{}, fmt.Errorf("unsupported send kind %q", req.Kind)
	}
}

// delegatedResult encodes a command result for the caller, which decodes it
// into the same type and prints it as the direct command would.
func delegatedResult(result any, err error) (sendDelegateResponse, error) {
	if err != nil {
		return sendDelegateResponse{}, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return sendDelegateResponse{}, fmt.Errorf("encode result: %w", err)
	}
	return sendDelegateResponse{OK: true, Result: raw}, nil
}

// decodeDelegatedResult reads a result encoded by delegatedResult.
func decodeDelegatedResult(resp sendDelegateResponse, into any) error {
	if len(resp.Result) == 0 {
		return fmt.Errorf("the running sync process returned no result; restart `wacli sync` after upgrading")
	}
	if err := json.Unmarshal(resp.Result, into); err != nil {
		return fmt.Errorf("decode result from the running sync process: %w", err)
	}
	return nil
}

func firstServerID(ids []int) types.MessageServerID {
	if len(ids) != 1 {
		return 0
	}
	return ids[0]
}

func executeDelegatedStatusSend(ctx context.Context, a statusSendApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	res, err := sendStatusUpdate(ctx, a, statusSendOptions{
		message:         req.Message,
		file:            req.File,
		mimeOverride:    req.MIME,
		backgroundColor: req.BackgroundColor,
		font:            req.Font,
	})
	if err != nil {
		return sendDelegateResponse{}, err
	}
	waitForPostSendRetryReceipts(ctx, millisDuration(req.PostSendWaitMS, 0))
	resp := sendDelegateResponse{OK: true, Sent: true, To: types.StatusBroadcastJID.String(), ID: res.id}
	if res.media != "" {
		resp.File = map[string]string{"media": res.media, "mime_type": res.mimeType}
	}
	if res.storeWarning != nil {
		resp.StoreWarning = res.storeWarning.Error()
	}
	return resp, nil
}
