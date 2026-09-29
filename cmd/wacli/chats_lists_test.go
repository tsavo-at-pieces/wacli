package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
)

// seedListsStore writes a store as sync would after mirroring lists,
// favorites, a lock, a deleted chat and a pin. Fictional data only.
func seedListsStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for i, c := range []struct{ jid, kind, name string }{
		{mgmtPhone, "dm", "Sam"},
		{mgmtOtherPhone, "dm", "Samantha"},
		{mgmtGroup, "group", "Test group"},
		{mgmtJoined, "group", "Old group"},
	} {
		if err := db.UpsertChat(c.jid, c.kind, c.name, base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: c.jid, MsgID: "MSG0" + string(rune('1'+i)), SenderJID: c.jid, Timestamp: base.Add(time.Duration(i) * time.Minute), Text: "Fictional note"}); err != nil {
			t.Fatal(err)
		}
	}
	custom, favorites, unread := int32(5), int32(3), int32(1)
	order := int32(1)
	for _, l := range []store.ChatList{
		{ID: "1", Name: "‎Unread", ListType: &unread},
		{ID: "3", Name: "‎Favorites", ListType: &favorites},
		{ID: "6", Name: "Test list", Color: 2, OrderIndex: &order, ListType: &custom},
		{ID: "7", Name: "Gone list", ListType: &custom, Deleted: true},
	} {
		if err := db.UpsertChatList(l); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []store.ChatListMember{
		{ListID: "6", ChatJID: mgmtPhone, RawJID: mgmtLID, Labeled: true},
		{ListID: "6", ChatJID: mgmtGroup, RawJID: mgmtGroup, Labeled: true},
		{ListID: "6", ChatJID: mgmtOtherPhone, RawJID: mgmtOtherPhone, Labeled: false},
	} {
		if err := db.SetChatListMember(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.ReplaceFavorites([]store.FavoriteChat{{ChatJID: mgmtGroup, RawJID: mgmtGroup}}, base); err != nil {
		t.Fatal(err)
	}
	if err := db.SetChatLocked(mgmtOtherPhone, mgmtOtherPhone, true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MarkChatDeleted([]string{mgmtJoined}, base.Add(time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	if err := db.SetMessagePin(store.MessagePin{ChatJID: mgmtGroup, MsgID: "MSG03", Pinned: true, PinnedBy: mgmtPhone, ChangedAt: time.Now().Add(-time.Minute), ExpiresAt: &expires}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// runLocalRead runs a read command while another process holds the store
// lock, as sync --follow does: local reads must not need it.
func runLocalRead(t *testing.T, storeDir string, args ...string) string {
	t.Helper()
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatalf("lock store: %v", err)
	}
	defer lk.Release()
	var runErr error
	stdout := captureRootStdout(t, func() {
		captureRootStderr(t, func() {
			runErr = execute(append([]string{"--store", storeDir, "--timeout", "5s"}, args...))
		})
	})
	if runErr != nil {
		t.Fatalf("%v: %v", args, runErr)
	}
	return stdout
}

func decodeListsData(t *testing.T, stdout string, data any) {
	t.Helper()
	var envelope struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil || !envelope.Success {
		t.Fatalf("output %q: %v", stdout, err)
	}
	if err := json.Unmarshal(envelope.Data, data); err != nil {
		t.Fatalf("data %s: %v", envelope.Data, err)
	}
}

func TestChatsListsShowsListsWithMembers(t *testing.T) {
	dir := seedListsStore(t)
	var lists []chatListView
	decodeListsData(t, runLocalRead(t, dir, "--json", "chats", "lists"), &lists)
	byID := map[string]chatListView{}
	for _, l := range lists {
		byID[l.ID] = l
	}
	if len(lists) != 3 {
		t.Fatalf("lists = %+v, want unread, favorites and the custom list (not the deleted one)", lists)
	}
	if l := byID["6"]; l.Name != "Test list" || l.Type != "custom" || l.Count != 2 || l.Members[0] != mgmtGroup || l.Members[1] != mgmtPhone {
		t.Fatalf("custom list = %+v", l)
	}
	if l := byID["3"]; l.Name != "Favorites" || l.Type != "favorites" || l.Count != 1 || l.Members[0] != mgmtGroup {
		t.Fatalf("favorites = %+v", l)
	}
	if l := byID["1"]; !l.Computed || l.Count != 0 || l.Type != "unread" {
		t.Fatalf("unread list = %+v", l)
	}
	var all []chatListView
	decodeListsData(t, runLocalRead(t, dir, "--json", "chats", "lists", "--include-deleted"), &all)
	if len(all) != 4 {
		t.Fatalf("lists with deleted = %d", len(all))
	}
	human := runLocalRead(t, dir, "chats", "lists")
	for _, want := range []string{"Test list", "custom", "(computed)"} {
		if !strings.Contains(human, want) {
			t.Fatalf("human lists %q missing %q", human, want)
		}
	}
}

func TestChatsListFiltersByListLockAndDeletion(t *testing.T) {
	dir := seedListsStore(t)
	jids := func(args ...string) []string {
		var chats []store.Chat
		decodeListsData(t, runLocalRead(t, dir, append([]string{"--json", "chats", "list"}, args...)...), &chats)
		var out []string
		for _, c := range chats {
			out = append(out, c.JID)
		}
		return out
	}
	if got := jids(); strings.Join(got, ",") != strings.Join([]string{mgmtGroup, mgmtOtherPhone, mgmtPhone}, ",") {
		t.Fatalf("default list = %v; want the deleted chat hidden", got)
	}
	if got := jids("--list", "test list"); strings.Join(got, ",") != mgmtGroup+","+mgmtPhone {
		t.Fatalf("--list = %v", got)
	}
	if got := jids("--list", "favorites"); strings.Join(got, ",") != mgmtGroup {
		t.Fatalf("--list favorites = %v", got)
	}
	if got := jids("--list", "3"); strings.Join(got, ",") != mgmtGroup {
		t.Fatalf("--list by favorites ID = %v", got)
	}
	if got := jids("--locked"); strings.Join(got, ",") != mgmtOtherPhone {
		t.Fatalf("--locked = %v", got)
	}
	if got := jids("--deleted"); strings.Join(got, ",") != mgmtJoined {
		t.Fatalf("--deleted = %v", got)
	}

	var chats []store.Chat
	decodeListsData(t, runLocalRead(t, dir, "--json", "chats", "list", "--locked"), &chats)
	if !chats[0].Locked {
		t.Fatalf("locked chat JSON = %+v", chats[0])
	}
	var shown store.Chat
	decodeListsData(t, runLocalRead(t, dir, "--json", "chats", "show", "--jid", mgmtJoined), &shown)
	if shown.DeletedAt == nil {
		t.Fatalf("show deleted chat = %+v", shown)
	}
	if human := runLocalRead(t, dir, "chats", "show", "--jid", mgmtOtherPhone); !strings.Contains(human, "Locked: true") {
		t.Fatalf("show human = %q", human)
	}
	if human := runLocalRead(t, dir, "chats", "list", "--locked"); !strings.Contains(human, "locked") {
		t.Fatalf("list human flags = %q", human)
	}

	for _, bad := range [][]string{{"--list", "No such list"}, {"--list", "unread"}} {
		lk, _ := lock.Acquire(dir)
		var err error
		captureRootStdout(t, func() {
			captureRootStderr(t, func() { err = execute(append([]string{"--store", dir, "chats", "list"}, bad...)) })
		})
		lk.Release()
		if err == nil {
			t.Fatalf("chats list %v succeeded", bad)
		}
	}
}

func TestMessagesPinnedListsCurrentPins(t *testing.T) {
	dir := seedListsStore(t)
	var pins []struct {
		ChatJID  string         `json:"chat_jid"`
		MsgID    string         `json:"msg_id"`
		PinnedBy string         `json:"pinned_by"`
		Message  *store.Message `json:"message"`
	}
	decodeListsData(t, runLocalRead(t, dir, "--json", "messages", "pinned", "--chat", mgmtGroup), &pins)
	if len(pins) != 1 || pins[0].MsgID != "MSG03" || pins[0].PinnedBy != mgmtPhone || pins[0].Message == nil || pins[0].Message.Text != "Fictional note" {
		t.Fatalf("pins = %+v", pins)
	}
	decodeListsData(t, runLocalRead(t, dir, "--json", "messages", "pinned", "--chat", mgmtPhone), &pins)
	if len(pins) != 0 {
		t.Fatalf("pins in another chat = %+v", pins)
	}
	if human := runLocalRead(t, dir, "messages", "pinned"); !strings.Contains(human, "MSG03") {
		t.Fatalf("human pins = %q", human)
	}
}
