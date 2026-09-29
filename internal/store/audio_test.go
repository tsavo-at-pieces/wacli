package store

import (
	"strings"
	"testing"
	"time"
)

func seedAudioFixture(t *testing.T, db *DB) (chat, other string, base time.Time) {
	t.Helper()
	chat = "15550000001@s.whatsapp.net"
	other = "15550000002@s.whatsapp.net"
	base = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for _, jid := range []string{chat, other} {
		if err := db.UpsertChat(jid, "dm", "Synthetic", base); err != nil {
			t.Fatalf("UpsertChat: %v", err)
		}
	}
	rows := []UpsertMessageParams{
		{ChatJID: chat, MsgID: "a-old", SenderJID: chat, Timestamp: base, Text: "[Audio]", MediaType: "audio", DirectPath: "/v/fake-1", MediaKey: []byte{1}},
		{ChatJID: chat, MsgID: "a-new", SenderJID: chat, Timestamp: base.Add(2 * time.Hour), Text: "[Audio]", MediaType: "audio"},
		{ChatJID: chat, MsgID: "a-upper", SenderJID: chat, Timestamp: base.Add(time.Hour), Text: "[Audio]", MediaType: "AUDIO", DirectPath: "/v/fake-2"},
		{ChatJID: chat, MsgID: "text", SenderJID: chat, Timestamp: base.Add(3 * time.Hour), Text: "made-up text"},
		{ChatJID: chat, MsgID: "image", SenderJID: chat, Timestamp: base.Add(4 * time.Hour), MediaType: "image"},
		{ChatJID: chat, MsgID: "a-deleted", SenderJID: chat, Timestamp: base.Add(5 * time.Hour), MediaType: "audio", Revoked: true},
		{ChatJID: other, MsgID: "a-other", SenderJID: other, Timestamp: base.Add(6 * time.Hour), Text: "[Audio]", MediaType: "audio", DirectPath: "/v/fake-3", MediaKey: []byte{3}},
	}
	for _, row := range rows {
		if err := db.UpsertMessage(row); err != nil {
			t.Fatalf("UpsertMessage %s: %v", row.MsgID, err)
		}
	}
	return chat, other, base
}

func audioIDs(msgs []AudioMessage) string {
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.MsgID)
	}
	return strings.Join(ids, ",")
}

func TestListAudioMessages(t *testing.T) {
	db := openTestDB(t)
	chat, other, base := seedAudioFixture(t, db)

	all, err := db.ListAudioMessages(ListAudioMessagesParams{})
	if err != nil {
		t.Fatalf("ListAudioMessages: %v", err)
	}
	if got := audioIDs(all); got != "a-other,a-new,a-upper,a-old" {
		t.Fatalf("all audio = %s", got)
	}
	downloadable := map[string]bool{}
	for _, m := range all {
		downloadable[m.MsgID] = m.Downloadable
	}
	if !downloadable["a-old"] || downloadable["a-new"] || downloadable["a-upper"] || !downloadable["a-other"] {
		t.Fatalf("downloadable = %v", downloadable)
	}

	scoped, err := db.ListAudioMessages(ListAudioMessagesParams{ChatJIDs: []string{chat}})
	if err != nil || audioIDs(scoped) != "a-new,a-upper,a-old" {
		t.Fatalf("chat scoped = %s, %v", audioIDs(scoped), err)
	}
	after := base.Add(30 * time.Minute)
	before := base.Add(5 * time.Hour)
	windowed, err := db.ListAudioMessages(ListAudioMessagesParams{ChatJIDs: []string{chat, other}, After: &after, Before: &before})
	if err != nil || audioIDs(windowed) != "a-new,a-upper" {
		t.Fatalf("windowed = %s, %v", audioIDs(windowed), err)
	}

	if err := db.MarkMediaUnavailable(storeCtx(), chat, "a-old", base); err != nil {
		t.Fatalf("MarkMediaUnavailable: %v", err)
	}
	scoped, err = db.ListAudioMessages(ListAudioMessagesParams{ChatJIDs: []string{chat}})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range scoped {
		if m.MsgID == "a-old" && m.Downloadable {
			t.Fatal("media the phone reported gone is still downloadable")
		}
	}
}

func TestSearchAudioMessagesByIDAppliesFilters(t *testing.T) {
	db := openTestDB(t)
	chat, other, base := seedAudioFixture(t, db)
	ids := []string{"a-old", "a-new", "a-other", "text", "image", "a-deleted", "missing"}

	got, err := db.SearchAudioMessagesByID(ids, SearchMessagesParams{})
	if err != nil {
		t.Fatalf("SearchAudioMessagesByID: %v", err)
	}
	if messageIDsOf(got) != "a-other,a-new,a-old" {
		t.Fatalf("unfiltered = %s", messageIDsOf(got))
	}

	got, err = db.SearchAudioMessagesByID(ids, SearchMessagesParams{ChatJIDs: []string{chat}})
	if err != nil || messageIDsOf(got) != "a-new,a-old" {
		t.Fatalf("chat filter = %s, %v", messageIDsOf(got), err)
	}
	got, err = db.SearchAudioMessagesByID(ids, SearchMessagesParams{From: other})
	if err != nil || messageIDsOf(got) != "a-other" {
		t.Fatalf("from filter = %s, %v", messageIDsOf(got), err)
	}
	after := base.Add(time.Hour)
	got, err = db.SearchAudioMessagesByID(ids, SearchMessagesParams{After: &after, Type: "audio", HasMedia: true})
	if err != nil || messageIDsOf(got) != "a-other,a-new" {
		t.Fatalf("after/type filter = %s, %v", messageIDsOf(got), err)
	}
	if err := db.SetStarred(SetStarredParams{ChatJID: chat, MsgID: "a-old", SenderJID: chat, Starred: true, StarredAt: base}); err != nil {
		t.Fatalf("SetStarred: %v", err)
	}
	got, err = db.SearchAudioMessagesByID(ids, SearchMessagesParams{Starred: true})
	if err != nil || messageIDsOf(got) != "a-old" {
		t.Fatalf("starred filter = %s, %v", messageIDsOf(got), err)
	}
	got, err = db.SearchAudioMessagesByID(nil, SearchMessagesParams{})
	if err != nil || len(got) != 0 {
		t.Fatalf("no ids = %v, %v", got, err)
	}
}

func messageIDsOf(msgs []Message) string {
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.MsgID)
	}
	return strings.Join(ids, ",")
}
