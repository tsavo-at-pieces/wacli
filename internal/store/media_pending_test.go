package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMediaTypeFilter(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
		err  string
	}{
		{in: "audio", want: []string{"audio"}},
		{in: "video", want: []string{"video", "gif"}},
		{in: " Image , gif,image", want: []string{"image", "gif"}},
		{in: "sticker,document", want: []string{"sticker", "document"}},
		{in: "voice", err: `unknown media type "voice"`},
		{in: " , ", err: "no media type given"},
	} {
		got, err := MediaTypeFilter(tc.in)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("MediaTypeFilter(%q) error = %v, want %q", tc.in, err, tc.err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) {
			t.Fatalf("MediaTypeFilter(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
}

func TestListPendingMediaFilters(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	chatA := "15550000001@s.whatsapp.net"
	chatB := "120363000000000001@g.us"
	base := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct{ jid, kind string }{{chatA, "dm"}, {chatB, "group"}} {
		if err := db.UpsertChat(c.jid, c.kind, "Test chat", base); err != nil {
			t.Fatalf("UpsertChat: %v", err)
		}
	}
	add := func(chat, id, kind string, ts time.Time, local string) {
		t.Helper()
		if err := db.UpsertMessage(UpsertMessageParams{
			ChatJID: chat, MsgID: id, SenderJID: chatA, Timestamp: ts,
			MediaType: kind, DirectPath: "/direct/" + id, MediaKey: []byte{1},
		}); err != nil {
			t.Fatalf("UpsertMessage %s: %v", id, err)
		}
		if local != "" {
			if err := db.MarkMediaDownloaded(chat, id, local, ts); err != nil {
				t.Fatalf("MarkMediaDownloaded %s: %v", id, err)
			}
		}
	}
	add(chatA, "a-img", "image", base, "")
	add(chatA, "a-voice", "audio", base.Add(time.Hour), "")
	add(chatA, "a-done", "audio", base.Add(2*time.Hour), "/tmp/fictional.ogg")
	add(chatB, "b-voice", "audio", base.Add(3*time.Hour), "")
	add(chatB, "b-vid", "video", base.Add(4*time.Hour), "")
	if err := db.MarkMediaUnavailable(ctx, chatB, "b-vid", base); err != nil {
		t.Fatalf("MarkMediaUnavailable: %v", err)
	}

	ids := func(f PendingMediaFilter) []string {
		t.Helper()
		rows, err := db.ListPendingMedia(ctx, f)
		if err != nil {
			t.Fatalf("ListPendingMedia(%+v): %v", f, err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.MsgID)
		}
		return out
	}
	// Without filters it is the pending scan.
	all, err := db.ListPendingMediaDownloads(ctx, "", 0)
	if err != nil {
		t.Fatalf("ListPendingMediaDownloads: %v", err)
	}
	var want []string
	for _, r := range all {
		want = append(want, r.MsgID)
	}
	if got := ids(PendingMediaFilter{}); !slices.Equal(got, want) || !slices.Equal(got, []string{"b-voice", "a-voice", "a-img"}) {
		t.Fatalf("unfiltered = %v, pending scan = %v", got, want)
	}
	for _, tc := range []struct {
		name string
		f    PendingMediaFilter
		want []string
	}{
		{"type", PendingMediaFilter{MediaTypes: []string{"audio"}}, []string{"b-voice", "a-voice"}},
		{"type and chat", PendingMediaFilter{ChatJID: chatA, MediaTypes: []string{"audio"}}, []string{"a-voice"}},
		{"type and before", PendingMediaFilter{MediaTypes: []string{"audio", "image"}, BeforeUnix: base.Add(2 * time.Hour).Unix(), BeforeSet: true}, []string{"a-voice", "a-img"}},
		{"epoch before", PendingMediaFilter{MediaTypes: []string{"audio"}, BeforeSet: true}, nil},
		{"limit", PendingMediaFilter{MediaTypes: []string{"audio", "image"}, Limit: 2}, []string{"b-voice", "a-voice"}},
		{"unavailable skipped", PendingMediaFilter{MediaTypes: []string{"video"}}, nil},
	} {
		if got := ids(tc.f); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
