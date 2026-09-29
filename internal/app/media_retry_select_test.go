package app

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow"
)

const retryTestChat = "15550000001@s.whatsapp.net"

func newRetrySelectTest(t *testing.T) (*App, *fakeWA) {
	t.Helper()
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	f.onMediaRetry = notOnPhoneHook
	f.downloadErr = whatsmeow.ErrMediaDownloadFailedWith403
	if err := a.db.UpsertChat(retryTestChat, "dm", "Test contact", time.Now()); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	return a, f
}

func insertTypedMedia(t *testing.T, a *App, id, mediaType string, ts time.Time) {
	t.Helper()
	if err := a.db.UpsertMessage(store.UpsertMessageParams{
		ChatJID:       retryTestChat,
		MsgID:         id,
		SenderJID:     retryTestChat,
		Timestamp:     ts,
		MediaType:     mediaType,
		MimeType:      "application/octet-stream",
		DirectPath:    "/direct/" + id,
		MediaKey:      []byte{1, 2, 3},
		FileSHA256:    []byte{4, 5},
		FileEncSHA256: []byte{6, 7},
		FileLength:    123,
	}); err != nil {
		t.Fatalf("UpsertMessage %s: %v", id, err)
	}
}

func TestRetryMediaByIDRetriesExactlyThatMessage(t *testing.T) {
	a, f := newRetrySelectTest(t)
	base := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	insertTypedMedia(t, a, "newest", "image", base.Add(2*time.Hour))
	insertTypedMedia(t, a, "target", "audio", base)
	// Named explicitly, a row the pending scan skips is still retried.
	if err := a.db.MarkMediaUnavailable(context.Background(), retryTestChat, "target", base); err != nil {
		t.Fatalf("MarkMediaUnavailable: %v", err)
	}
	f.downloadErr = nil // the stored CDN path still works

	res, err := a.RetryMedia(context.Background(), RetryMediaOptions{ChatJID: retryTestChat, MsgID: "target", Wait: time.Second})
	if err != nil {
		t.Fatalf("RetryMedia: %v", err)
	}
	if !slices.Equal(f.mediaRetryReceipts, []string{"target"}) {
		t.Fatalf("receipts = %v, want only the named message", f.mediaRetryReceipts)
	}
	if res.Requested != 1 || res.Recovered != 1 || len(res.Outcomes) != 1 || res.Outcomes[0].MsgID != "target" || res.Outcomes[0].Path == "" {
		t.Fatalf("result = %+v", res)
	}
	info, err := a.db.GetMediaDownloadInfo(retryTestChat, "target")
	if err != nil || info.LocalPath != res.Outcomes[0].Path {
		t.Fatalf("recorded local path = %q (err %v), want %q", info.LocalPath, err, res.Outcomes[0].Path)
	}
}

func TestRetryMediaByIDRejectsUnusableTargets(t *testing.T) {
	a, f := newRetrySelectTest(t)
	insertTypedMedia(t, a, "media", "image", time.Now())
	if err := a.db.UpsertMessage(store.UpsertMessageParams{ChatJID: retryTestChat, MsgID: "text", SenderJID: retryTestChat, Timestamp: time.Now(), Text: "Fictional note"}); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	insertTypedMedia(t, a, "gone", "image", time.Now())
	if err := a.db.MarkMessageDeletedForMePreserveMedia(retryTestChat, "gone"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, tc := range []struct {
		name, chat, id, want string
	}{
		{"no chat", "", "media", "--id requires --chat"},
		{"unknown message", retryTestChat, "missing", "message missing not found in chat " + retryTestChat},
		{"other chat", "15550000002@s.whatsapp.net", "media", "not found"},
		{"no media", retryTestChat, "text", "has no downloadable media metadata"},
		{"deleted", retryTestChat, "gone", "was deleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.RetryMedia(context.Background(), RetryMediaOptions{ChatJID: tc.chat, MsgID: tc.id, Wait: time.Second})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	if len(f.mediaRetryReceipts) != 0 {
		t.Fatalf("receipts sent for rejected targets: %v", f.mediaRetryReceipts)
	}
}

func TestRetryMediaTypeFilterScopesThePendingScan(t *testing.T) {
	a, f := newRetrySelectTest(t)
	base := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	for i, m := range []struct{ id, kind string }{
		{"img", "image"}, {"vid", "video"}, {"loop", "gif"}, {"voice", "audio"}, {"doc", "document"}, {"stk", "sticker"},
	} {
		insertTypedMedia(t, a, m.id, m.kind, base.Add(time.Duration(i)*time.Minute))
	}
	types, err := store.MediaTypeFilter("video")
	if err != nil {
		t.Fatalf("MediaTypeFilter: %v", err)
	}
	res, err := a.RetryMedia(context.Background(), RetryMediaOptions{MediaTypes: types, Wait: time.Second})
	if err != nil {
		t.Fatalf("RetryMedia: %v", err)
	}
	// Newest first, as the unfiltered scan.
	if !slices.Equal(f.mediaRetryReceipts, []string{"loop", "vid"}) || res.Requested != 2 {
		t.Fatalf("receipts = %v, result = %+v; want the video and the gif", f.mediaRetryReceipts, res)
	}

	f.mediaRetryReceipts = nil
	res, err = a.RetryMedia(context.Background(), RetryMediaOptions{MediaTypes: []string{"audio", "image"}, Limit: 1, Wait: time.Second})
	if err != nil {
		t.Fatalf("RetryMedia: %v", err)
	}
	if !slices.Equal(f.mediaRetryReceipts, []string{"voice"}) || res.Requested != 1 {
		t.Fatalf("receipts = %v, result = %+v; want the newest of audio and image", f.mediaRetryReceipts, res)
	}
}

// Media operations run for another process stream their events to it and
// keep them out of this process's event stream.
func TestMediaOperationEventsGoToOperationSink(t *testing.T) {
	a, _ := newRetrySelectTest(t)
	var own bytes.Buffer
	a.opts.Events = out.NewEventWriter(&own, true)
	insertTypedMedia(t, a, "m1", "image", time.Now())
	var mu sync.Mutex
	var got []string
	ctx := WithOperationEvents(context.Background(), func(ev OperationEvent) {
		mu.Lock()
		defer mu.Unlock()
		name := ev.Event
		if code, _ := ev.Data["code"].(string); code != "" {
			name += ":" + code
		}
		got = append(got, name)
	})

	if _, err := a.BackfillMedia(ctx, BackfillMediaOptions{}); err != nil {
		t.Fatalf("BackfillMedia: %v", err)
	}
	if _, err := a.RetryMedia(ctx, RetryMediaOptions{Wait: time.Second}); err != nil {
		t.Fatalf("RetryMedia: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"media_backfill_start", "warning:media_download_failed", "media_backfill_done", "media_retry_progress"}
	if !slices.Equal(got, want) {
		t.Fatalf("streamed events = %v, want %v", got, want)
	}
	if own.Len() != 0 {
		t.Fatalf("own event stream got %s", &own)
	}
}
