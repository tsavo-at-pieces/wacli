package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Fictional identities only.
const (
	testDMPN  = "15550000001@s.whatsapp.net"
	testDMLID = "100000000001@lid"
	testGroup = "120363000000000001@g.us"
)

func seedChatMessages(t *testing.T, db *DB, chat string, base time.Time, ids ...string) {
	t.Helper()
	if err := db.UpsertChat(chat, "dm", "Test chat", base.Add(time.Duration(len(ids))*time.Minute)); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	for i, id := range ids {
		if err := db.UpsertMessage(UpsertMessageParams{
			ChatJID: chat, MsgID: id, SenderJID: chat, Timestamp: base.Add(time.Duration(i+1) * time.Minute), Text: "Fictional note " + id,
		}); err != nil {
			t.Fatalf("UpsertMessage %s: %v", id, err)
		}
	}
}

func TestOpenMigratesChatActionsAndLists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wacli.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	seedChatMessages(t, db, testDMPN, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), "M1")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Rewind to a store from before migration 30.
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DELETE FROM schema_migrations WHERE version = 30`,
		`ALTER TABLE chats DROP COLUMN locked`,
		`ALTER TABLE chats DROP COLUMN lock_jid`,
		`ALTER TABLE chats DROP COLUMN deleted_at`,
		`ALTER TABLE chats DROP COLUMN cleared_at`,
		`DROP TABLE chat_lists`,
		`DROP TABLE chat_list_members`,
		`DROP TABLE chat_favorites`,
		`DROP TABLE message_pins`,
		`DROP TABLE app_state_mirrors`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			_ = raw.Close()
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatalf("Open migrated: %v", err)
	}
	defer db.Close()
	cols, err := tableColumns(db.sql, "chats")
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"locked", "lock_jid", "deleted_at", "cleared_at"} {
		if !cols[col] {
			t.Fatalf("chats.%s missing after migration", col)
		}
	}
	for _, table := range []string{"chat_lists", "chat_list_members", "chat_favorites", "message_pins", "app_state_mirrors"} {
		if ok, err := db.tableExists(table); err != nil || !ok {
			t.Fatalf("table %s exists = %t, %v", table, ok, err)
		}
	}
	c, err := db.GetChat(testDMPN)
	if err != nil || c.Locked || c.DeletedAt != nil || c.ClearedAt != nil {
		t.Fatalf("migrated chat = %+v, %v", c, err)
	}
	if _, err := db.GetMessage(testDMPN, "M1"); err != nil {
		t.Fatalf("message lost in migration: %v", err)
	}
	// The migration is idempotent, as ensure-style repairs require.
	if err := migrateChatActionsAndLists(db); err != nil {
		t.Fatalf("second migration run: %v", err)
	}
}

func TestMarkChatDeletedTombstonesAndHidesUntilNewMessage(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seedChatMessages(t, db, testDMPN, base, "M1", "M2")
	seedChatMessages(t, db, testDMLID, base, "L1")
	if err := db.SetStarred(SetStarredParams{ChatJID: testDMPN, MsgID: "M1", Starred: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetChatUnreadCount(testDMPN, 2); err != nil {
		t.Fatal(err)
	}
	through := base.Add(10 * time.Minute)
	n, err := db.MarkChatDeleted([]string{testDMPN, testDMLID}, through, through)
	if err != nil || n != 3 {
		t.Fatalf("MarkChatDeleted = %d, %v; want 3 tombstones across both identities", n, err)
	}
	for _, ref := range [][2]string{{testDMPN, "M1"}, {testDMPN, "M2"}, {testDMLID, "L1"}} {
		m, err := db.GetMessage(ref[0], ref[1])
		if err != nil {
			t.Fatalf("GetMessage %v: %v", ref, err)
		}
		if m.DeletedAt == nil || m.DeletionReason != MessageDeletionReasonWhatsAppDeleteChat || !m.DeletedForMe || m.Text == "" {
			t.Fatalf("message %v = %+v; want a tombstone that keeps its text", ref, m)
		}
	}
	if listed, err := db.ListMessages(ListMessagesParams{ChatJIDs: []string{testDMPN, testDMLID}}); err != nil || len(listed) != 0 {
		t.Fatalf("listed after delete = %+v, %v", listed, err)
	}
	if starred, err := db.ListStarredMessages(ListStarredMessagesParams{}); err != nil || len(starred) != 0 {
		t.Fatalf("starred after delete = %+v, %v", starred, err)
	}

	chats, err := db.ListChatsFiltered(ChatListFilter{})
	if err != nil || len(chats) != 0 {
		t.Fatalf("chats after delete = %+v, %v; want deleted chats hidden", chats, err)
	}
	deleted := true
	chats, err = db.ListChatsFiltered(ChatListFilter{Deleted: &deleted})
	if err != nil || len(chats) != 2 || chats[0].DeletedAt == nil || chats[0].UnreadCount != 0 || chats[0].Unread {
		t.Fatalf("deleted chats = %+v, %v", chats, err)
	}
	c, err := db.GetChat(testDMPN)
	if err != nil || c.DeletedAt == nil || !c.DeletedAt.Equal(through) {
		t.Fatalf("GetChat deleted = %+v, %v", c, err)
	}

	// A newer message brings the chat back; the old messages stay tombstones.
	if err := db.UpsertChat(testDMPN, "dm", "Test chat", through.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMessage(UpsertMessageParams{ChatJID: testDMPN, MsgID: "M3", SenderJID: testDMPN, Timestamp: through.Add(time.Minute), Text: "Fictional new note"}); err != nil {
		t.Fatal(err)
	}
	chats, err = db.ListChatsFiltered(ChatListFilter{})
	if err != nil || len(chats) != 1 || chats[0].JID != testDMPN || chats[0].DeletedAt != nil {
		t.Fatalf("chats after new message = %+v, %v", chats, err)
	}
	listed, err := db.ListMessages(ListMessagesParams{ChatJID: testDMPN})
	if err != nil || len(listed) != 1 || listed[0].MsgID != "M3" {
		t.Fatalf("messages after new message = %+v, %v", listed, err)
	}
	// Re-ingesting a tombstoned message does not resurrect it.
	if err := db.UpsertMessage(UpsertMessageParams{ChatJID: testDMPN, MsgID: "M1", SenderJID: testDMPN, Timestamp: base.Add(time.Minute), Text: "Fictional note M1"}); err != nil {
		t.Fatal(err)
	}
	if m, _ := db.GetMessage(testDMPN, "M1"); m.DeletedAt == nil {
		t.Fatal("re-ingested message lost its tombstone")
	}
}

func TestMarkChatClearedKeepsStarredUnlessAsked(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seedChatMessages(t, db, testGroup, base, "G1", "G2", "G3")
	if err := db.SetStarred(SetStarredParams{ChatJID: testGroup, MsgID: "G1", Starred: true}); err != nil {
		t.Fatal(err)
	}
	through := base.Add(2 * time.Minute) // covers G1 and G2 only
	n, err := db.MarkChatCleared([]string{testGroup}, through, through, false)
	if err != nil || n != 1 {
		t.Fatalf("clear keeping starred = %d, %v; want only G2", n, err)
	}
	for id, wantDeleted := range map[string]bool{"G1": false, "G2": true, "G3": false} {
		m, err := db.GetMessage(testGroup, id)
		if err != nil || (m.DeletedAt != nil) != wantDeleted {
			t.Fatalf("%s deleted=%v, want %t (%v)", id, m.DeletedAt, wantDeleted, err)
		}
		if wantDeleted && m.DeletionReason != MessageDeletionReasonWhatsAppClearChat {
			t.Fatalf("%s reason = %q", id, m.DeletionReason)
		}
	}
	// A cleared chat stays listed.
	chats, err := db.ListChatsFiltered(ChatListFilter{})
	if err != nil || len(chats) != 1 || chats[0].ClearedAt == nil || chats[0].DeletedAt != nil {
		t.Fatalf("chats after clear = %+v, %v", chats, err)
	}
	n, err = db.MarkChatCleared([]string{testGroup}, through, through, true)
	if err != nil || n != 1 {
		t.Fatalf("clear with starred = %d, %v; want G1", n, err)
	}
	if _, err := db.MarkChatCleared(nil, through, through, false); err == nil {
		t.Fatal("clear without a chat accepted")
	}
}

func TestChatLockStateAndFilter(t *testing.T) {
	db := openTestDB(t)
	seedChatMessages(t, db, testDMPN, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), "M1")
	seedChatMessages(t, db, testGroup, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), "G1")
	if err := db.SetChatLocked(testDMPN, testDMLID, true); err != nil {
		t.Fatal(err)
	}
	if raw, err := db.ChatLockJID(testDMPN); err != nil || raw != testDMLID {
		t.Fatalf("lock JID = %q, %v", raw, err)
	}
	locked := true
	chats, err := db.ListChatsFiltered(ChatListFilter{Locked: &locked})
	if err != nil || len(chats) != 1 || chats[0].JID != testDMPN || !chats[0].Locked {
		t.Fatalf("locked chats = %+v, %v", chats, err)
	}
	// Unlocking without a JID keeps the one the phone used.
	if err := db.SetChatLocked(testDMPN, "", false); err != nil {
		t.Fatal(err)
	}
	if raw, _ := db.ChatLockJID(testDMPN); raw != testDMLID {
		t.Fatalf("lock JID after unlock = %q", raw)
	}
	if c, _ := db.GetChat(testDMPN); c.Locked {
		t.Fatal("chat still locked")
	}
	if raw, err := db.ChatLockJID("15550000009@s.whatsapp.net"); err != nil || raw != "" {
		t.Fatalf("unknown chat lock JID = %q, %v", raw, err)
	}
	chats, err = db.ListChatsFiltered(ChatListFilter{JIDs: []string{testGroup}})
	if err != nil || len(chats) != 1 || chats[0].JID != testGroup {
		t.Fatalf("JID filter = %+v, %v", chats, err)
	}
	if chats, err := db.ListChatsFiltered(ChatListFilter{JIDs: []string{}}); err != nil || len(chats) != 0 {
		t.Fatalf("empty JID filter = %+v, %v; want no chats", chats, err)
	}
}

func TestChatListsMembersAndFavorites(t *testing.T) {
	db := openTestDB(t)
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	order, custom, favoritesType := int32(2), int32(5), int32(3)
	if err := db.UpsertChatList(ChatList{ID: "3", Name: "‎Favorites", ListType: &favoritesType, UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertChatList(ChatList{ID: "9", Name: "Test list", Color: 4, OrderIndex: &order, ListType: &custom, UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertChatList(ChatList{ID: "11", Name: "Old list", ListType: &custom, Deleted: true, UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	live, err := db.ListChatLists(false)
	if err != nil || len(live) != 2 {
		t.Fatalf("live lists = %+v, %v", live, err)
	}
	all, _ := db.ListChatLists(true)
	if len(all) != 3 {
		t.Fatalf("all lists = %+v", all)
	}
	got, err := db.GetChatList("9")
	if err != nil || got.Name != "Test list" || got.Color != 4 || *got.OrderIndex != 2 || *got.ListType != 5 || got.IsActive != nil || got.PredefinedID != nil {
		t.Fatalf("GetChatList = %+v, %v", got, err)
	}
	for ref, want := range map[string]string{"9": "9", "test LIST": "9", " favorites ": "3"} {
		if l, ok := FindChatList(live, ref); !ok || l.ID != want {
			t.Fatalf("FindChatList(%q) = %+v, %t; want %s", ref, l, ok, want)
		}
	}
	if _, ok := FindChatList(live, "Old list"); ok {
		t.Fatal("found a deleted list among live lists")
	}
	// New IDs skip every stored ID, deleted lists included.
	if id, err := db.NextChatListID(); err != nil || id != "12" {
		t.Fatalf("NextChatListID = %q, %v", id, err)
	}

	for _, m := range []ChatListMember{
		{ListID: "9", ChatJID: testDMPN, RawJID: testDMLID, Labeled: true, UpdatedAt: at},
		{ListID: "9", ChatJID: testGroup, RawJID: testGroup, Labeled: true, UpdatedAt: at},
		{ListID: "9", ChatJID: testGroup, RawJID: testGroup, Labeled: false, UpdatedAt: at.Add(time.Minute)},
	} {
		if err := db.SetChatListMember(m); err != nil {
			t.Fatal(err)
		}
	}
	members, err := db.ChatListMembers("9", true)
	if err != nil || len(members) != 1 || members[0].ChatJID != testDMPN || members[0].RawJID != testDMLID {
		t.Fatalf("labeled members = %+v, %v; the removal must win", members, err)
	}
	if everything, _ := db.ChatListMembers("9", false); len(everything) != 2 {
		t.Fatalf("all members = %+v", everything)
	}

	if known, _ := db.FavoritesKnown(); known {
		t.Fatal("favorites known before any value")
	}
	favs := []FavoriteChat{{ChatJID: testGroup, RawJID: testGroup}, {ChatJID: testDMPN, RawJID: testDMLID}}
	if err := db.ReplaceFavorites(favs, at); err != nil {
		t.Fatal(err)
	}
	gotFavs, err := db.ListFavorites()
	if err != nil || !slices.Equal(gotFavs, favs) {
		t.Fatalf("favorites = %+v, %v; want phone order", gotFavs, err)
	}
	if known, _ := db.FavoritesKnown(); !known {
		t.Fatal("favorites unknown after a value")
	}
	if err := db.ReplaceFavorites(nil, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if gotFavs, _ := db.ListFavorites(); len(gotFavs) != 0 {
		t.Fatalf("favorites after empty value = %+v", gotFavs)
	}

	if ok, _ := db.AppStateMirrored("regular"); ok {
		t.Fatal("regular mirrored before a full sync")
	}
	if err := db.MarkAppStateMirrored("regular", at); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.AppStateMirrored("regular"); !ok {
		t.Fatal("regular not mirrored after marking")
	}
}

func TestFavoritesKnownAfterFullRegularHighSync(t *testing.T) {
	db := openTestDB(t)
	if err := db.MarkAppStateMirrored("regular_high", time.Now()); err != nil {
		t.Fatal(err)
	}
	if known, err := db.FavoritesKnown(); err != nil || !known {
		t.Fatalf("FavoritesKnown = %t, %v", known, err)
	}
}

func TestMessagePinsExpireAndOrder(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	future, past := now.Add(time.Hour), now.Add(-time.Hour)
	for _, p := range []MessagePin{
		{ChatJID: testGroup, MsgID: "G1", Pinned: true, PinnedBy: testDMPN, ChangedAt: now.Add(-2 * time.Minute), ExpiresAt: &future},
		{ChatJID: testGroup, MsgID: "G2", Pinned: true, ChangedAt: now.Add(-time.Minute)},
		{ChatJID: testGroup, MsgID: "G3", Pinned: true, ChangedAt: now.Add(-3 * time.Hour), ExpiresAt: &past},
		{ChatJID: testDMPN, MsgID: "M1", Pinned: true, ChangedAt: now.Add(-time.Minute), ExpiresAt: &future},
	} {
		if err := db.SetMessagePin(p); err != nil {
			t.Fatal(err)
		}
	}
	pins, err := db.ListPinnedMessages([]string{testGroup}, now)
	if err != nil || len(pins) != 2 || pins[0].MsgID != "G2" || pins[1].MsgID != "G1" || pins[1].PinnedBy != testDMPN || pins[0].ExpiresAt != nil {
		t.Fatalf("group pins = %+v, %v; want unexpired pins newest first", pins, err)
	}
	// An older pin event does not undo a newer unpin.
	if err := db.SetMessagePin(MessagePin{ChatJID: testGroup, MsgID: "G2", Pinned: false, ChangedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMessagePin(MessagePin{ChatJID: testGroup, MsgID: "G2", Pinned: true, ChangedAt: now.Add(-30 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	pins, _ = db.ListPinnedMessages(nil, now)
	if len(pins) != 2 {
		t.Fatalf("all pins = %+v; want G1 and M1", pins)
	}
	if err := db.SetMessagePin(MessagePin{ChatJID: testGroup}); err == nil {
		t.Fatal("pin without a message accepted")
	}
}
