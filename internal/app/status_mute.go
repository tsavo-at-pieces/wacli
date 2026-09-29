package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// MuteStatus mutes or unmutes a contact's status updates on WhatsApp and
// records the result locally. target is the identity written into the
// app-state index; the local row is keyed by the contact's stored JID.
//
// It follows the chat state writes: the regular_high collection is brought up
// to date first, and the local row is written in app-state persistence order.
func (a *App) MuteStatus(ctx context.Context, target types.JID, mute bool) error {
	if target.Server != types.DefaultUserServer && target.Server != types.HiddenUserServer {
		return fmt.Errorf("status mute needs a user JID, got %s", target)
	}
	target = target.ToNonAD()
	release, err := a.beginChatStateWrite(ctx, appstate.WAPatchRegularHigh)
	if err != nil {
		return err
	}
	defer release()
	contact := canonicalJIDString(a.canonicalStoreJID(ctx, target))
	pending, err := a.beginLocalAppStateWrite(appstate.WAPatchRegularHigh)
	if err != nil {
		return err
	}
	postSendEvents, err := a.wa.MuteUserStatus(ctx, target, mute, func() { pending.reserve(a) })
	if err != nil {
		return errors.Join(err, a.failLocalAppStateWrite(ctx, &pending, postSendEvents))
	}
	if !pending.reserved {
		return fmt.Errorf("WhatsApp app state send completed without an apply boundary")
	}
	return a.completeLocalAppStateWrite(ctx, &pending, postSendEvents, func() error {
		return a.db.SetStatusMute(store.SetStatusMuteParams{
			JID:       contact,
			IndexJID:  target.String(),
			Muted:     mute,
			UpdatedAt: nowUTC(),
		})
	})
}

// handleUserStatusMuteEvent mirrors a status mute made on any device.
func (a *App) handleUserStatusMuteEvent(ctx context.Context, evt *events.UserStatusMute) error {
	if evt == nil || evt.JID.IsEmpty() || evt.Action == nil {
		return nil
	}
	index := evt.JID.ToNonAD()
	contact := canonicalJIDString(a.canonicalStoreJID(ctx, index))
	if err := a.db.SetStatusMute(store.SetStatusMuteParams{
		JID:       contact,
		IndexJID:  index.String(),
		Muted:     evt.Action.GetMuted(),
		UpdatedAt: evt.Timestamp,
	}); err != nil {
		a.emitWarning(
			"status_mute_store_failed",
			fmt.Sprintf("warning: failed to store status mute for %s: %v", evt.JID, err),
			map[string]any{"jid": evt.JID.String(), "error": err.Error()},
		)
		return err
	}
	return nil
}
