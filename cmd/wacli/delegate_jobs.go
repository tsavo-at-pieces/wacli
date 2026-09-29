package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
)

// Jobs are the delegated kinds that can run for minutes: media download,
// retry and backfill, and history backfill. A same-store `sync --follow` runs
// them with its connection so they work while it holds the store lock.
//
// They never take the send slot. That slot serializes sends and management
// operations, and anything queued behind a job would sit there until its
// caller's deadline and be refused (#446). Each job instead runs on its
// connection's goroutine, bounded by the server's delegateJobRunner: at most
// delegateJobConcurrency jobs at once, and at most one bulk job of each kind
// (see delegateJobClass). Waiting for a slot shares the job's budget.
//
// A job's budget is the caller's --timeout, capped by its deadline less the
// reply margin, as for sends. A caller without a timeout (bulk commands have
// none by default, as when they run directly) gets no deadline. Either way the
// job stops as soon as the caller goes away: the caller closing its end of the
// connection (Ctrl-C, exit, its own timeout) cancels the job.
//
// While it runs, a job streams its progress events and warnings as frames
// (sendDelegateResponse.Event) ahead of the final response. The caller prints
// them as the direct command would, as NDJSON with --events or as the same
// stderr lines without it.
const (
	mediaDownloadKind   = "media_download"
	mediaRetryKind      = "media_retry"
	mediaBackfillKind   = "media_backfill"
	historyBackfillKind = "history_backfill"

	// delegateJobConcurrency bounds the jobs one sync process runs at once.
	delegateJobConcurrency = 4
	// delegateJobEventWriteTimeout bounds one progress write. A caller that
	// stops reading for this long is treated as gone and its job stops.
	delegateJobEventWriteTimeout = 10 * time.Second
	// delegateJobShutdownWait bounds how long stopping the delegate server
	// waits for jobs to return once the sync run's context has ended.
	delegateJobShutdownWait = 10 * time.Second
)

func isDelegateJobKind(kind string) bool {
	switch kind {
	case mediaDownloadKind, mediaRetryKind, mediaBackfillKind, historyBackfillKind:
		return true
	}
	return false
}

// delegateJobLabel names a job kind in messages.
func delegateJobLabel(kind string) string {
	return strings.ReplaceAll(kind, "_", " ")
}

// delegateJobArgs carries the flags of the job kinds.
type delegateJobArgs struct {
	// Output is the absolute --output of media download.
	Output string `json:"output,omitempty"`
	// MediaTypes are the stored media types media retry --type selects.
	MediaTypes []string `json:"media_types,omitempty"`
	BeforeUnix int64    `json:"before_unix,omitempty"`
	BeforeSet  bool     `json:"before_set,omitempty"`
	Limit      int      `json:"limit,omitempty"`
	Batch      int      `json:"batch,omitempty"`
	Workers    int      `json:"workers,omitempty"`
	WaitMS     int64    `json:"wait_ms,omitempty"`
	Count      int      `json:"count,omitempty"`
	Requests   int      `json:"requests,omitempty"`
	IdleExitMS int64    `json:"idle_exit_ms,omitempty"`
}

// delegateJobEvent is one progress event or warning streamed by a job. Human
// is what the direct command prints on stderr without --events.
type delegateJobEvent struct {
	Event string         `json:"event"`
	Data  map[string]any `json:"data,omitempty"`
	Human string         `json:"human,omitempty"`
}

// delegateJobClass names the jobs that must not overlap: two history
// backfills would each download and store every on-demand response and count
// each other's messages, and two bulk media backfills or retries would work
// through the same pending rows. A download or a single-message retry has no
// class and only takes a general slot.
func delegateJobClass(req sendDelegateRequest) string {
	switch req.Kind {
	case historyBackfillKind, mediaBackfillKind:
		return req.Kind
	case mediaRetryKind:
		if strings.TrimSpace(req.ID) == "" {
			return req.Kind
		}
	}
	return ""
}

// delegateJobRunner bounds and tracks the jobs of one delegate server.
type delegateJobRunner struct {
	slots   chan struct{}
	mu      sync.Mutex
	classes map[string]chan struct{}
	running sync.WaitGroup
}

func newDelegateJobRunner(concurrency int) *delegateJobRunner {
	if concurrency <= 0 {
		concurrency = 1
	}
	return &delegateJobRunner{slots: make(chan struct{}, concurrency), classes: map[string]chan struct{}{}}
}

func (r *delegateJobRunner) classSlot(class string) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.classes[class]
	if !ok {
		ch = make(chan struct{}, 1)
		r.classes[class] = ch
	}
	return ch
}

// acquire waits, bounded by ctx, for the job's class and a general slot.
func (r *delegateJobRunner) acquire(ctx context.Context, class string) (func(), error) {
	var classSlot chan struct{}
	if class != "" {
		classSlot = r.classSlot(class)
		select {
		case classSlot <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	select {
	case r.slots <- struct{}{}:
	case <-ctx.Done():
		if classSlot != nil {
			<-classSlot
		}
		return nil, ctx.Err()
	}
	r.running.Add(1)
	return func() {
		<-r.slots
		if classSlot != nil {
			<-classSlot
		}
		r.running.Done()
	}, nil
}

// wait returns when no job is running, or after timeout.
func (r *delegateJobRunner) wait(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		r.running.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

type delegateJobEventsKey struct{}

// withDelegateJobEvents carries the stream of the job's caller.
func withDelegateJobEvents(ctx context.Context, emit func(delegateJobEvent)) context.Context {
	return context.WithValue(ctx, delegateJobEventsKey{}, emit)
}

// delegateJobEventsFrom returns the caller's stream (a no-op outside a job).
func delegateJobEventsFrom(ctx context.Context) func(delegateJobEvent) {
	if emit, ok := ctx.Value(delegateJobEventsKey{}).(func(delegateJobEvent)); ok && emit != nil {
		return emit
	}
	return func(delegateJobEvent) {}
}

// withAppOperationEvents routes the app's operation events under ctx to the
// job's caller.
func withAppOperationEvents(ctx context.Context) context.Context {
	emit := delegateJobEventsFrom(ctx)
	return app.WithOperationEvents(ctx, func(ev app.OperationEvent) {
		emit(delegateJobEvent{Event: ev.Event, Data: ev.Data, Human: ev.Human})
	})
}

type delegateJobRunnerKey struct{}

// fallbackDelegateJobRunner serves connections handled without a server that
// set its own runner (tests driving handleSendDelegateConn directly).
var fallbackDelegateJobRunner = newDelegateJobRunner(delegateJobConcurrency)

func withDelegateJobRunner(ctx context.Context, r *delegateJobRunner) context.Context {
	return context.WithValue(ctx, delegateJobRunnerKey{}, r)
}

func delegateJobRunnerFrom(ctx context.Context) *delegateJobRunner {
	if r, ok := ctx.Value(delegateJobRunnerKey{}).(*delegateJobRunner); ok && r != nil {
		return r
	}
	return fallbackDelegateJobRunner
}

// delegateJobDeadline is the job's budget as an absolute deadline, if any.
func delegateJobDeadline(req sendDelegateRequest, now time.Time) (time.Time, bool) {
	var deadline time.Time
	if req.TimeoutMS > 0 {
		deadline = now.Add(millisDuration(req.TimeoutMS, 0))
	}
	if req.DeadlineUnixMS > 0 {
		callerDeadline := time.UnixMilli(req.DeadlineUnixMS).Add(-sendDelegateReplyMargin)
		if deadline.IsZero() || callerDeadline.Before(deadline) {
			deadline = callerDeadline
		}
	}
	return deadline, !deadline.IsZero()
}

// serveDelegateJob runs one job request on its connection's goroutine.
func serveDelegateJob(ctx context.Context, conn net.Conn, req sendDelegateRequest, execute sendDelegateExecutor) {
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if deadline, ok := delegateJobDeadline(req, time.Now()); ok {
		var cancelDeadline context.CancelFunc
		jobCtx, cancelDeadline = context.WithDeadline(jobCtx, deadline)
		defer cancelDeadline()
	}
	// Reads only watch for the caller leaving; each write sets its own
	// deadline.
	_ = conn.SetDeadline(time.Time{})
	go func() {
		// The caller sends nothing after its request, so any read result
		// means it closed its end (or the job finished and closed ours).
		var buf [1]byte
		for {
			if _, err := conn.Read(buf[:]); err != nil {
				cancel()
				return
			}
		}
	}()
	w := &delegateJobWriter{conn: conn, enc: json.NewEncoder(conn), cancel: cancel}

	release, err := delegateJobRunnerFrom(ctx).acquire(jobCtx, delegateJobClass(req))
	if err != nil {
		w.final(sendDelegateResponse{OK: false, Error: delegateJobRefusal(req, jobCtx)})
		return
	}
	defer release()
	// Never start a job after its caller gave up.
	if jobCtx.Err() != nil {
		w.final(sendDelegateResponse{OK: false, Error: delegateJobRefusal(req, jobCtx)})
		return
	}

	resp, err := execute(withDelegateJobEvents(jobCtx, w.event), req)
	if err != nil {
		resp = sendDelegateResponse{OK: false, Error: err.Error()}
	}
	w.final(resp)
}

func delegateJobRefusal(req sendDelegateRequest, ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("the running sync process was busy with other jobs until this %s's timeout; it did not run", delegateJobLabel(req.Kind))
	}
	return fmt.Sprintf("the %s was cancelled before it started; it did not run", delegateJobLabel(req.Kind))
}

// delegateJobWriter serializes the frames written to one caller.
type delegateJobWriter struct {
	mu     sync.Mutex
	conn   net.Conn
	enc    *json.Encoder
	cancel context.CancelFunc
	broken bool
}

func (w *delegateJobWriter) event(ev delegateJobEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.broken {
		return
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(delegateJobEventWriteTimeout))
	if err := w.enc.Encode(sendDelegateResponse{OK: true, Event: &ev}); err != nil {
		// The caller is gone or stopped reading: nobody wants the rest.
		w.broken = true
		w.cancel()
	}
}

func (w *delegateJobWriter) final(resp sendDelegateResponse) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.broken {
		return
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(sendDelegateResponseGrace))
	_ = w.enc.Encode(resp)
}

// delegateJob runs req in the same-store sync process and returns its final
// response, passing each streamed progress event to onEvent. budget is the
// job's timeout (0 = none); ctx's deadline, if sooner, caps it. Cancelling ctx
// (Ctrl-C) closes the connection, which stops the job in the sync process.
func delegateJob(ctx context.Context, flags *rootFlags, req sendDelegateRequest, budget time.Duration, onEvent func(delegateJobEvent)) (sendDelegateResponse, error) {
	req.Version = sendDelegateVersion
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl); budget <= 0 || left < budget {
			budget = max(left, time.Millisecond)
		}
	}
	req.TimeoutMS = durationMillis(budget)
	storeDir, err := resolveStoreDir(flags)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", sendDelegateSocketPath(storeDir))
	if err != nil {
		return sendDelegateResponse{}, fmt.Errorf("%w: %v", errSendDelegateUnavailable, err)
	}
	defer conn.Close()
	if budget > 0 {
		deadline := time.Now().Add(budget)
		req.DeadlineUnixMS = deadline.UnixMilli()
		// The job stops at its deadline less the reply margin; allow it the
		// time to return and report what it did.
		_ = conn.SetDeadline(deadline.Add(sendDelegateResponseGrace))
	}
	// An interrupt closes the connection, and the sync process stops the job.
	// A deadline is left to the job, which reports how far it got.
	stopWatch := context.AfterFunc(ctx, func() {
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			_ = conn.Close()
		}
	})
	defer stopWatch()

	label := delegateJobLabel(req.Kind)
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return sendDelegateResponse{}, ctx.Err()
		}
		return sendDelegateResponse{}, err
	}
	dec := json.NewDecoder(conn)
	dec.UseNumber()
	for {
		var resp sendDelegateResponse
		if err := dec.Decode(&resp); err != nil {
			var netErr net.Error
			switch {
			case errors.Is(ctx.Err(), context.Canceled):
				return sendDelegateResponse{}, fmt.Errorf("interrupted; the running sync process stops the %s (work already done is kept): %w", label, ctx.Err())
			case errors.As(err, &netErr) && netErr.Timeout():
				return sendDelegateResponse{}, fmt.Errorf("no reply from the running sync process before the timeout; the %s may have partly run: %w", label, err)
			case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
				return sendDelegateResponse{}, fmt.Errorf("the running sync process closed the connection before the %s finished (did it exit?); it may have partly run", label)
			}
			return sendDelegateResponse{}, err
		}
		if resp.Event != nil {
			if onEvent != nil {
				onEvent(*resp.Event)
			}
			continue
		}
		if !resp.OK {
			return sendDelegateResponse{}, errors.New(resp.Error)
		}
		return resp, nil
	}
}

// delegateJobAfterOpenFailure hands req to a same-store `sync --follow` when
// openErr is that process's store lock, prints the job's progress as it
// streams in, then prints the result with write. Without a running sync
// process it returns openErr unchanged.
func delegateJobAfterOpenFailure(ctx context.Context, flags *rootFlags, openErr error, req sendDelegateRequest, budget time.Duration, write func(sendDelegateResponse) error) error {
	resp, delegated, err := tryDelegateJob(ctx, flags, openErr, req, budget)
	if !delegated {
		return err
	}
	if err != nil {
		return err
	}
	return write(resp)
}

// tryDelegateJob is tryDelegateSend for a job: delegated is false, with
// openErr, when openErr is not the store lock or no sync process listens.
func tryDelegateJob(ctx context.Context, flags *rootFlags, openErr error, req sendDelegateRequest, budget time.Duration) (sendDelegateResponse, bool, error) {
	if !lock.IsLocked(openErr) {
		return sendDelegateResponse{}, false, openErr
	}
	resp, err := delegateJob(ctx, flags, req, budget, printDelegatedJobEvent(flags))
	if errors.Is(err, errSendDelegateUnavailable) {
		return sendDelegateResponse{}, false, openErr
	}
	if err != nil {
		return sendDelegateResponse{}, true, explainUnsupportedDelegateKind(err, req.Kind)
	}
	return resp, true, nil
}

// printDelegatedJobEvent prints a streamed event the way the direct command
// prints it: NDJSON with --events, else its human line.
func printDelegatedJobEvent(flags *rootFlags) func(delegateJobEvent) {
	events := out.NewEventWriter(os.Stderr, flags.events)
	return func(ev delegateJobEvent) {
		if events.Enabled() {
			_ = events.Emit(ev.Event, ev.Data)
			return
		}
		if ev.Human != "" {
			fmt.Fprint(os.Stderr, ev.Human)
		}
	}
}

// delegateJobResult wraps a job's result, encoded as the command's own type.
func delegateJobResult(v any) (sendDelegateResponse, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return sendDelegateResponse{}, fmt.Errorf("encode result: %w", err)
	}
	return sendDelegateResponse{OK: true, Result: raw}, nil
}

func decodeDelegateJobResult(resp sendDelegateResponse, v any) error {
	if len(resp.Result) == 0 {
		return errors.New("the running sync process returned no result")
	}
	if err := json.Unmarshal(resp.Result, v); err != nil {
		return fmt.Errorf("decode result from the running sync process: %w", err)
	}
	return nil
}

// bulkJobBudget is a bulk command's delegated budget: its --timeout when set
// explicitly, else none, matching mediaBulkContext for the direct run.
func bulkJobBudget(enabled bool, flags *rootFlags) time.Duration {
	if !enabled || flags == nil || flags.timeout <= 0 {
		return 0
	}
	return flags.timeout
}
