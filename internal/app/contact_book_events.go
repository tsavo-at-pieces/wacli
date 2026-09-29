package app

import (
	"context"
	"fmt"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// handleContactEvent mirrors a WhatsApp contact entry saved or renamed from
// any linked device, including wacli's own `contacts save`, into the local
// contacts table so search and show use the saved name.
func (a *App) handleContactEvent(ctx context.Context, evt *events.Contact) {
	if evt == nil || evt.JID.IsEmpty() || evt.Action == nil {
		return
	}
	jid := a.canonicalStoreJID(ctx, evt.JID)
	phone := ""
	if jid.Server == types.DefaultUserServer {
		phone = jid.User
	}
	var others []string
	if raw := evt.Action.GetLidJID(); raw != "" {
		if lid, err := types.ParseJID(raw); err == nil {
			others = append(others, canonicalJIDString(lid))
		}
	}
	if original := canonicalJID(evt.JID); original != jid {
		others = append(others, original.String())
	}
	if err := a.db.SetContactBookName(jid.String(), phone, evt.Action.GetFirstName(), evt.Action.GetFullName(), others...); err != nil {
		a.emitWarning(
			"contact_store_failed",
			fmt.Sprintf("warning: failed to store WhatsApp contact %s: %v", jid, err),
			map[string]any{"jid": jid.String(), "error": err.Error()},
		)
	}
}

// handleBlocklistEvent applies block and unblock changes WhatsApp reports for
// this account. A "modify" notification carries no changes; `contacts
// blocklist` refetches the whole list.
func (a *App) handleBlocklistEvent(ctx context.Context, evt *events.Blocklist) {
	if evt == nil || evt.Action == events.BlocklistActionModify {
		return
	}
	now := nowUTC()
	for _, change := range evt.Changes {
		var blocked bool
		switch change.Action {
		case events.BlocklistChangeActionBlock:
			blocked = true
		case events.BlocklistChangeActionUnblock:
		default:
			continue
		}
		jids := []string{canonicalJIDString(change.JID)}
		// Only the stored mapping: an event handler must not wait on lookups.
		if pn := a.wa.ResolveLIDToPN(ctx, change.JID); canonicalJID(pn) != canonicalJID(change.JID) {
			jids = append(jids, canonicalJIDString(pn))
		}
		if err := a.db.SetContactsBlocked(jids, blocked, now); err != nil {
			a.emitWarning(
				"blocklist_store_failed",
				fmt.Sprintf("warning: failed to store block list change for %s: %v", change.JID, err),
				map[string]any{"jid": change.JID.String(), "error": err.Error()},
			)
		}
	}
}
