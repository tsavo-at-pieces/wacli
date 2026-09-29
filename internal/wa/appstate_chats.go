package wa

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// App-state index versions that whatsmeow has no builder for. They follow
// WhatsApp Web's syncd action registry: clearChat and deleteChat are version 6
// in regular_high, lock is version 7 in regular_low, favorites is version 1 in
// regular_high, and label_edit is version 3 in regular.
const (
	clearChatVersion = 6
	lockChatVersion  = 7
	favoritesVersion = 1
	labelEditVersion = 3
)

// SendAppStatePatch sends one app-state patch. beforeApply runs before
// whatsmeow can advance the collection, so callers can reserve their ordered
// local persistence first, as the archive, pin and mute writes do.
func (c *Client) SendAppStatePatch(ctx context.Context, patch appstate.PatchInfo, beforeApply func()) ([]any, error) {
	return c.sendAppStateWithBoundary(ctx, patch, beforeApply)
}

// BuildDeleteChatPatch deletes a chat on every device of the account up to the
// given message range. A zero timestamp means now.
func BuildDeleteChatPatch(target types.JID, lastMessageTS time.Time, lastMessageKey *waCommon.MessageKey, deleteMedia bool) appstate.PatchInfo {
	return appstate.BuildDeleteChat(target, lastMessageTS, lastMessageKey, deleteMedia)
}

// BuildClearChatPatch clears a chat's messages on every device of the account
// up to the given message range. The index is
// ["clearChat", chat, deleteStarred, deleteMedia]; deleteStarred "0" keeps
// starred messages, as the phone's default does.
func BuildClearChatPatch(target types.JID, lastMessageTS time.Time, lastMessageKey *waCommon.MessageKey, deleteStarred, deleteMedia bool) appstate.PatchInfo {
	return appstate.PatchInfo{
		Type: appstate.WAPatchRegularHigh,
		Mutations: []appstate.MutationInfo{{
			Index:   []string{appstate.IndexClearChat, target.String(), boolIndex(deleteStarred), boolIndex(deleteMedia)},
			Version: clearChatVersion,
			Value: &waSyncAction.SyncActionValue{
				ClearChatAction: &waSyncAction.ClearChatAction{
					MessageRange: messageRange(lastMessageTS, lastMessageKey),
				},
			},
		}},
	}
}

// BuildLockChatPatch locks or unlocks a chat (WhatsApp's chat lock).
func BuildLockChatPatch(target types.JID, locked bool) appstate.PatchInfo {
	return appstate.PatchInfo{
		Type: appstate.WAPatchRegularLow,
		Mutations: []appstate.MutationInfo{{
			Index:   []string{appstate.IndexLock, target.String()},
			Version: lockChatVersion,
			Value: &waSyncAction.SyncActionValue{
				LockChatAction: &waSyncAction.LockChatAction{Locked: proto.Bool(locked)},
			},
		}},
	}
}

// BuildStarPatch stars or unstars one message. sender is the author of a group
// message sent by someone else; for your own messages and one-to-one chats the
// index participant is "0", matching delete-for-me.
func BuildStarPatch(chat, sender types.JID, messageID types.MessageID, fromMe, starred bool) appstate.PatchInfo {
	if fromMe || sender.IsEmpty() {
		sender = chat
	}
	return appstate.BuildStar(chat, sender, messageID, fromMe, starred)
}

// LabelDefinition is one WhatsApp list (a label at the protocol level). Nil
// pointers are left out of the mutation so a rewrite keeps what the phone set.
type LabelDefinition struct {
	Name         string
	Color        int32
	PredefinedID *int32
	Deleted      bool
	OrderIndex   *int32
	IsActive     *bool
	Type         *waSyncAction.LabelEditAction_ListType
	IsImmutable  *bool
	MuteEndMS    *int64
}

// BuildLabelEditPatch creates, rewrites or deletes one list definition.
func BuildLabelEditPatch(labelID string, def LabelDefinition) appstate.PatchInfo {
	action := &waSyncAction.LabelEditAction{
		Name:          proto.String(def.Name),
		Color:         proto.Int32(def.Color),
		PredefinedID:  def.PredefinedID,
		Deleted:       proto.Bool(def.Deleted),
		OrderIndex:    def.OrderIndex,
		IsActive:      def.IsActive,
		Type:          def.Type,
		IsImmutable:   def.IsImmutable,
		MuteEndTimeMS: def.MuteEndMS,
	}
	return appstate.PatchInfo{
		Type: appstate.WAPatchRegular,
		Mutations: []appstate.MutationInfo{{
			Index:   []string{appstate.IndexLabelEdit, labelID},
			Version: labelEditVersion,
			Value:   &waSyncAction.SyncActionValue{LabelEditAction: action},
		}},
	}
}

// BuildLabelChatPatch adds chats to, or removes them from, one list. Every
// target is written, so a chat known under both its phone and LID JIDs can be
// removed under each index the phone may have used.
func BuildLabelChatPatch(labelID string, targets []types.JID, labeled bool) appstate.PatchInfo {
	patch := appstate.PatchInfo{Type: appstate.WAPatchRegular}
	for _, target := range targets {
		patch.Mutations = append(patch.Mutations, appstate.BuildLabelChat(target, labelID, labeled).Mutations...)
	}
	return patch
}

// BuildFavoritesPatch replaces the account's favorites list. The list is one
// mutation, so every write carries the complete, ordered list.
func BuildFavoritesPatch(ids []string) appstate.PatchInfo {
	favorites := make([]*waSyncAction.FavoritesAction_Favorite, 0, len(ids))
	for _, id := range ids {
		favorites = append(favorites, &waSyncAction.FavoritesAction_Favorite{ID: proto.String(id)})
	}
	return appstate.PatchInfo{
		Type: appstate.WAPatchRegularHigh,
		Mutations: []appstate.MutationInfo{{
			Index:   []string{appstate.IndexFavorites},
			Version: favoritesVersion,
			Value: &waSyncAction.SyncActionValue{
				FavoritesAction: &waSyncAction.FavoritesAction{Favorites: favorites},
			},
		}},
	}
}

// MessageRangeBoundary is the newest message time a delete or clear covers:
// the range's last message timestamp or its newest listed message, whichever
// is later. ok is false when the range carries neither.
func MessageRangeBoundary(r *waSyncAction.SyncActionMessageRange) (time.Time, bool) {
	if r == nil {
		return time.Time{}, false
	}
	newest := r.GetLastMessageTimestamp()
	for _, msg := range r.GetMessages() {
		if ts := msg.GetTimestamp(); ts > newest {
			newest = ts
		}
	}
	if newest <= 0 {
		return time.Time{}, false
	}
	// Ranges carry seconds; tolerate a millisecond value.
	if newest > 1e12 {
		return time.UnixMilli(newest).UTC(), true
	}
	return time.Unix(newest, 0).UTC(), true
}

// ClearChatDeletesStarred reads the deleteStarred flag from a clearChat index.
func ClearChatDeletesStarred(index []string) bool {
	return len(index) > 2 && index[0] == appstate.IndexClearChat && strings.TrimSpace(index[2]) == "1"
}

// SetDisappearingTimer sets a chat's disappearing-message timer. In one-to-one
// chats WhatsApp delivers it as a message the other person sees; in groups it
// is a group setting.
func (c *Client) SetDisappearingTimer(ctx context.Context, chat types.JID, timer time.Duration) error {
	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()
	if cli == nil || !cli.IsConnected() {
		return fmt.Errorf("not connected")
	}
	return cli.SetDisappearingTimer(ctx, chat, timer, time.Now())
}

func messageRange(lastMessageTS time.Time, lastMessageKey *waCommon.MessageKey) *waSyncAction.SyncActionMessageRange {
	if lastMessageTS.IsZero() {
		lastMessageTS = time.Now()
	}
	r := &waSyncAction.SyncActionMessageRange{
		LastMessageTimestamp: proto.Int64(lastMessageTS.Unix()),
	}
	if lastMessageKey != nil {
		r.Messages = []*waSyncAction.SyncActionMessage{{
			Key:       lastMessageKey,
			Timestamp: proto.Int64(lastMessageTS.Unix()),
		}}
	}
	return r
}

func boolIndex(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
