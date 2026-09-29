package wa

import (
	"context"
	"fmt"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// userStatusMuteVersion is the syncd action version WhatsApp Web registers for
// userStatusMute (module WAWebUserStatusMuteSync, collection regular_high).
// whatsmeow only decodes this action, so wacli builds the patch itself.
const userStatusMuteVersion = 7

// buildUserStatusMutePatch mutes or unmutes one contact's status updates for
// this account. The index is ["userStatusMute", <user JID>], the same shape
// whatsmeow decodes into events.UserStatusMute.
func buildUserStatusMutePatch(target types.JID, mute bool) appstate.PatchInfo {
	return appstate.PatchInfo{
		Type: appstate.WAPatchRegularHigh,
		Mutations: []appstate.MutationInfo{{
			Index:   []string{appstate.IndexUserStatusMute, target.ToNonAD().String()},
			Version: userStatusMuteVersion,
			Value: &waSyncAction.SyncActionValue{
				UserStatusMuteAction: &waSyncAction.UserStatusMuteAction{
					Muted: proto.Bool(mute),
				},
			},
		}},
	}
}

// MuteUserStatus sends the status mute app-state patch. beforeApply runs
// before whatsmeow can advance the local app-state version, as for the chat
// state patches.
func (c *Client) MuteUserStatus(ctx context.Context, target types.JID, mute bool, beforeApply func()) ([]any, error) {
	if target.Server != types.DefaultUserServer && target.Server != types.HiddenUserServer {
		return nil, fmt.Errorf("status mute needs a user JID, got %s", target)
	}
	return c.sendAppStateWithBoundary(ctx, buildUserStatusMutePatch(target, mute), beforeApply)
}

// GetStatusPrivacy returns who receives this account's status updates.
func (c *Client) GetStatusPrivacy(ctx context.Context) ([]types.StatusPrivacy, error) {
	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()
	if cli == nil || !cli.IsConnected() {
		return nil, fmt.Errorf("not connected")
	}
	return cli.GetStatusPrivacy(ctx)
}
