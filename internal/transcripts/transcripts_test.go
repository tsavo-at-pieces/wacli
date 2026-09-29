package transcripts

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	testChat = "15550000001@s.whatsapp.net"
	testLID  = "100000000001@lid"
)

// get looks one message up through ByMessageIDs, the production read path.
func get(t *testing.T, db *DB, chatJID, msgID string) (Transcript, bool) {
	t.Helper()
	rows, err := db.ByMessageIDs([]string{msgID})
	if err != nil {
		t.Fatalf("ByMessageIDs: %v", err)
	}
	for _, r := range rows {
		if r.ChatJID == chatJID {
			return r, true
		}
	}
	return Transcript{}, false
}

func openTest(t *testing.T) (*DB, string) {
	t.Helper()
	path := PathFor(t.TempDir())
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func TestOpenCreatesPrivateWALDatabase(t *testing.T) {
	db, path := openTest(t)
	if filepath.Base(path) != "transcripts.db" {
		t.Fatalf("path = %s", path)
	}
	if err := db.Upsert(Transcript{ChatJID: testChat, MsgID: "m1", Text: "made-up words", Engine: "command"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		if os.IsNotExist(err) && p != path {
			continue
		}
		if err != nil {
			t.Fatalf("Stat %s: %v", p, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %04o, want 0600", filepath.Base(p), got)
		}
	}
	var mode string
	if err := db.sql.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}

	// Reopening keeps existing rows (Open never truncates).
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	if _, ok := get(t, again, testChat, "m1"); !ok {
		t.Fatal("row missing after reopen")
	}
}

func TestUpsertGetAndOverwrite(t *testing.T) {
	db, _ := openTest(t)
	first := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := db.Upsert(Transcript{ChatJID: testChat, MsgID: "m1", Text: "first pass", Engine: "nemo-speech", Model: "parakeet-tdt", DurationMS: 4200, CreatedAt: first}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, ok := get(t, db, testChat, "m1")
	if !ok {
		t.Fatal("row missing")
	}
	want := Transcript{ChatJID: testChat, MsgID: "m1", Text: "first pass", Engine: "nemo-speech", Model: "parakeet-tdt", DurationMS: 4200, CreatedAt: first}
	if got != want {
		t.Fatalf("Get = %+v, want %+v", got, want)
	}

	// --force re-transcription replaces the row.
	second := first.Add(time.Hour)
	if err := db.Upsert(Transcript{ChatJID: testChat, MsgID: "m1", Text: "", Engine: "command", CreatedAt: second}); err != nil {
		t.Fatalf("Upsert overwrite: %v", err)
	}
	got, ok = get(t, db, testChat, "m1")
	if !ok {
		t.Fatal("overwritten row missing")
	}
	if got.Text != "" || got.Engine != "command" || got.Model != "" || got.DurationMS != 0 || !got.CreatedAt.Equal(second) {
		t.Fatalf("overwritten = %+v", got)
	}

	if _, ok := get(t, db, testChat, "missing"); ok {
		t.Fatal("found a transcript that was never stored")
	}
	if _, ok := get(t, db, testLID, "m1"); ok {
		t.Fatal("a transcript matched a different chat")
	}
	if err := db.Upsert(Transcript{ChatJID: testChat, MsgID: "m2", Text: "x"}); err == nil {
		t.Fatal("expected an error without an engine")
	}
	if err := db.Upsert(Transcript{MsgID: "m2", Text: "x", Engine: "command"}); err == nil {
		t.Fatal("expected an error without a chat")
	}
}

func TestByMessageIDsKeysAndSearch(t *testing.T) {
	db, _ := openTest(t)
	rows := []Transcript{
		{ChatJID: testChat, MsgID: "m1", Text: "Bring the Picnic blanket on Saturday", Engine: "command"},
		{ChatJID: testLID, MsgID: "m2", Text: "the picnic moved to sunday", Engine: "command"},
		{ChatJID: testChat, MsgID: "m3", Text: "100% sure about the_plan", Engine: "command"},
	}
	for _, r := range rows {
		if err := db.Upsert(r); err != nil {
			t.Fatalf("Upsert %s: %v", r.MsgID, err)
		}
	}

	got, err := db.ByMessageIDs([]string{"m2", "m1", "m1", "", "nope"})
	if err != nil {
		t.Fatalf("ByMessageIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ByMessageIDs = %+v", got)
	}

	keys, err := db.Keys()
	if err != nil || len(keys) != 3 {
		t.Fatalf("Keys = %+v, %v", keys, err)
	}

	for _, tc := range []struct {
		query string
		want  int
	}{
		{query: "picnic", want: 2},
		{query: "PICNIC saturday", want: 1},
		{query: `"picnic"`, want: 2},
		{query: "saturday picnic", want: 1},
		{query: "100%", want: 1},
		{query: "%", want: 1},
		{query: "the_plan", want: 1},
		{query: "_", want: 1},
		{query: "absent", want: 0},
		{query: "   ", want: 0},
	} {
		hits, err := db.Search(tc.query)
		if err != nil {
			t.Fatalf("Search(%q): %v", tc.query, err)
		}
		if len(hits) != tc.want {
			t.Fatalf("Search(%q) = %d hits, want %d", tc.query, len(hits), tc.want)
		}
	}
}

func TestFailuresAreCountedAndClearedBySuccess(t *testing.T) {
	db, _ := openTest(t)
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if err := db.RecordFailure(testChat, "m1", "download audio: 403", at); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if err := db.RecordFailure(testChat, "m1", "download audio: 404", at.Add(time.Minute)); err != nil {
		t.Fatalf("RecordFailure again: %v", err)
	}
	failures, err := db.Failures()
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	f := failures[Key{ChatJID: testChat, MsgID: "m1"}]
	if f.Attempts != 2 || f.Error != "download audio: 404" || !f.LastAttemptAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("failure = %+v", f)
	}
	if err := db.Upsert(Transcript{ChatJID: testChat, MsgID: "m1", Text: "ok", Engine: "command"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	failures, err = db.Failures()
	if err != nil || len(failures) != 0 {
		t.Fatalf("failures after success = %+v, %v", failures, err)
	}
}

func TestOpenReadOnlyNeverCreates(t *testing.T) {
	path := PathFor(t.TempDir())
	if _, err := OpenReadOnly(path); !IsNotExist(err) {
		t.Fatalf("OpenReadOnly missing = %v, want not-exist", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("OpenReadOnly created %s (stat err %v)", path, err)
	}

	// An empty file (created but never initialized) also reads as absent.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(path); !IsNotExist(err) {
		t.Fatalf("OpenReadOnly uninitialized = %v, want not-exist", err)
	}
}

func TestOpenReadOnlyReadsAndRejectsWrites(t *testing.T) {
	db, path := openTest(t)
	if err := db.Upsert(Transcript{ChatJID: testChat, MsgID: "m1", Text: "made-up words", Engine: "command"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	if _, ok := get(t, ro, testChat, "m1"); !ok {
		t.Fatal("read-only handle cannot read the row")
	}
	if err := ro.Upsert(Transcript{ChatJID: testChat, MsgID: "m2", Text: "x", Engine: "command"}); err == nil {
		t.Fatal("read-only handle accepted a write")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(path + suffix); err == nil {
			t.Fatalf("read-only open left %s behind", suffix)
		}
	}
}

func TestOpenRejectsURIInjection(t *testing.T) {
	for _, p := range []string{filepath.Join(t.TempDir(), "x.db?mode=memory"), filepath.Join(t.TempDir(), "x#y.db")} {
		if _, err := Open(p); err == nil {
			t.Fatalf("Open(%q) accepted a URI-like path", p)
		}
		if _, err := OpenReadOnly(p); err == nil {
			t.Fatalf("OpenReadOnly(%q) accepted a URI-like path", p)
		}
	}
}
