package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/fsutil"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
)

// Fictional identities only.
const (
	jobChat  = "15550000001@s.whatsapp.net"
	jobGroup = "120363000000000001@g.us"
)

// fakeJobApp stands in for the sync process that owns the store. The store and
// media paths are real (an app.App without a WhatsApp client); downloads and
// the long operations are faked.
type fakeJobApp struct {
	*app.App
	wa *fakeJobWA

	mu        sync.Mutex
	retries   []app.RetryMediaOptions
	backfills []app.BackfillMediaOptions
	histories []app.BackfillOptions

	retry     func(context.Context, app.RetryMediaOptions) (app.MediaRetryResult, error)
	backfill  func(context.Context, app.BackfillMediaOptions) (app.BackfillMediaResult, error)
	history   func(context.Context, app.BackfillOptions) (app.BackfillResult, error)
	downloads atomic.Int32
}

type fakeJobWA struct {
	app.WAClient
	owner *fakeJobApp
	// hold, when set, blocks each download until it is closed or ctx ends.
	hold chan struct{}
}

func (w *fakeJobWA) DownloadMediaToFile(ctx context.Context, directPath string, _, _, _ []byte, _ uint64, _, _ string, target string) (int64, error) {
	w.owner.downloads.Add(1)
	if w.hold != nil {
		select {
		case <-w.hold:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return 0, err
	}
	data := []byte("fictional media for " + directPath)
	if err := fsutil.WritePrivateFile(target, data); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

func newFakeJobApp(t *testing.T, storeDir string) *fakeJobApp {
	t.Helper()
	a, err := app.New(app.Options{StoreDir: storeDir})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(a.Close)
	f := &fakeJobApp{App: a}
	f.wa = &fakeJobWA{owner: f}
	return f
}

func (f *fakeJobApp) WA() app.WAClient { return f.wa }

// configure sets the fake's hooks under its lock: the sync process reads them
// on its own goroutines.
func (f *fakeJobApp) configure(set func(*fakeJobApp)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	set(f)
}

func (f *fakeJobApp) lastRetry(t *testing.T) app.RetryMediaOptions {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.retries) == 0 {
		t.Fatal("no media retry ran")
	}
	return f.retries[len(f.retries)-1]
}

func (f *fakeJobApp) lastBackfill(t *testing.T) app.BackfillMediaOptions {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.backfills) == 0 {
		t.Fatal("no media backfill ran")
	}
	return f.backfills[len(f.backfills)-1]
}

func (f *fakeJobApp) lastHistory(t *testing.T) app.BackfillOptions {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.histories) == 0 {
		t.Fatal("no history backfill ran")
	}
	return f.histories[len(f.histories)-1]
}

func (f *fakeJobApp) RetryMedia(ctx context.Context, opts app.RetryMediaOptions) (app.MediaRetryResult, error) {
	f.mu.Lock()
	f.retries = append(f.retries, opts)
	run := f.retry
	f.mu.Unlock()
	if run == nil {
		return app.MediaRetryResult{}, nil
	}
	return run(ctx, opts)
}

func (f *fakeJobApp) BackfillMedia(ctx context.Context, opts app.BackfillMediaOptions) (app.BackfillMediaResult, error) {
	f.mu.Lock()
	f.backfills = append(f.backfills, opts)
	run := f.backfill
	f.mu.Unlock()
	if run == nil {
		return app.BackfillMediaResult{}, nil
	}
	return run(ctx, opts)
}

func (f *fakeJobApp) BackfillHistoryConnected(ctx context.Context, opts app.BackfillOptions) (app.BackfillResult, error) {
	f.mu.Lock()
	f.histories = append(f.histories, opts)
	run := f.history
	f.mu.Unlock()
	if run == nil {
		return app.BackfillResult{ChatJID: opts.ChatJID}, nil
	}
	return run(ctx, opts)
}

// jobDaemon plays `sync --follow` for one store: it holds the lock and serves
// the production delegate socket, running job kinds against a fakeJobApp and
// answering any other kind as an instant send.
type jobDaemon struct {
	storeDir string
	fake     *fakeJobApp
	mu       sync.Mutex
	requests []sendDelegateRequest
}

func startJobDaemon(t *testing.T, storeDir string) *jobDaemon {
	t.Helper()
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	if storeDir == "" {
		storeDir = shortPresenceDelegateStoreDir(t)
	}
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatalf("lock store: %v", err)
	}
	t.Cleanup(func() { _ = lk.Release() })
	d := &jobDaemon{storeDir: storeDir, fake: newFakeJobApp(t, storeDir)}
	stop, err := startSendDelegateServerForStore(context.Background(), storeDir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		d.mu.Lock()
		d.requests = append(d.requests, req)
		d.mu.Unlock()
		if req.Version != sendDelegateVersion {
			return sendDelegateResponse{}, fmt.Errorf("unsupported send delegate version %d", req.Version)
		}
		if isDelegateJobKind(req.Kind) {
			return executeDelegatedJob(ctx, d.fake, req)
		}
		return sendDelegateResponse{OK: true, Sent: true, ID: req.Message}, nil
	})
	if err != nil {
		t.Fatalf("start delegate server: %v", err)
	}
	t.Cleanup(stop)
	return d
}

func (d *jobDaemon) run(t *testing.T, global []string, args ...string) (string, string, error) {
	t.Helper()
	all := append([]string{"--store", d.storeDir}, global...)
	stdout, stderr, err := runPresenceDelegateHelper(t, append(all, args...))
	return strings.TrimSuffix(stdout, "PASS\n"), stderr, err
}

func (d *jobDaemon) lastRequest(t *testing.T) sendDelegateRequest {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.requests) == 0 {
		t.Fatal("the sync process received no request")
	}
	return d.requests[len(d.requests)-1]
}

func seedJobMedia(t *testing.T, db *store.DB, id, mediaType string) {
	t.Helper()
	if err := db.UpsertChat(jobChat, "dm", "Test contact", time.Now()); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	if err := db.UpsertMessage(store.UpsertMessageParams{
		ChatJID: jobChat, MsgID: id, SenderJID: jobChat, Timestamp: time.Now().Add(-time.Hour),
		MediaType: mediaType, Filename: id + ".jpg", MimeType: "image/jpeg",
		DirectPath: "/direct/" + id, MediaKey: []byte{1, 2, 3}, FileLength: 10,
	}); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
}

func TestMediaDownloadDelegatesToSyncWhenStoreLocked(t *testing.T) {
	d := startJobDaemon(t, "")
	seedJobMedia(t, d.fake.DB(), "IMG01", "image")

	stdout, stderr, err := d.run(t, []string{"--json"}, "media", "download", "--chat", jobChat, "--id", "IMG01")
	if err != nil {
		t.Fatalf("media download: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
	req := d.lastRequest(t)
	if req.Kind != mediaDownloadKind || req.Chat != jobChat || req.ID != "IMG01" {
		t.Fatalf("request = %+v", req)
	}
	info, err := d.fake.DB().GetMediaDownloadInfo(jobChat, "IMG01")
	if err != nil {
		t.Fatalf("GetMediaDownloadInfo: %v", err)
	}
	// The store media path the direct command uses.
	wantPath, err := d.fake.ResolveMediaOutputPath(info, "")
	if err != nil || !strings.HasPrefix(wantPath, d.storeDir) {
		t.Fatalf("ResolveMediaOutputPath = %q, %v", wantPath, err)
	}
	if info.LocalPath != wantPath || info.DownloadedAt.IsZero() {
		t.Fatalf("recorded local_path = %q at %v, want %q", info.LocalPath, info.DownloadedAt, wantPath)
	}
	// The direct command's JSON keys and values.
	var env struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil || !env.Success {
		t.Fatalf("output %q: %v", stdout, err)
	}
	keys := make([]string, 0, len(env.Data))
	for k := range env.Data {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"bytes", "chat", "downloaded", "downloaded_at", "id", "media_type", "mime_type", "path"}) {
		t.Fatalf("output keys = %v", keys)
	}
	if env.Data["path"] != wantPath || env.Data["chat"] != jobChat || env.Data["downloaded"] != true || env.Data["media_type"] != "image" {
		t.Fatalf("output = %v", env.Data)
	}
	if at, err := time.Parse(time.RFC3339Nano, env.Data["downloaded_at"].(string)); err != nil || at.Unix() != info.DownloadedAt.Unix() {
		t.Fatalf("downloaded_at = %v (%v), recorded %v", env.Data["downloaded_at"], err, info.DownloadedAt)
	}

	// Human output, and an explicit --output.
	out := filepath.Join(t.TempDir(), "photo.jpg")
	stdout, stderr, err = d.run(t, nil, "media", "download", "--chat", jobChat, "--id", "IMG01", "--output", out)
	if err != nil {
		t.Fatalf("media download --output: %v stderr=%q", err, stderr)
	}
	data, readErr := os.ReadFile(out)
	if readErr != nil || stdout != fmt.Sprintf("%s (%d bytes)\n", out, len(data)) {
		t.Fatalf("stdout = %q, file %d bytes (%v)", stdout, len(data), readErr)
	}
}

func TestMediaDownloadDelegatedErrorsComeFromTheSyncProcess(t *testing.T) {
	d := startJobDaemon(t, "")
	_, _, err := d.run(t, nil, "media", "download", "--chat", jobChat, "--id", "MISSING")
	if err == nil {
		t.Fatal("download of an unknown message succeeded")
	}
	if n := d.fake.downloads.Load(); n != 0 {
		t.Fatalf("downloads = %d, want none", n)
	}
}

func TestAbsoluteOutputPathKeepsDirectoryMarker(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	sep := string(os.PathSeparator)
	for in, want := range map[string]string{
		"":                     "",
		"/abs/file.jpg":        "/abs/file.jpg",
		"rel/file.jpg":         filepath.Join(wd, "rel", "file.jpg"),
		"rel" + sep:            filepath.Join(wd, "rel") + sep,
		"." + sep + "x" + sep:  filepath.Join(wd, "x") + sep,
		"rel/../other.jpg":     filepath.Join(wd, "other.jpg"),
		"/abs/dir" + sep:       "/abs/dir" + sep,
		"relative-name.ogg":    filepath.Join(wd, "relative-name.ogg"),
		"nested/dir/name.webp": filepath.Join(wd, "nested", "dir", "name.webp"),
	} {
		got, err := absoluteOutputPath(in)
		if err != nil || got != want {
			t.Fatalf("absoluteOutputPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestMediaRetryDelegatesTargetingFlags(t *testing.T) {
	d := startJobDaemon(t, "")
	result := app.MediaRetryResult{Requested: 1, Recovered: 1, Outcomes: []app.MediaRetryOutcome{
		{ChatJID: jobChat, MsgID: "VOICE01", Status: "recovered", Path: "/fictional/voice.ogg", Bytes: 42},
	}}
	d.fake.configure(func(f *fakeJobApp) {
		f.retry = func(context.Context, app.RetryMediaOptions) (app.MediaRetryResult, error) { return result, nil }
	})

	stdout, stderr, err := d.run(t, []string{"--json"}, "media", "retry", "--chat", jobChat, "--id", "VOICE01", "--wait", "5s", "--batch", "8")
	if err != nil {
		t.Fatalf("media retry --id: %v stderr=%q", err, stderr)
	}
	if want := directJSON(t, result); stdout != want {
		t.Fatalf("stdout = %q, want the direct output %q", stdout, want)
	}
	got := d.fake.lastRetry(t)
	want := app.RetryMediaOptions{ChatJID: jobChat, MsgID: "VOICE01", BatchSize: 8, Wait: 5 * time.Second}
	if !retryOptionsEqual(got, want) {
		t.Fatalf("retry options = %+v, want %+v", got, want)
	}

	stdout, _, err = d.run(t, nil, "media", "retry", "--type", "video,audio", "--limit", "3", "--before", "2026-01-02")
	if err != nil {
		t.Fatalf("media retry --type: %v", err)
	}
	if !strings.HasPrefix(stdout, "Requested: 1  Recovered: 1  Not on phone: 0  No response: 0  Failed: 0\n") || !strings.Contains(stdout, "recovered     "+jobChat+"/VOICE01  (42 bytes) /fictional/voice.ogg") {
		t.Fatalf("human output = %q", stdout)
	}
	got = d.fake.lastRetry(t)
	want = app.RetryMediaOptions{MediaTypes: []string{"video", "gif", "audio"}, Limit: 3, BatchSize: 32, Wait: 30 * time.Second,
		BeforeUnix: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC).Unix(), BeforeSet: true}
	if !retryOptionsEqual(got, want) {
		t.Fatalf("retry options = %+v, want %+v", got, want)
	}
}

func retryOptionsEqual(a, b app.RetryMediaOptions) bool {
	return a.ChatJID == b.ChatJID && a.MsgID == b.MsgID && slices.Equal(a.MediaTypes, b.MediaTypes) &&
		a.BeforeUnix == b.BeforeUnix && a.BeforeSet == b.BeforeSet && a.Limit == b.Limit &&
		a.BatchSize == b.BatchSize && a.Wait == b.Wait
}

func TestMediaRetryTargetingFlagsAreValidatedBeforeStoreAccess(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--id", "VOICE01"}, "--id requires --chat"},
		{[]string{"--chat", jobChat, "--id", "VOICE01", "--limit", "2"}, "--limit cannot be combined with --id"},
		{[]string{"--chat", jobChat, "--id", "VOICE01", "--type", "audio"}, "--type cannot be combined with --id"},
		{[]string{"--chat", jobChat, "--id", "VOICE01", "--before", "2026-01-02"}, "--before cannot be combined with --id"},
		{[]string{"--type", "voice"}, `--type: unknown media type "voice"`},
		{[]string{"--type", ""}, "--type: no media type given"},
	} {
		err := execute(append([]string{"--account", "does-not-exist", "media", "retry"}, tc.args...))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("args %v: error = %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestMediaBackfillDelegatesToSync(t *testing.T) {
	d := startJobDaemon(t, "")
	d.fake.configure(func(f *fakeJobApp) {
		f.backfill = func(ctx context.Context, opts app.BackfillMediaOptions) (app.BackfillMediaResult, error) {
			delegateJobEventsFrom(ctx)(delegateJobEvent{Event: "media_backfill_start", Data: map[string]any{"pending": 3}})
			delegateJobEventsFrom(ctx)(delegateJobEvent{
				Event: "warning",
				Data:  map[string]any{"code": "media_download_failed", "message": "media download failed for " + jobChat + "/IMG02: fictional"},
				Human: "media download failed for " + jobChat + "/IMG02: fictional\n",
			})
			return app.BackfillMediaResult{Pending: 3, Attempted: 2, Downloaded: 1, Failed: 1}, nil
		}
	})
	stdout, stderr, err := d.run(t, nil, "media", "backfill", "--chat", jobChat, "--limit", "2", "--workers", "1")
	if err != nil {
		t.Fatalf("media backfill: %v stderr=%q", err, stderr)
	}
	if stdout != "Pending: 3  Attempted: 2  Downloaded: 1  Skipped: 0  Failed: 1\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	// Without --events only the warning line prints, as in a direct run.
	if stderr != "media download failed for "+jobChat+"/IMG02: fictional\n" {
		t.Fatalf("stderr = %q", stderr)
	}
	got := d.fake.lastBackfill(t)
	if got != (app.BackfillMediaOptions{ChatJID: jobChat, Limit: 2, Workers: 1}) {
		t.Fatalf("backfill options = %+v", got)
	}
	stdout, _, err = d.run(t, []string{"--json"}, "media", "backfill")
	if err != nil {
		t.Fatalf("media backfill --json: %v", err)
	}
	if want := directJSON(t, map[string]any{"pending": 3, "attempted": 2, "downloaded": 1, "skipped": 0, "failed": 1}); stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func historyEventsForTest(ctx context.Context, opts app.BackfillOptions) (app.BackfillResult, error) {
	emit := delegateJobEventsFrom(ctx)
	emit(delegateJobEvent{
		Event: "backfill_requesting",
		Data:  map[string]any{"chat_jid": opts.ChatJID, "count": opts.Count, "request": 1, "anchor_msg_id": "ANCHOR01"},
		Human: fmt.Sprintf("Requesting %d older messages for %s...\n", opts.Count, opts.ChatJID),
	})
	emit(delegateJobEvent{
		Event: "warning",
		Data:  map[string]any{"code": "backfill_anchor_retry", "message": "warning: no history response for anchor ANCHOR01; retrying once with next local anchor NEXT01", "anchor_msg_id": "ANCHOR01", "retry_anchor_msg_id": "NEXT01"},
		Human: "warning: no history response for anchor ANCHOR01; retrying once with next local anchor NEXT01\n",
	})
	return app.BackfillResult{ChatJID: opts.ChatJID, RequestsSent: 2, ResponsesSeen: 1, MessagesAdded: 7, MessagesSynced: 7}, nil
}

func TestHistoryBackfillDelegatesToSyncAndStreamsEvents(t *testing.T) {
	d := startJobDaemon(t, "")
	d.fake.configure(func(f *fakeJobApp) { f.history = historyEventsForTest })

	stdout, stderr, err := d.run(t, []string{"--json", "--events"}, "history", "backfill", "--chat", jobGroup, "--count", "40", "--requests", "3", "--wait", "20s", "--idle-exit", "2s")
	if err != nil {
		t.Fatalf("history backfill: %v stderr=%q", err, stderr)
	}
	want := directJSON(t, map[string]any{"chat": jobGroup, "requests_sent": 2, "responses_seen": 1, "messages_added": int64(7), "messages_synced": int64(7)})
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	got := d.fake.lastHistory(t)
	if got != (app.BackfillOptions{ChatJID: jobGroup, Count: 40, Requests: 3, WaitPerRequest: 20 * time.Second, IdleExit: 2 * time.Second}) {
		t.Fatalf("backfill options = %+v", got)
	}
	var events []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(stderr), "\n") {
		var ev struct {
			Event string         `json:"event"`
			Data  map[string]any `json:"data"`
			TS    int64          `json:"ts"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("stderr line %q is not an NDJSON event: %v", line, err)
		}
		if ev.TS == 0 {
			t.Fatalf("event without timestamp: %q", line)
		}
		ev.Data["event"] = ev.Event
		events = append(events, ev.Data)
	}
	if len(events) != 2 || events[0]["event"] != "backfill_requesting" || events[0]["count"] != float64(40) || events[0]["anchor_msg_id"] != "ANCHOR01" ||
		events[1]["event"] != "warning" || events[1]["code"] != "backfill_anchor_retry" || events[1]["retry_anchor_msg_id"] != "NEXT01" {
		t.Fatalf("events = %v", events)
	}

	stdout, stderr, err = d.run(t, nil, "history", "backfill", "--chat", jobGroup)
	if err != nil {
		t.Fatalf("history backfill (human): %v", err)
	}
	if stdout != "Backfill complete for "+jobGroup+". Added 7 messages (2 requests).\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	wantErr := "Requesting 50 older messages for " + jobGroup + "...\nwarning: no history response for anchor ANCHOR01; retrying once with next local anchor NEXT01\n"
	if stderr != wantErr {
		t.Fatalf("stderr = %q, want %q", stderr, wantErr)
	}
}

func TestDelegatedJobRejectedByOlderSyncProcess(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	storeDir := shortPresenceDelegateStoreDir(t)
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatalf("lock store: %v", err)
	}
	defer lk.Release()
	// A sync process that predates the job kinds routes them to management,
	// which rejects the kind before doing anything.
	stop, err := startSendDelegateServerForStore(context.Background(), storeDir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		return executeDelegatedManagement(ctx, nil, req)
	})
	if err != nil {
		t.Fatalf("start delegate server: %v", err)
	}
	defer stop()
	for _, args := range [][]string{
		{"history", "backfill", "--chat", jobGroup},
		{"media", "backfill"},
		{"media", "retry"},
		{"media", "download", "--chat", jobChat, "--id", "IMG01"},
	} {
		_, stderr, err := runPresenceDelegateHelper(t, append([]string{"--store", storeDir}, args...))
		if err == nil || !strings.Contains(stderr, "does not support this command and did not run it") {
			t.Fatalf("%v: err = %v stderr = %q, want the restart hint", args, err, stderr)
		}
	}
}

// blockingJobServer serves the production delegate socket with an executor
// whose job kinds block until released or cancelled.
type blockingJobServer struct {
	storeDir  string
	started   chan string
	release   chan struct{}
	cancelled chan string
	executed  atomic.Int32
	stop      func()
}

func startBlockingJobServer(t *testing.T, ctx context.Context) *blockingJobServer {
	t.Helper()
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	s := &blockingJobServer{
		storeDir:  shortPresenceDelegateStoreDir(t),
		started:   make(chan string, 16),
		release:   make(chan struct{}),
		cancelled: make(chan string, 16),
	}
	stop, err := startSendDelegateServerForStore(ctx, s.storeDir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		if !isDelegateJobKind(req.Kind) {
			return sendDelegateResponse{OK: true, Sent: true, ID: req.Message}, nil
		}
		s.executed.Add(1)
		s.started <- req.Kind + ":" + req.ID
		if _, ok := ctx.Deadline(); !ok && req.TimeoutMS > 0 {
			return sendDelegateResponse{}, errors.New("job has no deadline despite a budget")
		}
		select {
		case <-s.release:
			return delegateJobResult(map[string]string{"id": req.ID})
		case <-ctx.Done():
			s.cancelled <- req.ID
			return sendDelegateResponse{}, ctx.Err()
		}
	})
	if err != nil {
		t.Fatalf("start delegate server: %v", err)
	}
	s.stop = stop
	t.Cleanup(func() {
		select {
		case <-s.release:
		default:
			close(s.release)
		}
		stop()
	})
	return s
}

func (s *blockingJobServer) flags(timeout time.Duration) *rootFlags {
	return &rootFlags{storeDir: s.storeDir, timeout: timeout}
}

func (s *blockingJobServer) waitStarted(t *testing.T) string {
	t.Helper()
	select {
	case got := <-s.started:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("job did not start")
		return ""
	}
}

func (s *blockingJobServer) startJob(t *testing.T, ctx context.Context, req sendDelegateRequest, budget time.Duration) chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := delegateJob(ctx, s.flags(0), req, budget, nil)
		done <- err
	}()
	return done
}

// Sends keep flowing while a long job runs: jobs do not take the send slot.
func TestLongJobDoesNotBlockSends(t *testing.T) {
	s := startBlockingJobServer(t, context.Background())
	job := s.startJob(t, context.Background(), sendDelegateRequest{Kind: historyBackfillKind, Chat: jobChat}, 0)
	s.waitStarted(t)

	for i := range 3 {
		start := time.Now()
		resp, err := delegateSend(context.Background(), s.flags(2*time.Second), sendDelegateRequest{Kind: "text", Message: fmt.Sprintf("send-%d", i)})
		if err != nil || !resp.Sent {
			t.Fatalf("send %d while a job runs: %+v, %v", i, resp, err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("send %d took %s behind the job", i, elapsed)
		}
	}
	close(s.release)
	if err := <-job; err != nil {
		t.Fatalf("job: %v", err)
	}
}

// Two bulk jobs of one kind never overlap. One still waiting when its budget
// runs out is refused and never runs; other kinds are not held up.
func TestBulkJobsOfOneKindRunOneAtATime(t *testing.T) {
	s := startBlockingJobServer(t, context.Background())
	first := s.startJob(t, context.Background(), sendDelegateRequest{Kind: historyBackfillKind, Chat: jobChat, ID: "first"}, 0)
	s.waitStarted(t)

	start := time.Now()
	_, err := delegateJob(context.Background(), s.flags(0), sendDelegateRequest{Kind: historyBackfillKind, Chat: jobGroup, ID: "second"}, sendDelegateReplyMargin+300*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "busy with other jobs") || !strings.Contains(err.Error(), "it did not run") {
		t.Fatalf("second backfill error = %v, want a did-not-run refusal", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("refusal took %s", elapsed)
	}

	// A download is not a bulk job and runs beside it.
	download := s.startJob(t, context.Background(), sendDelegateRequest{Kind: mediaDownloadKind, Chat: jobChat, ID: "dl"}, 0)
	if got := s.waitStarted(t); got != mediaDownloadKind+":dl" {
		t.Fatalf("started %q, want the download", got)
	}
	close(s.release)
	for _, done := range []chan error{first, download} {
		if err := <-done; err != nil {
			t.Fatalf("job: %v", err)
		}
	}
	time.Sleep(100 * time.Millisecond) // a late start of the refused job would show here
	if n := s.executed.Load(); n != 2 {
		t.Fatalf("executed %d jobs, want 2 (the refused one never runs)", n)
	}
}

func TestJobConcurrencyIsBounded(t *testing.T) {
	s := startBlockingJobServer(t, context.Background())
	var running []chan error
	for i := range delegateJobConcurrency {
		running = append(running, s.startJob(t, context.Background(), sendDelegateRequest{Kind: mediaDownloadKind, Chat: jobChat, ID: fmt.Sprintf("dl-%d", i)}, 0))
		s.waitStarted(t)
	}
	_, err := delegateJob(context.Background(), s.flags(0), sendDelegateRequest{Kind: mediaDownloadKind, Chat: jobChat, ID: "one-too-many"}, sendDelegateReplyMargin+300*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "it did not run") {
		t.Fatalf("job over the limit: %v, want a did-not-run refusal", err)
	}
	close(s.release)
	for _, done := range running {
		if err := <-done; err != nil {
			t.Fatalf("job: %v", err)
		}
	}
}

// Ctrl-C in the caller closes its connection, and the sync process stops the
// job instead of running it to completion for nobody.
func TestJobStopsWhenCallerDisconnects(t *testing.T) {
	s := startBlockingJobServer(t, context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	job := s.startJob(t, ctx, sendDelegateRequest{Kind: mediaBackfillKind}, 0)
	s.waitStarted(t)
	cancel()
	select {
	case err := <-job:
		if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "interrupted") {
			t.Fatalf("caller error = %v, want interrupted", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("caller did not return after cancel")
	}
	select {
	case <-s.cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("the sync process kept running the job after its caller left")
	}
}

func TestJobStopsAtItsBudget(t *testing.T) {
	s := startBlockingJobServer(t, context.Background())
	start := time.Now()
	budget := sendDelegateReplyMargin + 300*time.Millisecond
	_, err := delegateJob(context.Background(), s.flags(0), sendDelegateRequest{Kind: mediaRetryKind}, budget, nil)
	if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("error = %v, want the job's deadline", err)
	}
	if elapsed := time.Since(start); elapsed > budget+time.Second {
		t.Fatalf("job ran %s past a %s budget", elapsed, budget)
	}
	select {
	case <-s.cancelled:
	default:
		t.Fatal("job was not cancelled at its deadline")
	}
}

func TestDelegateJobBudgetFollowsCallerDeadline(t *testing.T) {
	s := startBlockingJobServer(t, context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), sendDelegateReplyMargin+300*time.Millisecond)
	defer cancel()
	_, err := delegateJob(ctx, s.flags(0), sendDelegateRequest{Kind: mediaRetryKind}, 0, nil)
	if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("error = %v, want the job stopped at the caller's deadline", err)
	}
}

// Stopping the delegate server waits for running jobs, whose contexts end
// with the sync run, so the store is not closed under them.
func TestStopWaitsForRunningJobs(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	storeDir := shortPresenceDelegateStoreDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var finished atomic.Bool
	stop, err := startSendDelegateServerForStore(ctx, storeDir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		close(started)
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond) // winding down
		finished.Store(true)
		return sendDelegateResponse{}, ctx.Err()
	})
	if err != nil {
		t.Fatalf("start delegate server: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := delegateJob(context.Background(), &rootFlags{storeDir: storeDir}, sendDelegateRequest{Kind: historyBackfillKind}, 0, nil)
		done <- err
	}()
	<-started
	cancel() // the sync run ends
	stop()
	if !finished.Load() {
		t.Fatal("stop returned while a job was still running")
	}
	<-done
}

// Progress frames stay out of old clients' way: they are only written for job
// kinds, and a job's frames all precede its single final response.
func TestJobFramesPrecedeTheFinalResponse(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleSendDelegateConn(context.Background(), serverConn, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
			for i := range 3 {
				delegateJobEventsFrom(ctx)(delegateJobEvent{Event: "progress", Data: map[string]any{"n": i}})
			}
			return delegateJobResult(map[string]int{"done": 3})
		}, make(chan struct{}), newSendPacer(sendSpacing{})) // the send slot is never free
	}()
	_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(clientConn).Encode(sendDelegateRequest{Version: sendDelegateVersion, Kind: mediaBackfillKind}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	dec := json.NewDecoder(clientConn)
	var frames []sendDelegateResponse
	for {
		var resp sendDelegateResponse
		if err := dec.Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		frames = append(frames, resp)
		if resp.Event == nil {
			break
		}
	}
	<-done
	if len(frames) != 4 || !frames[3].OK || !bytes.Equal(frames[3].Result, []byte(`{"done":3}`)) {
		t.Fatalf("frames = %+v", frames)
	}
}

func TestDelegateJobArgsRoundTrip(t *testing.T) {
	retry := app.RetryMediaOptions{ChatJID: jobChat, MsgID: "VOICE01", MediaTypes: []string{"audio"}, BeforeUnix: 1767312000, BeforeSet: true, Limit: 4, BatchSize: 2, Wait: 1500 * time.Millisecond}
	raw, err := json.Marshal(sendDelegateRequest{Kind: mediaRetryKind, Chat: retry.ChatJID, ID: retry.MsgID, Job: retryJobArgs(retry)})
	if err != nil {
		t.Fatal(err)
	}
	var req sendDelegateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if got := req.Job.retryOptions(req.Chat, req.ID); !retryOptionsEqual(got, retry) {
		t.Fatalf("retry options = %+v, want %+v", got, retry)
	}
	history := app.BackfillOptions{ChatJID: jobGroup, Count: 25, Requests: 4, WaitPerRequest: 45 * time.Second, IdleExit: 3 * time.Second}
	if got := historyJobArgs(history).historyOptions(jobGroup); got != history {
		t.Fatalf("history options = %+v, want %+v", got, history)
	}
}
