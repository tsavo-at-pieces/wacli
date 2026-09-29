package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// appStateChange builds one app-state write after the collection has been
// caught up, so it can read the current local mirror. persist records the
// change locally once WhatsApp accepted it.
type appStateChange func() (patch appstate.PatchInfo, persist func() error, err error)

// applyAppStateChange sends one app-state patch through the same ordered,
// recoverable path as archive, pin and mute. With forceFullReplay the
// collection is replayed from a full snapshot first, for writes that must see
// state stored before wacli mirrored it (favorites, list IDs).
func (a *App) applyAppStateChange(ctx context.Context, collection appstate.WAPatchName, forceFullReplay bool, build appStateChange) error {
	release, err := a.acquireChatStateSync(ctx)
	if err != nil {
		return err
	}
	defer release()
	if forceFullReplay {
		// Held under the chat-state lock, so only this write pays for the replay.
		if _, err := a.db.MarkAppStateRecoveryGeneration(string(collection)); err != nil {
			return fmt.Errorf("request full WhatsApp app state replay for %s: %w", collection, err)
		}
	}
	if err := a.syncChatStateBeforeWrite(ctx, collection); err != nil {
		return err
	}
	if forceFullReplay {
		if err := a.db.MarkAppStateMirrored(string(collection), nowUTC()); err != nil {
			return fmt.Errorf("record full WhatsApp app state replay for %s: %w", collection, err)
		}
	}
	patch, persist, err := build()
	if err != nil {
		return err
	}
	pending, err := a.beginLocalAppStateWrite(collection)
	if err != nil {
		return err
	}
	postSendEvents, err := a.wa.SendAppStatePatch(ctx, patch, func() { pending.reserve(a) })
	if err != nil {
		return errors.Join(err, a.failLocalAppStateWrite(ctx, &pending, postSendEvents))
	}
	if !pending.reserved {
		return fmt.Errorf("WhatsApp app state send completed without an apply boundary")
	}
	return a.completeLocalAppStateWrite(ctx, &pending, postSendEvents, persist)
}

// chatStoreJIDs is every local chat JID one chat may be stored under: its
// canonical phone JID and its LID when known.
func (a *App) chatStoreJIDs(ctx context.Context, jid types.JID) []string {
	canonical := a.canonicalStoreJID(ctx, jid)
	out := []string{canonicalJIDString(canonical), canonicalJIDString(jid)}
	if canonical.Server == types.DefaultUserServer {
		if lid := a.wa.ResolvePNToLID(ctx, canonical); !lid.IsEmpty() && lid.Server == types.HiddenUserServer {
			out = append(out, lid.ToNonAD().String())
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// DeleteChat deletes a chat on every device of the account and keeps its
// local messages as tombstones. It returns how many local messages it hid.
func (a *App) DeleteChat(ctx context.Context, jid types.JID, deleteMedia bool) (int64, error) {
	var hidden int64
	err := a.applyAppStateChange(ctx, appstate.WAPatchRegularHigh, false, func() (appstate.PatchInfo, func() error, error) {
		// A fresh keyless range covers messages this store never saw, as archive does.
		through := nowUTC().Truncate(time.Second)
		jids := a.chatStoreJIDs(ctx, jid)
		return wa.BuildDeleteChatPatch(jid, through, nil, deleteMedia), func() error {
			n, err := a.db.MarkChatDeleted(jids, through, through)
			hidden = n
			return err
		}, nil
	})
	return hidden, err
}

// ClearChat clears a chat's messages on every device of the account and keeps
// them locally as tombstones. Starred messages survive unless deleteStarred.
func (a *App) ClearChat(ctx context.Context, jid types.JID, deleteStarred, deleteMedia bool) (int64, error) {
	var hidden int64
	err := a.applyAppStateChange(ctx, appstate.WAPatchRegularHigh, false, func() (appstate.PatchInfo, func() error, error) {
		through := nowUTC().Truncate(time.Second)
		jids := a.chatStoreJIDs(ctx, jid)
		return wa.BuildClearChatPatch(jid, through, nil, deleteStarred, deleteMedia), func() error {
			n, err := a.db.MarkChatCleared(jids, through, through, deleteStarred)
			hidden = n
			return err
		}, nil
	})
	return hidden, err
}

// LockChat locks or unlocks a chat with WhatsApp's chat lock. An existing
// lock is overwritten under the JID the phone used for it.
func (a *App) LockChat(ctx context.Context, jid types.JID, locked bool) error {
	canonical := canonicalJIDString(a.canonicalStoreJID(ctx, jid))
	return a.applyAppStateChange(ctx, appstate.WAPatchRegularLow, false, func() (appstate.PatchInfo, func() error, error) {
		target := jid
		raw, err := a.db.ChatLockJID(canonical)
		if err != nil {
			return appstate.PatchInfo{}, nil, err
		}
		if parsed, err := types.ParseJID(raw); raw != "" && err == nil {
			target = parsed
		}
		return wa.BuildLockChatPatch(target, locked), func() error {
			return a.db.SetChatLocked(canonical, target.String(), locked)
		}, nil
	})
}

// MessageRef identifies a stored message for app-state and protocol writes.
type MessageRef struct {
	Chat   types.JID
	ID     string
	FromMe bool
	// Sender is the author of a group message someone else sent.
	Sender types.JID
}

// StarMessage stars or unstars a message on every device of the account.
func (a *App) StarMessage(ctx context.Context, ref MessageRef, starred bool) error {
	chatJID := canonicalJIDString(a.canonicalStoreJID(ctx, ref.Chat))
	senderJID := ""
	if !ref.FromMe && !ref.Sender.IsEmpty() {
		senderJID = canonicalJIDString(a.canonicalStoreJID(ctx, ref.Sender))
	}
	return a.applyAppStateChange(ctx, appstate.WAPatchRegularHigh, false, func() (appstate.PatchInfo, func() error, error) {
		return wa.BuildStarPatch(ref.Chat, ref.Sender, types.MessageID(ref.ID), ref.FromMe, starred), func() error {
			return a.db.SetStarred(store.SetStarredParams{
				ChatJID:   chatJID,
				MsgID:     ref.ID,
				SenderJID: senderJID,
				FromMe:    ref.FromMe,
				Starred:   starred,
				StarredAt: nowUTC(),
			})
		}, nil
	})
}

// SetChatFavorite adds a chat to, or removes it from, the favorites list.
// Favorites are one app-state value, so the write carries the whole list; it
// replays regular_high first unless the complete list is already stored.
func (a *App) SetChatFavorite(ctx context.Context, jid types.JID, favorite bool) error {
	known, err := a.db.FavoritesKnown()
	if err != nil {
		return err
	}
	canonical := canonicalJIDString(a.canonicalStoreJID(ctx, jid))
	return a.applyAppStateChange(ctx, appstate.WAPatchRegularHigh, !known, func() (appstate.PatchInfo, func() error, error) {
		current, err := a.db.ListFavorites()
		if err != nil {
			return appstate.PatchInfo{}, nil, err
		}
		next := make([]store.FavoriteChat, 0, len(current)+1)
		present := false
		for _, f := range current {
			if f.ChatJID == canonical || f.RawJID == jid.String() {
				present = true
				if !favorite {
					continue
				}
			}
			next = append(next, f)
		}
		if favorite && !present {
			next = append(next, store.FavoriteChat{ChatJID: canonical, RawJID: jid.String()})
		}
		ids := make([]string, 0, len(next))
		for _, f := range next {
			ids = append(ids, f.RawJID)
		}
		return wa.BuildFavoritesPatch(ids), func() error {
			return a.db.ReplaceFavorites(next, nowUTC())
		}, nil
	})
}

// Lists (labels) live in the regular collection. Writes replay it from a full
// snapshot until one has been stored, so a new list ID cannot reuse the ID of
// a list created before wacli mirrored lists.
func (a *App) listsNeedFullReplay() (bool, error) {
	mirrored, err := a.db.AppStateMirrored(string(appstate.WAPatchRegular))
	return !mirrored, err
}

// CreateChatList creates a custom WhatsApp list.
func (a *App) CreateChatList(ctx context.Context, name string) (store.ChatList, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return store.ChatList{}, fmt.Errorf("list name is required")
	}
	force, err := a.listsNeedFullReplay()
	if err != nil {
		return store.ChatList{}, err
	}
	var created store.ChatList
	err = a.applyAppStateChange(ctx, appstate.WAPatchRegular, force, func() (appstate.PatchInfo, func() error, error) {
		lists, err := a.db.ListChatLists(false)
		if err != nil {
			return appstate.PatchInfo{}, nil, err
		}
		if existing, ok := store.FindChatList(lists, name); ok {
			return appstate.PatchInfo{}, nil, fmt.Errorf("a list named %q already exists (id %s)", existing.Name, existing.ID)
		}
		id, err := a.db.NextChatListID()
		if err != nil {
			return appstate.PatchInfo{}, nil, err
		}
		order, color := nextChatListPlacement(lists)
		listType := int32(waSyncAction.LabelEditAction_CUSTOM)
		def := store.ChatList{ID: id, Name: name, Color: color, OrderIndex: &order, ListType: &listType, UpdatedAt: nowUTC()}
		return wa.BuildLabelEditPatch(id, labelDefinition(def)), func() error {
			created = def
			return a.db.UpsertChatList(def)
		}, nil
	})
	return created, err
}

// RenameChatList renames a custom list, keeping everything else the phone set.
func (a *App) RenameChatList(ctx context.Context, ref, name string) (store.ChatList, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return store.ChatList{}, fmt.Errorf("new list name is required")
	}
	return a.editCustomChatList(ctx, ref, func(lists []store.ChatList, l *store.ChatList) error {
		if existing, ok := store.FindChatList(lists, name); ok && existing.ID != l.ID {
			return fmt.Errorf("a list named %q already exists (id %s)", existing.Name, existing.ID)
		}
		l.Name = name
		return nil
	})
}

// DeleteChatList deletes a custom list. Its chats are not changed.
func (a *App) DeleteChatList(ctx context.Context, ref string) (store.ChatList, error) {
	return a.editCustomChatList(ctx, ref, func(_ []store.ChatList, l *store.ChatList) error {
		l.Deleted = true
		return nil
	})
}

func (a *App) editCustomChatList(ctx context.Context, ref string, edit func([]store.ChatList, *store.ChatList) error) (store.ChatList, error) {
	if strings.TrimSpace(ref) == "" {
		return store.ChatList{}, fmt.Errorf("a list ID or name is required")
	}
	force, err := a.listsNeedFullReplay()
	if err != nil {
		return store.ChatList{}, err
	}
	var edited store.ChatList
	err = a.applyAppStateChange(ctx, appstate.WAPatchRegular, force, func() (appstate.PatchInfo, func() error, error) {
		lists, err := a.db.ListChatLists(false)
		if err != nil {
			return appstate.PatchInfo{}, nil, err
		}
		l, err := findEditableChatList(lists, ref)
		if err != nil {
			return appstate.PatchInfo{}, nil, err
		}
		if err := edit(lists, &l); err != nil {
			return appstate.PatchInfo{}, nil, err
		}
		l.UpdatedAt = nowUTC()
		return wa.BuildLabelEditPatch(l.ID, labelDefinition(l)), func() error {
			edited = l
			return a.db.UpsertChatList(l)
		}, nil
	})
	return edited, err
}

// SetChatListMember adds a chat to, or removes it from, a custom list. A
// removal is written under every JID the phone used for that chat.
func (a *App) SetChatListMember(ctx context.Context, ref string, jid types.JID, member bool) (store.ChatList, error) {
	if strings.TrimSpace(ref) == "" {
		return store.ChatList{}, fmt.Errorf("a list ID or name is required")
	}
	force, err := a.listsNeedFullReplay()
	if err != nil {
		return store.ChatList{}, err
	}
	canonical := canonicalJIDString(a.canonicalStoreJID(ctx, jid))
	var list store.ChatList
	err = a.applyAppStateChange(ctx, appstate.WAPatchRegular, force, func() (appstate.PatchInfo, func() error, error) {
		lists, err := a.db.ListChatLists(false)
		if err != nil {
			return appstate.PatchInfo{}, nil, err
		}
		l, err := findEditableChatList(lists, ref)
		if err != nil {
			return appstate.PatchInfo{}, nil, err
		}
		members, err := a.db.ChatListMembers(l.ID, false)
		if err != nil {
			return appstate.PatchInfo{}, nil, err
		}
		targets := listMemberTargets(members, canonical, jid, member)
		now := nowUTC()
		return wa.BuildLabelChatPatch(l.ID, targets, member), func() error {
			list = l
			for _, target := range targets {
				if err := a.db.SetChatListMember(store.ChatListMember{
					ListID: l.ID, ChatJID: canonical, RawJID: target.String(), Labeled: member, UpdatedAt: now,
				}); err != nil {
					return err
				}
			}
			return nil
		}, nil
	})
	return list, err
}

// listMemberTargets picks the app-state JIDs to write: the phone's own index
// entries for this chat when there are any, else the JID that was asked for.
func listMemberTargets(members []store.ChatListMember, canonical string, requested types.JID, member bool) []types.JID {
	var known []types.JID
	for _, m := range members {
		if m.ChatJID != canonical && m.RawJID != requested.String() {
			continue
		}
		if !member && !m.Labeled {
			continue
		}
		if parsed, err := types.ParseJID(m.RawJID); err == nil {
			known = append(known, parsed)
		}
	}
	switch {
	case len(known) == 0:
		return []types.JID{requested}
	case member:
		return known[:1]
	default:
		return known
	}
}

func findEditableChatList(lists []store.ChatList, ref string) (store.ChatList, error) {
	l, ok := store.FindChatList(lists, ref)
	if !ok {
		return store.ChatList{}, fmt.Errorf("no list matches %q (see `wacli chats lists`)", ref)
	}
	if l.ListType != nil && *l.ListType != int32(waSyncAction.LabelEditAction_CUSTOM) && *l.ListType != int32(waSyncAction.LabelEditAction_NONE) {
		return store.ChatList{}, fmt.Errorf("list %q is a %s list managed by WhatsApp; only custom lists can be changed (use `wacli chats favorite` for favorites)", l.Name, ChatListTypeName(l.ListType))
	}
	if l.IsImmutable != nil && *l.IsImmutable {
		return store.ChatList{}, fmt.Errorf("list %q cannot be changed", l.Name)
	}
	return l, nil
}

// nextChatListPlacement puts a new list after every other list, with the
// first color no other live list uses.
func nextChatListPlacement(lists []store.ChatList) (int32, int32) {
	order := int32(0)
	used := map[int32]bool{}
	for _, l := range lists {
		if l.OrderIndex != nil && *l.OrderIndex >= order {
			order = *l.OrderIndex + 1
		}
		used[l.Color] = true
	}
	const colors = 20
	for c := int32(0); c < colors; c++ {
		if !used[c] {
			return order, c
		}
	}
	return order, int32(len(lists) % colors)
}

func labelDefinition(l store.ChatList) wa.LabelDefinition {
	def := wa.LabelDefinition{
		Name:         l.Name,
		Color:        l.Color,
		PredefinedID: l.PredefinedID,
		Deleted:      l.Deleted,
		OrderIndex:   l.OrderIndex,
		IsActive:     l.IsActive,
		IsImmutable:  l.IsImmutable,
		MuteEndMS:    l.MuteEndMS,
	}
	if l.ListType != nil {
		t := waSyncAction.LabelEditAction_ListType(*l.ListType)
		def.Type = &t
	}
	return def
}

// ChatListTypeName names a list type the way `chats lists` prints it.
func ChatListTypeName(listType *int32) string {
	if listType == nil {
		return "none"
	}
	return strings.ToLower(waSyncAction.LabelEditAction_ListType(*listType).String())
}

// IsFavoritesListType reports whether a list is WhatsApp's favorites list,
// whose members travel in the separate favorites value.
func IsFavoritesListType(listType *int32) bool {
	return listType != nil && *listType == int32(waSyncAction.LabelEditAction_FAVORITES)
}

// IsComputedListType reports filter lists whose members WhatsApp computes on
// the phone (unread, groups and similar) instead of syncing them. Custom,
// business and other labels keep their synced chat associations.
func IsComputedListType(listType *int32) bool {
	if listType == nil {
		return false
	}
	switch waSyncAction.LabelEditAction_ListType(*listType) {
	case waSyncAction.LabelEditAction_UNREAD, waSyncAction.LabelEditAction_GROUPS,
		waSyncAction.LabelEditAction_COMMUNITY, waSyncAction.LabelEditAction_CHANNELS,
		waSyncAction.LabelEditAction_ARCHIVED, waSyncAction.LabelEditAction_LOCKED,
		waSyncAction.LabelEditAction_INVITES, waSyncAction.LabelEditAction_DRAFTED,
		waSyncAction.LabelEditAction_MENTIONS_AND_REPLIES, waSyncAction.LabelEditAction_REQUESTS:
		return true
	default:
		return false
	}
}

// persistChatAppStateEvent stores chat delete/clear/lock, list, favorites and
// full-sync events from other devices.
func (a *App) persistChatAppStateEvent(ctx context.Context, evt any) error {
	var err error
	var kind string
	var jid types.JID
	switch v := evt.(type) {
	case *events.DeleteChat:
		if v == nil || v.JID.IsEmpty() {
			return nil
		}
		kind, jid = "delete_chat", v.JID
		through, at := chatActionBoundary(v.Action.GetMessageRange(), v.Timestamp)
		_, err = a.db.MarkChatDeleted(a.chatStoreJIDs(ctx, v.JID), through, at)
	case *events.ClearChat:
		if v == nil || v.JID.IsEmpty() {
			return nil
		}
		kind, jid = "clear_chat", v.JID
		through, at := chatActionBoundary(v.Action.GetMessageRange(), v.Timestamp)
		// The typed event omits the keep-starred flag; the raw AppState event
		// below clears starred messages when the phone asked for that.
		_, err = a.db.MarkChatCleared(a.chatStoreJIDs(ctx, v.JID), through, at, false)
	case *events.AppState:
		kind, jid, err = a.persistChatAppStateValue(ctx, v)
	case *events.LabelEdit:
		if v == nil || strings.TrimSpace(v.LabelID) == "" || v.Action == nil {
			return nil
		}
		kind = "list"
		err = a.db.UpsertChatList(chatListFromAction(v.LabelID, v.Action, v.Timestamp))
	case *events.LabelAssociationChat:
		if v == nil || v.JID.IsEmpty() || strings.TrimSpace(v.LabelID) == "" || v.Action == nil {
			return nil
		}
		kind, jid = "list_member", v.JID
		err = a.db.SetChatListMember(store.ChatListMember{
			ListID:    strings.TrimSpace(v.LabelID),
			ChatJID:   canonicalJIDString(a.canonicalStoreJID(ctx, v.JID)),
			RawJID:    v.JID.String(),
			Labeled:   v.Action.GetLabeled(),
			UpdatedAt: v.Timestamp,
		})
	case *events.AppStateSyncComplete:
		if v == nil || strings.TrimSpace(string(v.Name)) == "" {
			return nil
		}
		kind = "app_state_mirror"
		err = a.db.MarkAppStateMirrored(string(v.Name), nowUTC())
	default:
		return nil
	}
	if err != nil {
		a.emitWarning(
			"chat_app_state_store_failed",
			fmt.Sprintf("warning: failed to store %s app state for %s: %v", kind, jid, err),
			map[string]any{"kind": kind, "jid": jid.String(), "error": err.Error()},
		)
	}
	return err
}

// persistChatAppStateValue stores the raw app-state values whatsmeow has no
// typed event for: chat lock, favorites, and clear-chat's keep-starred flag.
func (a *App) persistChatAppStateValue(ctx context.Context, v *events.AppState) (string, types.JID, error) {
	if v == nil || v.SyncActionValue == nil || len(v.Index) == 0 {
		return "", types.JID{}, nil
	}
	switch v.Index[0] {
	case appstate.IndexLock:
		if len(v.Index) < 2 || v.GetLockChatAction() == nil {
			return "", types.JID{}, nil
		}
		jid, err := types.ParseJID(v.Index[1])
		if err != nil || jid.IsEmpty() {
			return "", types.JID{}, nil
		}
		canonical := canonicalJIDString(a.canonicalStoreJID(ctx, jid))
		return "lock_chat", jid, a.db.SetChatLocked(canonical, jid.String(), v.GetLockChatAction().GetLocked())
	case appstate.IndexFavorites:
		if v.GetFavoritesAction() == nil {
			return "", types.JID{}, nil
		}
		var favorites []store.FavoriteChat
		for _, f := range v.GetFavoritesAction().GetFavorites() {
			raw := strings.TrimSpace(f.GetID())
			if raw == "" {
				continue
			}
			chat := raw
			if jid, err := types.ParseJID(raw); err == nil {
				chat = canonicalJIDString(a.canonicalStoreJID(ctx, jid))
			}
			favorites = append(favorites, store.FavoriteChat{ChatJID: chat, RawJID: raw})
		}
		return "favorites", types.JID{}, a.db.ReplaceFavorites(favorites, time.UnixMilli(v.GetTimestamp()))
	case appstate.IndexClearChat:
		if len(v.Index) < 2 || !wa.ClearChatDeletesStarred(v.Index) {
			return "", types.JID{}, nil
		}
		jid, err := types.ParseJID(v.Index[1])
		if err != nil || jid.IsEmpty() {
			return "", types.JID{}, nil
		}
		through, at := chatActionBoundary(v.GetClearChatAction().GetMessageRange(), time.UnixMilli(v.GetTimestamp()))
		_, err = a.db.MarkChatCleared(a.chatStoreJIDs(ctx, jid), through, at, true)
		return "clear_chat", jid, err
	}
	return "", types.JID{}, nil
}

// chatAppStateCollections is the app-state collection each chat event belongs
// to, for durable recovery markers while it is persisted.
func chatAppStateCollections(evt any) []appstate.WAPatchName {
	switch v := evt.(type) {
	case *events.DeleteChat, *events.ClearChat:
		return []appstate.WAPatchName{appstate.WAPatchRegularHigh}
	case *events.LabelEdit, *events.LabelAssociationChat:
		return []appstate.WAPatchName{appstate.WAPatchRegular}
	case *events.AppState:
		if v == nil || v.SyncActionValue == nil || len(v.Index) == 0 {
			return nil
		}
		switch {
		case v.Index[0] == appstate.IndexLock && v.GetLockChatAction() != nil:
			return []appstate.WAPatchName{appstate.WAPatchRegularLow}
		case v.Index[0] == appstate.IndexFavorites && v.GetFavoritesAction() != nil:
			return []appstate.WAPatchName{appstate.WAPatchRegularHigh}
		case wa.ClearChatDeletesStarred(v.Index):
			return []appstate.WAPatchName{appstate.WAPatchRegularHigh}
		}
	}
	return nil
}

// chatActionBoundary is the newest message a delete or clear covers, and when
// it happened. Without a message range, everything up to the action goes.
func chatActionBoundary(r *waSyncAction.SyncActionMessageRange, at time.Time) (time.Time, time.Time) {
	if at.IsZero() || at.Unix() <= 0 {
		at = nowUTC()
	}
	at = at.UTC()
	if through, ok := wa.MessageRangeBoundary(r); ok {
		return through, at
	}
	return at.Truncate(time.Second), at
}

func chatListFromAction(id string, action *waSyncAction.LabelEditAction, at time.Time) store.ChatList {
	l := store.ChatList{
		ID:           strings.TrimSpace(id),
		Name:         action.GetName(),
		Color:        action.GetColor(),
		PredefinedID: action.PredefinedID,
		OrderIndex:   action.OrderIndex,
		IsActive:     action.IsActive,
		IsImmutable:  action.IsImmutable,
		MuteEndMS:    action.MuteEndTimeMS,
		Deleted:      action.GetDeleted(),
		UpdatedAt:    at,
	}
	if action.Type != nil {
		t := int32(action.GetType())
		l.ListType = &t
	}
	return l
}

// storeParsedPin records a pin-in-chat or unpin message from any device.
func (a *App) storeParsedPin(chatJID, senderJID string, pm wa.ParsedMessage) error {
	p := pm.Pin
	changed := p.ChangedAt
	if changed.IsZero() {
		changed = pm.Timestamp
	}
	var expires *time.Time
	if p.Pinned && p.Duration > 0 {
		e := changed.Add(p.Duration)
		expires = &e
	}
	pinnedBy := senderJID
	if pm.FromMe {
		pinnedBy = ""
	}
	return a.db.SetMessagePin(store.MessagePin{
		ChatJID:   chatJID,
		MsgID:     p.TargetID,
		Pinned:    p.Pinned,
		PinnedBy:  pinnedBy,
		ChangedAt: changed,
		ExpiresAt: expires,
	})
}
