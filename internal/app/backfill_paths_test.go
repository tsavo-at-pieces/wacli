package app

import (
	"context"
	"testing"
	"time"
)

// backfillFunc runs one history backfill for a test.
type backfillFunc func(t *testing.T, ctx context.Context, a *App, opts BackfillOptions) (BackfillResult, error)

// forEachBackfillPath runs a backfill test through both paths: the direct
// command's own connection (BackfillHistory), and the connection of a
// running `sync --follow` (BackfillHistoryConnected). Both must send the same
// requests, retries and warnings.
func forEachBackfillPath(t *testing.T, test func(t *testing.T, backfill backfillFunc)) {
	t.Helper()
	t.Run("direct", func(t *testing.T) {
		test(t, func(t *testing.T, ctx context.Context, a *App, opts BackfillOptions) (BackfillResult, error) {
			return a.BackfillHistory(ctx, opts)
		})
	})
	t.Run("sync-follow", func(t *testing.T) {
		test(t, backfillThroughFollow)
	})
}

// followRun is a `sync --follow` running against a fake client.
type followRun struct {
	cancel context.CancelFunc
	done   chan error
}

// startFollowSync starts a follow-mode sync and returns once it has
// connected and finished startup, as the sync command does before opening
// its delegate socket.
func startFollowSync(t *testing.T, a *App) *followRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	connected := make(chan struct{})
	run := &followRun{cancel: cancel, done: make(chan error, 1)}
	go func() {
		_, err := a.Sync(ctx, SyncOptions{Mode: SyncModeFollow, AfterConnect: func(context.Context) error {
			close(connected)
			return nil
		}})
		run.done <- err
	}()
	select {
	case <-connected:
	case err := <-run.done:
		cancel()
		t.Fatalf("sync --follow exited before connecting: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("sync --follow did not connect")
	}
	return run
}

// stop ends the follow run and waits for it to return.
func (r *followRun) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("sync --follow did not stop")
	}
}

// running fails the test when the follow run has already returned.
func (r *followRun) running(t *testing.T) {
	t.Helper()
	select {
	case err := <-r.done:
		t.Fatalf("sync --follow stopped during the backfill: %v", err)
	default:
	}
}

// backfillThroughFollow runs a backfill over a running follow sync's
// connection and checks that it left the sync run as it found it.
func backfillThroughFollow(t *testing.T, ctx context.Context, a *App, opts BackfillOptions) (BackfillResult, error) {
	t.Helper()
	f := fakeWAOf(t, a)
	run := startFollowSync(t, a)
	defer run.stop(t)

	f.mu.Lock()
	connects := f.connectCalls
	manual := len(f.manualHistorySyncCalls)
	handlers := len(f.handlers)
	f.mu.Unlock()

	res, err := a.BackfillHistoryConnected(ctx, opts)

	run.running(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.connectCalls != connects {
		t.Errorf("backfill connected again (%d connects, want %d)", f.connectCalls, connects)
	}
	if len(f.manualHistorySyncCalls) != manual {
		t.Errorf("backfill changed manual history download: calls %v", f.manualHistorySyncCalls)
	}
	if len(f.handlers) != handlers {
		t.Errorf("backfill left %d event handlers, want %d", len(f.handlers), handlers)
	}
	return res, err
}

// fakeWAOf returns the fake client of a test app.
func fakeWAOf(t *testing.T, a *App) *fakeWA {
	t.Helper()
	f, ok := a.wa.(*fakeWA)
	if !ok {
		t.Fatalf("unexpected client %T", a.wa)
	}
	return f
}
