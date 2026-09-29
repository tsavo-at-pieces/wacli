package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/store"
)

// seedExpiredVoiceNote adds a voice note whose CDN copy has expired (the
// harness download fails with HTTP 403 for "/v/expired").
func seedExpiredVoiceNote(t *testing.T, storeDir string) {
	t.Helper()
	db, err := store.Open(filepath.Join(storeDir, "wacli.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	if err := db.UpsertMessage(store.UpsertMessageParams{
		ChatJID: tChat, MsgID: "voice-expired", SenderJID: tChat, SenderName: "Synthetic Contact",
		Timestamp: transcribeFixtureBase.Add(-time.Hour), Text: "[Audio]", MediaType: "audio", MimeType: "audio/ogg; codecs=opus",
		DirectPath: "/v/expired", MediaKey: []byte{1, 2, 3},
	}); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
}

// With a sync process running, expired audio is recovered through it (a
// single-message media retry) and transcribed from the recovered file.
func TestMediaTranscribeRecoversExpiredAudioThroughSync(t *testing.T) {
	storeDir := shortPresenceDelegateStoreDir(t)
	h := newTranscribeHarnessAt(t, storeDir)
	seedExpiredVoiceNote(t, storeDir)
	d := startJobDaemon(t, storeDir)
	recovered := filepath.Join(storeDir, "media", "recovered-voice.ogg")
	d.fake.configure(func(f *fakeJobApp) {
		f.retry = func(ctx context.Context, opts app.RetryMediaOptions) (app.MediaRetryResult, error) {
			if err := os.WriteFile(recovered, []byte("fictional recovered ogg"), 0o600); err != nil {
				return app.MediaRetryResult{}, err
			}
			if err := f.DB().MarkMediaDownloaded(opts.ChatJID, opts.MsgID, recovered, time.Now()); err != nil {
				return app.MediaRetryResult{}, err
			}
			return app.MediaRetryResult{Requested: 1, Recovered: 1, Outcomes: []app.MediaRetryOutcome{
				{ChatJID: opts.ChatJID, MsgID: opts.MsgID, Status: "recovered", Path: recovered, Bytes: 23},
			}}, nil
		}
	})
	t.Setenv("WACLI_TEST_TRANSCRIPT", "made-up words from a recovered note")

	flags := &rootFlags{storeDir: storeDir, asJSON: true}
	deps := h.deps(t)
	deps.recoverExpired = func(ctx context.Context, chatJID, msgID string) (string, error) {
		return recoverExpiredThroughSync(ctx, flags, chatJID, msgID)
	}
	raw, err := runCommandJSON(t, newMediaTranscribeCmdWithDeps(flags, deps), "--chat", tChat, "--id", "voice-expired")
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	res := decodeData[transcribeResult](t, raw)
	if res.Source != "retry" || res.Transcript != "made-up words from a recovered note" || res.Cached {
		t.Fatalf("result = %+v", res)
	}
	req := d.lastRequest(t)
	if req.Kind != mediaRetryKind || req.Chat != tChat || req.ID != "voice-expired" {
		t.Fatalf("request = %+v", req)
	}
	got := d.fake.lastRetry(t)
	if got.ChatJID != tChat || got.MsgID != "voice-expired" || got.BatchSize != 1 || got.Wait != transcribeRetryWait || got.Limit != 0 || len(got.MediaTypes) != 0 {
		t.Fatalf("retry options = %+v, want exactly this message", got)
	}
	if strings.Join(h.downloads, ",") != "voice-expired" {
		t.Fatalf("downloads = %v, want the one failed CDN attempt", h.downloads)
	}
	if stored, ok := storedTranscript(t, storeDir, tChat, "voice-expired"); !ok || stored.Text != res.Transcript {
		t.Fatalf("stored transcript = %+v, %v", stored, ok)
	}
}

func TestMediaTranscribeExpiredAudioWithoutRecovery(t *testing.T) {
	retryHint := "wacli media retry --chat " + tChat + " --id voice-expired"
	for _, tc := range []struct {
		name  string
		flags func(storeDir string) *rootFlags
		want  []string
	}{
		{
			name:  "no sync process",
			flags: func(storeDir string) *rootFlags { return &rootFlags{storeDir: storeDir, asJSON: true} },
			want:  []string{"status code 403", "no `wacli sync --follow` is running", retryHint},
		},
		{
			name:  "read-only",
			flags: func(storeDir string) *rootFlags { return &rootFlags{storeDir: storeDir, asJSON: true, readOnly: true} },
			want:  []string{"status code 403", "read-only mode does not ask the phone", retryHint + "` without --read-only"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTranscribeHarness(t)
			seedExpiredVoiceNote(t, h.storeDir)
			flags := tc.flags(h.storeDir)
			deps := h.deps(t)
			deps.recoverExpired = func(ctx context.Context, chatJID, msgID string) (string, error) {
				return recoverExpiredThroughSync(ctx, flags, chatJID, msgID)
			}
			_, err := runCommandJSON(t, newMediaTranscribeCmdWithDeps(flags, deps), "--chat", tChat, "--id", "voice-expired")
			if err == nil {
				t.Fatal("transcribe of expired audio succeeded without recovery")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %v, want %q", err, want)
				}
			}
		})
	}
}

// Only expired media is worth a retry; other download failures are reported
// as they are, and a pending run records the hint with the failure.
func TestMediaTranscribeRecoveryOnlyForExpiredMedia(t *testing.T) {
	h := newTranscribeHarness(t)
	seedExpiredVoiceNote(t, h.storeDir)
	var asked []string
	deps := h.deps(t)
	deps.recoverExpired = func(_ context.Context, chatJID, msgID string) (string, error) {
		asked = append(asked, chatJID+"/"+msgID)
		return "", errSendDelegateUnavailable
	}
	t.Setenv("WACLI_TEST_TRANSCRIPT", "made-up pending words")
	raw, err := runCommandJSON(t, newMediaTranscribeCmdWithDeps(&rootFlags{storeDir: h.storeDir, asJSON: true}, deps), "--pending")
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	rep := decodeData[transcribePendingReport](t, raw)
	if len(asked) != 1 || asked[0] != tChat+"/voice-expired" {
		t.Fatalf("recovery asked for %v, want only the expired note", asked)
	}
	for _, r := range rep.Results {
		switch r.ID {
		case "voice-expired":
			if r.Status != transcribeStatusFailed || !strings.Contains(r.Detail, "wacli media retry --chat "+tChat+" --id voice-expired") {
				t.Fatalf("expired result = %+v", r)
			}
		case "voice-broken":
			if r.Status != transcribeStatusFailed || strings.Contains(r.Detail, "media retry") {
				t.Fatalf("broken result = %+v, want the plain download error", r)
			}
		}
	}
}
