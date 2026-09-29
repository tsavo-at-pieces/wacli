package store

import (
	"path/filepath"
	"testing"
	"time"
)

// Fictional identities only.
const (
	testStatusPN    = "15550000001@s.whatsapp.net"
	testStatusLID   = "100000000001@lid"
	testStatusOther = "15550000002@s.whatsapp.net"
)

func TestStatusMutesMigrationCreatesTable(t *testing.T) {
	db := openTestDB(t)
	if ok, err := db.tableExists("status_mutes"); err != nil || !ok {
		t.Fatalf("status_mutes exists = %t, %v", ok, err)
	}
	if got := countRows(t, db.sql, "SELECT COUNT(*) FROM schema_migrations WHERE version = 28 AND name = 'status mutes'"); got != 1 {
		t.Fatalf("migration 28 rows = %d, want 1", got)
	}
}

// A store created before the table gets it on the next writable open.
func TestStatusMutesMigrationUpgradesExistingStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wacli.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.sql.Exec(`DROP TABLE status_mutes; DELETE FROM schema_migrations WHERE version = 28`); err != nil {
		t.Fatalf("simulate older store: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Read-only access to the older store sees no mutes instead of failing.
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if mutes, err := ro.ListStatusMutes(true); err != nil || len(mutes) != 0 {
		t.Fatalf("ListStatusMutes on older store = %+v, %v", mutes, err)
	}
	if _, ok, err := ro.FindStatusMute(testStatusPN); err != nil || ok {
		t.Fatalf("FindStatusMute on older store = %t, %v", ok, err)
	}
	if muted, err := ro.MutedStatusJIDs(); err != nil || len(muted) != 0 {
		t.Fatalf("MutedStatusJIDs on older store = %v, %v", muted, err)
	}
	_ = ro.Close()

	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	if err := db.SetStatusMute(SetStatusMuteParams{JID: testStatusPN, Muted: true}); err != nil {
		t.Fatalf("SetStatusMute after upgrade: %v", err)
	}
}

func TestSetStatusMuteUpsertsAndFindsEitherIdentity(t *testing.T) {
	db := openTestDB(t)
	t1 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if err := db.SetStatusMute(SetStatusMuteParams{JID: testStatusPN, IndexJID: testStatusLID, Muted: true, UpdatedAt: t1}); err != nil {
		t.Fatalf("SetStatusMute: %v", err)
	}
	for _, lookup := range [][]string{{testStatusPN}, {testStatusLID}, {"", testStatusOther, testStatusLID}} {
		got, ok, err := db.FindStatusMute(lookup...)
		if err != nil || !ok {
			t.Fatalf("FindStatusMute(%q) = %t, %v", lookup, ok, err)
		}
		if got.JID != testStatusPN || got.IndexJID != testStatusLID || !got.Muted || !got.UpdatedAt.Equal(t1) {
			t.Fatalf("FindStatusMute(%q) = %+v", lookup, got)
		}
	}
	if _, ok, err := db.FindStatusMute(testStatusOther); err != nil || ok {
		t.Fatalf("unknown contact found = %t, %v", ok, err)
	}
	if _, ok, err := db.FindStatusMute(); err != nil || ok {
		t.Fatalf("empty lookup found = %t, %v", ok, err)
	}

	// Later mutations win in arrival order, whatever their timestamps.
	if err := db.SetStatusMute(SetStatusMuteParams{JID: testStatusPN, IndexJID: testStatusPN, Muted: false, UpdatedAt: t1.Add(-time.Hour)}); err != nil {
		t.Fatalf("SetStatusMute unmute: %v", err)
	}
	got, _, err := db.FindStatusMute(testStatusPN)
	if err != nil || got.Muted || got.IndexJID != testStatusPN {
		t.Fatalf("after unmute = %+v, %v", got, err)
	}
	if n := countRows(t, db.sql, "SELECT COUNT(*) FROM status_mutes"); n != 1 {
		t.Fatalf("rows = %d, want one per contact", n)
	}
}

func TestSetStatusMuteFillsMissingIdentity(t *testing.T) {
	db := openTestDB(t)
	if err := db.SetStatusMute(SetStatusMuteParams{IndexJID: testStatusLID, Muted: true}); err != nil {
		t.Fatalf("SetStatusMute index only: %v", err)
	}
	got, ok, err := db.FindStatusMute(testStatusLID)
	if err != nil || !ok || got.JID != testStatusLID || got.UpdatedAt.IsZero() {
		t.Fatalf("index-only mute = %+v, %t, %v", got, ok, err)
	}
	if err := db.SetStatusMute(SetStatusMuteParams{}); err == nil {
		t.Fatal("SetStatusMute without a JID succeeded")
	}
}

func TestListStatusMutesAndMutedJIDs(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i, m := range []SetStatusMuteParams{
		{JID: testStatusPN, IndexJID: testStatusLID, Muted: true, UpdatedAt: base},
		{JID: testStatusOther, Muted: false, UpdatedAt: base.Add(time.Minute)},
	} {
		if err := db.SetStatusMute(m); err != nil {
			t.Fatalf("SetStatusMute %d: %v", i, err)
		}
	}
	muted, err := db.ListStatusMutes(false)
	if err != nil || len(muted) != 1 || muted[0].JID != testStatusPN {
		t.Fatalf("ListStatusMutes(false) = %+v, %v", muted, err)
	}
	all, err := db.ListStatusMutes(true)
	if err != nil || len(all) != 2 || all[0].JID != testStatusOther {
		t.Fatalf("ListStatusMutes(true) = %+v, %v; want newest first", all, err)
	}
	jids, err := db.MutedStatusJIDs()
	if err != nil || !jids[testStatusPN] || !jids[testStatusLID] || jids[testStatusOther] {
		t.Fatalf("MutedStatusJIDs = %v, %v", jids, err)
	}
}

func TestListStatusMessagesFilters(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i, p := range []UpsertStatusMessageParams{
		{MsgID: "ST01", Timestamp: base, SenderJID: testStatusPN, SenderName: "Sam", Text: "first"},
		{MsgID: "ST02", Timestamp: base.Add(time.Hour), SenderJID: testStatusLID, Text: "second"},
		{MsgID: "ST03", Timestamp: base.Add(2 * time.Hour), SenderJID: testStatusOther, MediaType: "image", DirectPath: "/v/t62/fake", MediaKey: []byte("key")},
		{MsgID: "ST04", Timestamp: base.Add(3 * time.Hour), FromMe: true, Text: "mine"},
	} {
		if err := db.UpsertStatusMessage(p); err != nil {
			t.Fatalf("UpsertStatusMessage %d: %v", i, err)
		}
	}
	ids := func(rows []StatusMessage) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.MsgID)
		}
		return out
	}
	after := base.Add(30 * time.Minute)
	before := base.Add(150 * time.Minute)
	for _, tc := range []struct {
		name string
		p    ListStatusMessagesParams
		want []string
	}{
		{"all newest first", ListStatusMessagesParams{}, []string{"ST04", "ST03", "ST02", "ST01"}},
		{"limit", ListStatusMessagesParams{Limit: 2}, []string{"ST04", "ST03"}},
		{"both identities of one sender", ListStatusMessagesParams{SenderJIDs: []string{testStatusPN, testStatusLID}}, []string{"ST02", "ST01"}},
		{"after and before", ListStatusMessagesParams{After: &after, Before: &before}, []string{"ST03", "ST02"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.ListStatusMessages(tc.p)
			if err != nil {
				t.Fatalf("ListStatusMessages: %v", err)
			}
			got := ids(rows)
			if len(got) != len(tc.want) {
				t.Fatalf("ids = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ids = %v, want %v", got, tc.want)
				}
			}
		})
	}
	rows, err := db.ListStatusMessages(ListStatusMessagesParams{SenderJIDs: []string{testStatusOther}})
	if err != nil || len(rows) != 1 || rows[0].DirectPath != "/v/t62/fake" || string(rows[0].MediaKey) != "key" || rows[0].MediaType != "image" {
		t.Fatalf("media status = %+v, %v", rows, err)
	}
}
