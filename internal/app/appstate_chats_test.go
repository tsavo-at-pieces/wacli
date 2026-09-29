package app

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/appstate"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Fictional identities only.
var (
	chatTestPN    = types.NewJID("15550000001", types.DefaultUserServer)
	chatTestLID   = types.NewJID("100000000001", types.HiddenUserServer)
	chatTestGroup = types.NewJID("120363000000000001", types.GroupServer)
	chatTestBase  = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
)

func newChatActionsTestApp(t *testing.T) (*App, *fakeWA) {
	t.Helper()
	a := newTestApp(t)
	f := newFakeWA()
	f.lids[chatTestLID] = chatTestPN
	a.wa = f
	previous := nowUTC
	nowUTC = func() time.Time { return chatTestBase.Add(time.Hour) }
	t.Cleanup(func() { nowUTC = previous })
	return a, f
}

func seedChatActionMessages(t *testing.T, a *App, chat types.JID, ids ...string) {
	t.Helper()
	if err := a.db.UpsertChat(chat.String(), chatKind(chat), "Test chat", chatTestBase.Add(time.Duration(len(ids))*time.Minute)); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	for i, id := range ids {
		if err := a.db.UpsertMessage(store.UpsertMessageParams{
			ChatJID: chat.String(), MsgID: id, SenderJID: chat.String(), Timestamp: chatTestBase.Add(time.Duration(i+1) * time.Minute), Text: "Fictional note",
		}); err != nil {
			t.Fatalf("UpsertMessage: %v", err)
		}
	}
}

func sentPatches(f *fakeWA) []appstate.PatchInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.appStatePatches)
}

func appStateFetches(f *fakeWA) []fakeAppStateFetch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.appStateFetches)
}

func onlyPatchIndex(t *testing.T, f *fakeWA, collection appstate.WAPatchName) appstate.MutationInfo {
	t.Helper()
	patches := sentPatches(f)
	if len(patches) != 1 || patches[0].Type != collection || len(patches[0].Mutations) != 1 {
		t.Fatalf("patches = %+v, want one %s mutation", patches, collection)
	}
	return patches[0].Mutations[0]
}

func TestDeleteChatSendsPatchAndKeepsTombstones(t *testing.T) {
	a, f := newChatActionsTestApp(t)
	seedChatActionMessages(t, a, chatTestPN, "M1", "M2")
	seedChatActionMessages(t, a, chatTestLID, "L1")

	hidden, err := a.DeleteChat(context.Background(), chatTestLID, false)
	if err != nil || hidden != 3 {
		t.Fatalf("DeleteChat = %d, %v; want 3 messages across phone and LID rows", hidden, err)
	}
	m := onlyPatchIndex(t, f, appstate.WAPatchRegularHigh)
	if want := []string{"deleteChat", chatTestLID.String(), "0"}; !slices.Equal(m.Index, want) {
		t.Fatalf("index = %q, want the requested JID %q", m.Index, want)
	}
	if ts := m.Value.GetDeleteChatAction().GetMessageRange().GetLastMessageTimestamp(); ts != chatTestBase.Add(time.Hour).Unix() {
		t.Fatalf("range timestamp = %d, want now", ts)
	}
	if fetches := appStateFetches(f); len(fetches) == 0 || fetches[0].name != string(appstate.WAPatchRegularHigh) || fetches[0].fullSync {
		t.Fatalf("fetches = %+v, want a regular_high catch-up before the write", fetches)
	}
	msg, err := a.db.GetMessage(chatTestPN.String(), "M1")
	if err != nil || msg.DeletedAt == nil || msg.DeletionReason != store.MessageDeletionReasonWhatsAppDeleteChat {
		t.Fatalf("M1 = %+v, %v", msg, err)
	}
	chats, err := a.db.ListChatsFiltered(store.ChatListFilter{})
	if err != nil || len(chats) != 0 {
		t.Fatalf("chats = %+v, %v; want the deleted chat hidden", chats, err)
	}
}

func TestDeleteChatFailureLeavesLocalHistory(t *testing.T) {
	a, f := newChatActionsTestApp(t)
	seedChatActionMessages(t, a, chatTestPN, "M1")
	f.appStatePatchErr = context.DeadlineExceeded
	if _, err := a.DeleteChat(context.Background(), chatTestPN, true); err == nil {
		t.Fatal("DeleteChat succeeded despite a send error")
	}
	if msg, _ := a.db.GetMessage(chatTestPN.String(), "M1"); msg.DeletedAt != nil {
		t.Fatalf("failed delete tombstoned %+v", msg)
	}
}

func TestClearChatKeepsStarredByDefault(t *testing.T) {
	a, f := newChatActionsTestApp(t)
	seedChatActionMessages(t, a, chatTestGroup, "G1", "G2")
	if err := a.db.SetStarred(store.SetStarredParams{ChatJID: chatTestGroup.String(), MsgID: "G1", Starred: true}); err != nil {
		t.Fatal(err)
	}
	hidden, err := a.ClearChat(context.Background(), chatTestGroup, false, true)
	if err != nil || hidden != 1 {
		t.Fatalf("ClearChat = %d, %v", hidden, err)
	}
	m := onlyPatchIndex(t, f, appstate.WAPatchRegularHigh)
	if want := []string{"clearChat", chatTestGroup.String(), "0", "1"}; !slices.Equal(m.Index, want) || m.Version != 6 {
		t.Fatalf("mutation = %q v%d", m.Index, m.Version)
	}
	if kept, _ := a.db.GetMessage(chatTestGroup.String(), "G1"); kept.DeletedAt != nil {
		t.Fatal("starred message cleared")
	}
	if chats, _ := a.db.ListChatsFiltered(store.ChatListFilter{}); len(chats) != 1 || chats[0].ClearedAt == nil {
		t.Fatalf("chats after clear = %+v", chats)
	}
}

func TestLockChatOverwritesThePhonesLockEntry(t *testing.T) {
	a, f := newChatActionsTestApp(t)
	seedChatActionMessages(t, a, chatTestPN, "M1")
	// The phone locked the chat under its LID.
	a.handleAppStatePersistenceEvent(context.Background(), &events.AppState{
		Index:           []string{"lock", chatTestLID.String()},
		SyncActionValue: &waSyncAction.SyncActionValue{LockChatAction: &waSyncAction.LockChatAction{Locked: proto.Bool(true)}},
	}, nil)
	if err := a.appStatePersist.waitIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c, _ := a.db.GetChat(chatTestPN.String()); !c.Locked {
		t.Fatalf("phone lock not mirrored: %+v", c)
	}
	if err := a.LockChat(context.Background(), chatTestPN, false); err != nil {
		t.Fatalf("LockChat: %v", err)
	}
	m := onlyPatchIndex(t, f, appstate.WAPatchRegularLow)
	if want := []string{"lock", chatTestLID.String()}; !slices.Equal(m.Index, want) || m.Version != 7 || m.Value.GetLockChatAction().GetLocked() {
		t.Fatalf("unlock mutation = %q v%d %+v; want the phone's LID entry", m.Index, m.Version, m.Value.GetLockChatAction())
	}
	if c, _ := a.db.GetChat(chatTestPN.String()); c.Locked {
		t.Fatalf("chat still locked locally: %+v", c)
	}
}

func TestStarMessageSendsPatchAndUpdatesStarred(t *testing.T) {
	a, f := newChatActionsTestApp(t)
	seedChatActionMessages(t, a, chatTestGroup, "G1")
	sender := types.NewJID("15550000002", types.DefaultUserServer)
	ref := MessageRef{Chat: chatTestGroup, ID: "G1", Sender: sender}
	if err := a.StarMessage(context.Background(), ref, true); err != nil {
		t.Fatalf("StarMessage: %v", err)
	}
	m := onlyPatchIndex(t, f, appstate.WAPatchRegularHigh)
	if want := []string{"star", chatTestGroup.String(), "G1", "0", sender.String()}; !slices.Equal(m.Index, want) || !m.Value.GetStarAction().GetStarred() {
		t.Fatalf("star mutation = %q %+v", m.Index, m.Value.GetStarAction())
	}
	starred, err := a.db.ListStarredMessages(store.ListStarredMessagesParams{})
	if err != nil || len(starred) != 1 || starred[0].MsgID != "G1" {
		t.Fatalf("starred = %+v, %v", starred, err)
	}
	if err := a.StarMessage(context.Background(), ref, false); err != nil {
		t.Fatal(err)
	}
	if starred, _ := a.db.ListStarredMessages(store.ListStarredMessagesParams{}); len(starred) != 0 {
		t.Fatalf("starred after unstar = %+v", starred)
	}
}

func TestSetChatFavoriteReplaysFullListFirst(t *testing.T) {
	a, f := newChatActionsTestApp(t)
	f.appStateFetchEvent = func(name string, fullSync, _ bool) any {
		if name != string(appstate.WAPatchRegularHigh) || !fullSync {
			return nil
		}
		// The phone's existing favorites, only visible in a full snapshot.
		return &events.AppState{
			Index: []string{"favorites"},
			SyncActionValue: &waSyncAction.SyncActionValue{FavoritesAction: &waSyncAction.FavoritesAction{
				Favorites: []*waSyncAction.FavoritesAction_Favorite{{ID: proto.String(chatTestGroup.String())}},
			}},
		}
	}
	if err := a.SetChatFavorite(context.Background(), chatTestPN, true); err != nil {
		t.Fatalf("SetChatFavorite: %v", err)
	}
	fetches := appStateFetches(f)
	if !slices.ContainsFunc(fetches, func(x fakeAppStateFetch) bool { return x.name == string(appstate.WAPatchRegularHigh) && x.fullSync }) {
		t.Fatalf("fetches = %+v, want a full regular_high replay before writing favorites", fetches)
	}
	m := onlyPatchIndex(t, f, appstate.WAPatchRegularHigh)
	var ids []string
	for _, fav := range m.Value.GetFavoritesAction().GetFavorites() {
		ids = append(ids, fav.GetID())
	}
	if want := []string{chatTestGroup.String(), chatTestPN.String()}; !slices.Equal(ids, want) || !slices.Equal(m.Index, []string{"favorites"}) {
		t.Fatalf("favorites patch = %q %q, want the existing favorite kept", m.Index, ids)
	}

	if known, _ := a.db.FavoritesKnown(); !known {
		t.Fatal("favorites not known after the replayed write")
	}
	// Once the list is known, removing one does not force a replay. (Clear the
	// replay debt every write leaves, which the next sync would pay.)
	if err := a.db.ClearAppStateRecoveryRequired(string(appstate.WAPatchRegularHigh)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.appStatePatches = nil
	f.appStateFetches = nil
	f.mu.Unlock()
	if err := a.SetChatFavorite(context.Background(), chatTestGroup, false); err != nil {
		t.Fatal(err)
	}
	for _, fetch := range appStateFetches(f) {
		if fetch.fullSync {
			t.Fatalf("second favorite write replayed: %+v", fetch)
		}
	}
	m = onlyPatchIndex(t, f, appstate.WAPatchRegularHigh)
	if len(m.Value.GetFavoritesAction().GetFavorites()) != 1 || m.Value.GetFavoritesAction().GetFavorites()[0].GetID() != chatTestPN.String() {
		t.Fatalf("favorites after removal = %+v", m.Value.GetFavoritesAction())
	}
	favs, _ := a.db.ListFavorites()
	if len(favs) != 1 || favs[0].ChatJID != chatTestPN.String() {
		t.Fatalf("stored favorites = %+v", favs)
	}
}

func TestCreateChatListAllocatesUnusedIDAfterFullReplay(t *testing.T) {
	a, f := newChatActionsTestApp(t)
	custom := waSyncAction.LabelEditAction_CUSTOM
	f.appStateFetchEvent = func(name string, fullSync, _ bool) any {
		if name != string(appstate.WAPatchRegular) || !fullSync {
			return nil
		}
		return &events.LabelEdit{LabelID: "4", Timestamp: chatTestBase, Action: &waSyncAction.LabelEditAction{
			Name: proto.String("Existing list"), Color: proto.Int32(0), OrderIndex: proto.Int32(3), Type: &custom,
		}}
	}
	created, err := a.CreateChatList(context.Background(), "Test list")
	if err != nil {
		t.Fatalf("CreateChatList: %v", err)
	}
	if created.ID != "5" || created.Name != "Test list" {
		t.Fatalf("created = %+v, want id 5 after the replayed list 4", created)
	}
	m := onlyPatchIndex(t, f, appstate.WAPatchRegular)
	action := m.Value.GetLabelEditAction()
	if !slices.Equal(m.Index, []string{"label_edit", "5"}) || action.GetName() != "Test list" || action.GetType() != custom ||
		action.GetOrderIndex() != 4 || action.GetColor() != 1 || action.GetDeleted() {
		t.Fatalf("create mutation = %q %+v", m.Index, action)
	}
	if mirrored, _ := a.db.AppStateMirrored(string(appstate.WAPatchRegular)); !mirrored {
		t.Fatal("regular not marked mirrored after the replay")
	}

	// A duplicate name is refused without sending anything, and without
	// forcing another replay once the debt the write left is paid.
	if err := a.db.ClearAppStateRecoveryRequired(string(appstate.WAPatchRegular)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.appStatePatches = nil
	f.appStateFetches = nil
	f.mu.Unlock()
	if _, err := a.CreateChatList(context.Background(), " test LIST "); err == nil {
		t.Fatal("duplicate list name accepted")
	}
	if patches := sentPatches(f); len(patches) != 0 {
		t.Fatalf("duplicate sent %+v", patches)
	}
	for _, fetch := range appStateFetches(f) {
		if fetch.fullSync {
			t.Fatalf("mirrored lists replayed again: %+v", fetch)
		}
	}
}

func TestRenameAndDeleteChatListKeepPhoneFields(t *testing.T) {
	a, f := newChatActionsTestApp(t)
	if err := a.db.MarkAppStateMirrored(string(appstate.WAPatchRegular), chatTestBase); err != nil {
		t.Fatal(err)
	}
	custom, favorites := int32(waSyncAction.LabelEditAction_CUSTOM), int32(waSyncAction.LabelEditAction_FAVORITES)
	order, active := int32(1), true
	for _, l := range []store.ChatList{
		{ID: "8", Name: "Test list", Color: 2, OrderIndex: &order, IsActive: &active, ListType: &custom},
		{ID: "3", Name: "Favorites", ListType: &favorites},
	} {
		if err := a.db.UpsertChatList(l); err != nil {
			t.Fatal(err)
		}
	}
	renamed, err := a.RenameChatList(context.Background(), "test list", "Renamed list")
	if err != nil || renamed.ID != "8" || renamed.Name != "Renamed list" {
		t.Fatalf("RenameChatList = %+v, %v", renamed, err)
	}
	action := onlyPatchIndex(t, f, appstate.WAPatchRegular).Value.GetLabelEditAction()
	if action.GetName() != "Renamed list" || action.GetColor() != 2 || action.GetOrderIndex() != 1 || !action.GetIsActive() || action.GetType() != waSyncAction.LabelEditAction_CUSTOM {
		t.Fatalf("rename action = %+v, want every other field kept", action)
	}
	f.mu.Lock()
	f.appStatePatches = nil
	f.mu.Unlock()
	if _, err := a.DeleteChatList(context.Background(), "8"); err != nil {
		t.Fatal(err)
	}
	if action := onlyPatchIndex(t, f, appstate.WAPatchRegular).Value.GetLabelEditAction(); !action.GetDeleted() || action.GetName() != "Renamed list" {
		t.Fatalf("delete action = %+v", action)
	}
	if lists, _ := a.db.ListChatLists(false); len(lists) != 1 || lists[0].ID != "3" {
		t.Fatalf("live lists = %+v", lists)
	}
	if _, err := a.RenameChatList(context.Background(), "Favorites", "Mine"); err == nil {
		t.Fatal("renamed the predefined favorites list")
	}
	if _, err := a.DeleteChatList(context.Background(), "No such list"); err == nil {
		t.Fatal("deleted a missing list")
	}
}

func TestSetChatListMemberUsesPhoneIndexEntries(t *testing.T) {
	a, f := newChatActionsTestApp(t)
	if err := a.db.MarkAppStateMirrored(string(appstate.WAPatchRegular), chatTestBase); err != nil {
		t.Fatal(err)
	}
	custom := int32(waSyncAction.LabelEditAction_CUSTOM)
	if err := a.db.UpsertChatList(store.ChatList{ID: "8", Name: "Test list", ListType: &custom}); err != nil {
		t.Fatal(err)
	}
	if err := a.db.SetChatListMember(store.ChatListMember{ListID: "8", ChatJID: chatTestPN.String(), RawJID: chatTestLID.String(), Labeled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetChatListMember(context.Background(), "Test list", chatTestPN, false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	m := onlyPatchIndex(t, f, appstate.WAPatchRegular)
	if want := []string{"label_jid", "8", chatTestLID.String()}; !slices.Equal(m.Index, want) || m.Value.GetLabelAssociationAction().GetLabeled() {
		t.Fatalf("remove mutation = %q %+v; want the phone's LID entry", m.Index, m.Value.GetLabelAssociationAction())
	}
	if members, _ := a.db.ChatListMembers("8", true); len(members) != 0 {
		t.Fatalf("members after remove = %+v", members)
	}
	f.mu.Lock()
	f.appStatePatches = nil
	f.mu.Unlock()
	if _, err := a.SetChatListMember(context.Background(), "8", chatTestGroup, true); err != nil {
		t.Fatalf("add: %v", err)
	}
	if m := onlyPatchIndex(t, f, appstate.WAPatchRegular); !slices.Equal(m.Index, []string{"label_jid", "8", chatTestGroup.String()}) || !m.Value.GetLabelAssociationAction().GetLabeled() {
		t.Fatalf("add mutation = %q", m.Index)
	}
}

// Changes made on the phone reach the store through the sync handler.
func TestSyncMirrorsChatDeleteClearLockListsAndFavorites(t *testing.T) {
	a, f := newChatActionsTestApp(t)
	seedChatActionMessages(t, a, chatTestPN, "M1", "M2")
	seedChatActionMessages(t, a, chatTestGroup, "G1", "G2", "G3")
	if err := a.db.SetStarred(store.SetStarredParams{ChatJID: chatTestGroup.String(), MsgID: "G1", Starred: true}); err != nil {
		t.Fatal(err)
	}

	var messagesStored, lastEvent atomic.Int64
	handlerID, _ := a.addSyncEventHandler(context.Background(), SyncOptions{Mode: SyncModeFollow}, &messagesStored, &lastEvent,
		make(chan struct{}, 1), make(chan struct{}, 1), make(chan staleReconnectRequest, 1), func(string, string) {}, nil, nil, &syncPresence{}, nil)
	defer f.RemoveEventHandler(handlerID)

	through := chatTestBase.Add(2 * time.Minute)
	clearRange := &waSyncAction.SyncActionMessageRange{LastMessageTimestamp: proto.Int64(through.Unix())}
	custom := waSyncAction.LabelEditAction_CUSTOM
	for _, evt := range []any{
		// Deleted on the phone under the chat's LID, up to M2.
		&events.DeleteChat{JID: chatTestLID, Timestamp: chatTestBase.Add(10 * time.Minute), Action: &waSyncAction.DeleteChatAction{MessageRange: clearRange}},
		// Cleared up to G2 with "delete starred": whatsmeow emits the raw value first.
		&events.AppState{Index: []string{"clearChat", chatTestGroup.String(), "1", "0"}, SyncActionValue: &waSyncAction.SyncActionValue{
			Timestamp: proto.Int64(chatTestBase.Add(10 * time.Minute).UnixMilli()), ClearChatAction: &waSyncAction.ClearChatAction{MessageRange: clearRange},
		}},
		&events.ClearChat{JID: chatTestGroup, Timestamp: chatTestBase.Add(10 * time.Minute), Action: &waSyncAction.ClearChatAction{MessageRange: clearRange}},
		&events.AppState{Index: []string{"lock", chatTestGroup.String()}, SyncActionValue: &waSyncAction.SyncActionValue{LockChatAction: &waSyncAction.LockChatAction{Locked: proto.Bool(true)}}},
		&events.LabelEdit{LabelID: "6", Timestamp: chatTestBase, Action: &waSyncAction.LabelEditAction{Name: proto.String("Test list"), Type: &custom, OrderIndex: proto.Int32(0)}},
		&events.LabelAssociationChat{LabelID: "6", JID: chatTestLID, Timestamp: chatTestBase, Action: &waSyncAction.LabelAssociationAction{Labeled: proto.Bool(true)}},
		&events.AppState{Index: []string{"favorites"}, SyncActionValue: &waSyncAction.SyncActionValue{
			Timestamp: proto.Int64(chatTestBase.UnixMilli()),
			FavoritesAction: &waSyncAction.FavoritesAction{Favorites: []*waSyncAction.FavoritesAction_Favorite{
				{ID: proto.String(chatTestLID.String())}, {ID: proto.String(chatTestGroup.String())},
			}},
		}},
		&events.AppStateSyncComplete{Name: appstate.WAPatchRegular},
	} {
		f.emit(evt)
	}
	if err := a.appStatePersist.waitIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"M1", "M2"} {
		if m, _ := a.db.GetMessage(chatTestPN.String(), id); m.DeletedAt == nil || m.DeletionReason != store.MessageDeletionReasonWhatsAppDeleteChat {
			t.Fatalf("%s after phone delete = %+v", id, m)
		}
	}
	for id, deleted := range map[string]bool{"G1": true, "G2": true, "G3": false} {
		if m, _ := a.db.GetMessage(chatTestGroup.String(), id); (m.DeletedAt != nil) != deleted {
			t.Fatalf("%s deleted=%v, want %t (G1 is starred but the phone cleared starred too)", id, m.DeletedAt, deleted)
		}
	}
	group, err := a.db.GetChat(chatTestGroup.String())
	if err != nil || !group.Locked || group.ClearedAt == nil {
		t.Fatalf("group after phone lock/clear = %+v, %v", group, err)
	}
	if chats, _ := a.db.ListChatsFiltered(store.ChatListFilter{}); len(chats) != 1 || chats[0].JID != chatTestGroup.String() {
		t.Fatalf("listed chats = %+v; want the deleted DM hidden", chats)
	}
	members, err := a.db.ChatListMembers("6", true)
	if err != nil || len(members) != 1 || members[0].ChatJID != chatTestPN.String() || members[0].RawJID != chatTestLID.String() {
		t.Fatalf("list members = %+v, %v; want the LID stored under the phone chat", members, err)
	}
	favs, _ := a.db.ListFavorites()
	if len(favs) != 2 || favs[0].ChatJID != chatTestPN.String() || favs[0].RawJID != chatTestLID.String() || favs[1].ChatJID != chatTestGroup.String() {
		t.Fatalf("favorites = %+v", favs)
	}
	if mirrored, _ := a.db.AppStateMirrored(string(appstate.WAPatchRegular)); !mirrored {
		t.Fatal("full regular sync not recorded")
	}
	for _, collection := range appstate.AllPatchNames {
		if required, _ := a.db.AppStateRecoveryRequired(string(collection)); required {
			t.Fatalf("successful persistence left %s replay debt", collection)
		}
	}
}

func TestChatAppStateEventCollections(t *testing.T) {
	for _, tc := range []struct {
		evt  any
		want []appstate.WAPatchName
	}{
		{&events.DeleteChat{JID: chatTestPN}, []appstate.WAPatchName{appstate.WAPatchRegularHigh}},
		{&events.ClearChat{JID: chatTestPN}, []appstate.WAPatchName{appstate.WAPatchRegularHigh}},
		{&events.LabelEdit{LabelID: "1"}, []appstate.WAPatchName{appstate.WAPatchRegular}},
		{&events.LabelAssociationChat{LabelID: "1"}, []appstate.WAPatchName{appstate.WAPatchRegular}},
		{&events.AppState{Index: []string{"lock", chatTestPN.String()}, SyncActionValue: &waSyncAction.SyncActionValue{LockChatAction: &waSyncAction.LockChatAction{}}}, []appstate.WAPatchName{appstate.WAPatchRegularLow}},
		{&events.AppState{Index: []string{"favorites"}, SyncActionValue: &waSyncAction.SyncActionValue{FavoritesAction: &waSyncAction.FavoritesAction{}}}, []appstate.WAPatchName{appstate.WAPatchRegularHigh}},
		{&events.AppState{Index: []string{"clearChat", chatTestPN.String(), "1", "0"}, SyncActionValue: &waSyncAction.SyncActionValue{}}, []appstate.WAPatchName{appstate.WAPatchRegularHigh}},
		{&events.AppState{Index: []string{"clearChat", chatTestPN.String(), "0", "0"}, SyncActionValue: &waSyncAction.SyncActionValue{}}, nil},
		{&events.AppStateSyncComplete{Name: appstate.WAPatchRegular}, nil},
	} {
		if got := appStateCollectionsForEvent(tc.evt); !slices.Equal(got, tc.want) {
			t.Fatalf("collections for %T = %v, want %v", tc.evt, got, tc.want)
		}
	}
}

func TestLiveSyncMirrorsPinInChat(t *testing.T) {
	a, f := newChatActionsTestApp(t)
	_ = f
	seedChatActionMessages(t, a, chatTestGroup, "G1")
	sender := types.NewJID("15550000002", types.DefaultUserServer)
	pinAt := chatTestBase.Add(5 * time.Minute)
	pin := func(id string, pinType waE2E.PinInChatMessage_Type, at time.Time) *events.Message {
		return &events.Message{
			Info: types.MessageInfo{
				MessageSource: types.MessageSource{Chat: chatTestGroup, Sender: sender, IsGroup: true},
				ID:            types.MessageID(id), Timestamp: at,
			},
			Message: &waProto.Message{
				PinInChatMessage: &waE2E.PinInChatMessage{
					Key:               &waCommon.MessageKey{RemoteJID: proto.String(chatTestGroup.String()), ID: proto.String("G1"), FromMe: proto.Bool(false)},
					Type:              pinType.Enum(),
					SenderTimestampMS: proto.Int64(at.UnixMilli()),
				},
				MessageContextInfo: &waE2E.MessageContextInfo{MessageAddOnDurationInSecs: proto.Uint32(86400)},
			},
		}
	}
	var stored atomic.Int64
	a.handleLiveSyncMessage(context.Background(), SyncOptions{}, pin("PIN01", waE2E.PinInChatMessage_PIN_FOR_ALL, pinAt), &stored, func(string, string) {}, nil)
	pins, err := a.db.ListPinnedMessages([]string{chatTestGroup.String()}, pinAt)
	if err != nil || len(pins) != 1 || pins[0].MsgID != "G1" || pins[0].PinnedBy != sender.String() || pins[0].ExpiresAt == nil || !pins[0].ExpiresAt.Equal(pinAt.Add(24*time.Hour)) {
		t.Fatalf("pins = %+v, %v", pins, err)
	}
	a.handleLiveSyncMessage(context.Background(), SyncOptions{}, pin("PIN02", waE2E.PinInChatMessage_UNPIN_FOR_ALL, pinAt.Add(time.Minute)), &stored, func(string, string) {}, nil)
	if pins, _ := a.db.ListPinnedMessages(nil, pinAt); len(pins) != 0 {
		t.Fatalf("pins after unpin = %+v", pins)
	}
	// Pin notices are protocol traffic, not unread messages.
	if c, _ := a.db.GetChat(chatTestGroup.String()); c.UnreadCount != 0 {
		t.Fatalf("pin notices counted as unread: %+v", c)
	}
}

func TestChatListTypeHelpers(t *testing.T) {
	typ := func(v waSyncAction.LabelEditAction_ListType) *int32 { n := int32(v); return &n }
	for _, v := range []waSyncAction.LabelEditAction_ListType{waSyncAction.LabelEditAction_UNREAD, waSyncAction.LabelEditAction_GROUPS, waSyncAction.LabelEditAction_LOCKED} {
		if !IsComputedListType(typ(v)) {
			t.Fatalf("%s not computed", v)
		}
	}
	for _, v := range []waSyncAction.LabelEditAction_ListType{waSyncAction.LabelEditAction_CUSTOM, waSyncAction.LabelEditAction_FAVORITES, waSyncAction.LabelEditAction_PREDEFINED, waSyncAction.LabelEditAction_NONE} {
		if IsComputedListType(typ(v)) {
			t.Fatalf("%s computed", v)
		}
	}
	if IsComputedListType(nil) || ChatListTypeName(nil) != "none" || ChatListTypeName(typ(waSyncAction.LabelEditAction_CUSTOM)) != "custom" {
		t.Fatal("untyped list helpers")
	}
	if !IsFavoritesListType(typ(waSyncAction.LabelEditAction_FAVORITES)) || IsFavoritesListType(typ(waSyncAction.LabelEditAction_CUSTOM)) {
		t.Fatal("favorites type helper")
	}
}
