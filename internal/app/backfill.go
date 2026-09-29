package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type BackfillOptions struct {
	ChatJID        string
	Count          int
	Requests       int
	WaitPerRequest time.Duration
	IdleExit       time.Duration
}

const (
	DefaultBackfillCount    = 50
	DefaultBackfillRequests = 1
	MaxBackfillCount        = 500
	MaxBackfillRequests     = 100
)

type BackfillResult struct {
	ChatJID        string
	RequestsSent   int
	ResponsesSeen  int
	MessagesAdded  int64
	MessagesSynced int64
}

type onDemandResponse struct {
	conversations int
	messages      int
	endType       waHistorySync.Conversation_EndOfHistoryTransferType
}

var errOnDemandResponseTimeout = errors.New("timed out waiting for on-demand history sync response")

// BackfillHistory requests older messages for one chat from the primary
// device over a connection of its own: it connects, runs a one-shot sync that
// stores the on-demand responses, and disconnects once idle. A running
// `sync --follow` uses BackfillHistoryConnected instead, over its connection.
func (a *App) BackfillHistory(ctx context.Context, opts BackfillOptions) (BackfillResult, error) {
	chat, opts, err := prepareBackfill(opts)
	if err != nil {
		return BackfillResult{}, err
	}
	if err := a.EnsureAuthed(ctx); err != nil {
		return BackfillResult{}, err
	}
	if err := a.OpenWA(); err != nil {
		return BackfillResult{}, err
	}
	a.wa.SetManualHistorySyncDownload(true)
	defer a.wa.SetManualHistorySyncDownload(false)

	beforeCount, _ := a.db.CountMessages()
	run := a.startHistoryBackfill(ctx, chat, opts, false)
	defer run.stop()

	syncRes, err := a.Sync(ctx, SyncOptions{
		Mode:             SyncModeOnce,
		AllowQR:          false,
		IdleExit:         opts.IdleExit,
		afterHistorySync: run.handleOnDemand,
		// Sync can learn mappings and migrate old LID rows while connecting,
		// so the requests start only after that has completed.
		AfterConnect: run.requestBatches,
	})
	if err != nil {
		return BackfillResult{}, err
	}

	afterCount, _ := a.db.CountMessages()
	return run.result(afterCount-beforeCount, syncRes.MessagesStored), nil
}

// BackfillHistoryConnected runs the same backfill as BackfillHistory over the
// connection of a `sync --follow` already running in this process. It sends
// the requests with that connection and processes the ON_DEMAND responses the
// sync run receives, without connecting, starting a second sync loop, or
// changing the client's history-sync settings. Requests, retries, events and
// warnings are the same as the direct path's. Afterwards it keeps listening
// until no response has arrived for IdleExit, as the direct path's one-shot
// sync does before disconnecting, so a response delivered in parts is stored
// in full.
func (a *App) BackfillHistoryConnected(ctx context.Context, opts BackfillOptions) (BackfillResult, error) {
	chat, opts, err := prepareBackfill(opts)
	if err != nil {
		return BackfillResult{}, err
	}
	if client := a.WA(); client == nil || !client.IsConnected() {
		return BackfillResult{}, errors.New("not connected to WhatsApp (the sync process may be reconnecting); try again shortly")
	}

	beforeCount, _ := a.db.CountMessages()
	run := a.startHistoryBackfill(ctx, chat, opts, true)
	defer run.stop()

	if err := run.requestBatches(ctx); err != nil {
		return BackfillResult{}, err
	}
	run.waitIdle(ctx)

	afterCount, _ := a.db.CountMessages()
	return run.result(afterCount-beforeCount, run.stored.Load()), nil
}

func prepareBackfill(opts BackfillOptions) (types.JID, BackfillOptions, error) {
	chatStr := strings.TrimSpace(opts.ChatJID)
	if chatStr == "" {
		return types.JID{}, opts, fmt.Errorf("--chat is required")
	}
	chat, err := types.ParseJID(chatStr)
	if err != nil {
		return types.JID{}, opts, fmt.Errorf("parse chat JID: %w", err)
	}
	opts = normalizeBackfillOptions(opts)
	if err := validateBackfillOptions(opts); err != nil {
		return types.JID{}, opts, err
	}
	return chat, opts, nil
}

// historyBackfillRun is one backfill of one chat: the on-demand response
// handler it installs and the request loop that waits on it.
type historyBackfillRun struct {
	a    *App
	ctx  context.Context
	chat types.JID
	opts BackfillOptions
	// live is set when a running sync already stores every HistorySync event
	// before this run's handler sees it (BackfillHistoryConnected).
	live bool

	mu     sync.Mutex
	waitCh chan onDemandResponse

	// stored counts messages stored from on-demand notifications this run
	// downloaded; lastEvent is when an on-demand response last arrived.
	stored    atomic.Int64
	lastEvent atomic.Int64

	// Only the request loop touches these.
	requestsSent  int
	responsesSeen int
	preferPN      bool

	handlerID uint32
}

func (a *App) startHistoryBackfill(ctx context.Context, chat types.JID, opts BackfillOptions, live bool) *historyBackfillRun {
	r := &historyBackfillRun{a: a, ctx: ctx, chat: chat, opts: opts, live: live}
	r.lastEvent.Store(nowUTC().UnixNano())
	r.handlerID = a.wa.AddEventHandler(r.handleEvent)
	return r
}

func (r *historyBackfillRun) stop() {
	r.a.wa.RemoveEventHandler(r.handlerID)
}

func (r *historyBackfillRun) handleEvent(evt any) {
	a, ctx := r.a, r.ctx
	switch v := evt.(type) {
	case *events.Message:
		// Sync ignores ON_DEMAND notifications (it downloads the others), so
		// the backfill in progress downloads and stores its responses.
		notif := historySyncNotificationFromMessage(v)
		if notif == nil || notif.GetSyncType() != waE2E.HistorySyncType_ON_DEMAND {
			return
		}
		data, err := a.wa.DownloadHistorySync(ctx, notif)
		if err != nil {
			a.opEmitWarning(ctx,
				"on_demand_history_download_failed",
				fmt.Sprintf("warning: failed to download on-demand history sync: %v", err),
				map[string]any{"error": err.Error()},
			)
			return
		}
		if data.GetSyncType() != waHistorySync.HistorySync_ON_DEMAND {
			return
		}
		hs := &events.HistorySync{Data: data}
		a.handleHistorySync(ctx, SyncOptions{}, hs, &r.stored, &r.lastEvent, func(string, string) {})
		r.handleOnDemand(hs)
	case *events.HistorySync:
		// The running sync's handler was registered first and has already
		// stored this. The direct path is signalled by afterHistorySync, after
		// its own sync handler stores it.
		if r.live {
			r.handleOnDemand(v)
		}
	}
}

// handleOnDemand wakes the request waiting for this chat's response.
func (r *historyBackfillRun) handleOnDemand(hs *events.HistorySync) {
	if hs == nil || hs.Data == nil || hs.Data.GetSyncType() != waHistorySync.HistorySync_ON_DEMAND {
		return
	}
	r.lastEvent.Store(nowUTC().UnixNano())
	a, ctx := r.a, r.ctx
	want := a.canonicalStoreJID(ctx, r.chat).String()
	for _, conv := range hs.Data.GetConversations() {
		if a.canonicalStoreJIDString(ctx, strings.TrimSpace(conv.GetID())) != want {
			continue
		}
		r.mu.Lock()
		ch := r.waitCh
		r.mu.Unlock()
		if ch == nil {
			return
		}
		resp := onDemandResponse{
			conversations: len(hs.Data.GetConversations()),
			messages:      len(conv.GetMessages()),
			endType:       conv.GetEndOfHistoryTransferType(),
		}
		select {
		case ch <- resp:
		default:
		}
		return
	}
}

func (r *historyBackfillRun) request(ctx context.Context, anchor store.MessageInfo, requestChat types.JID) (onDemandResponse, error) {
	if err := ctx.Err(); err != nil {
		return onDemandResponse{}, err
	}
	a := r.a
	ch := make(chan onDemandResponse, 4)
	r.mu.Lock()
	r.waitCh = ch
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.waitCh = nil
		r.mu.Unlock()
	}()

	storeChat := a.canonicalStoreJID(ctx, r.chat).String()
	r.requestsSent++
	a.opEmitOrPrint(ctx, "backfill_requesting", map[string]any{
		"chat_jid":         storeChat,
		"request_chat_jid": requestChat.String(),
		"count":            r.opts.Count,
		"request":          r.requestsSent,
		"anchor_msg_id":    anchor.MsgID,
	}, "Requesting %d older messages for %s...\n", r.opts.Count, storeChat)
	reqInfo := types.MessageInfo{
		MessageSource: types.MessageSource{Chat: requestChat, IsFromMe: anchor.FromMe},
		ID:            types.MessageID(anchor.MsgID),
		Timestamp:     anchor.Timestamp,
	}
	if _, err := a.wa.RequestHistorySyncOnDemand(ctx, reqInfo, r.opts.Count); err != nil {
		return onDemandResponse{}, err
	}
	timer := time.NewTimer(r.opts.WaitPerRequest)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return onDemandResponse{}, ctx.Err()
	case resp := <-ch:
		r.responsesSeen++
		return resp, nil
	case <-timer.C:
		return onDemandResponse{}, fmt.Errorf("%w (anchor %s)", errOnDemandResponseTimeout, anchor.MsgID)
	}
}

// requestIdentities orders the identities to ask for a chat. A mapped 1:1
// chat has two. The primary device files some chats under the LID and others
// under the phone number, and answers only requests addressed to the one it
// uses (#444). Ask by LID first, retry the same anchor by phone number when
// that goes unanswered, and keep using whichever identity answered for the
// rest of this run. Resolve on every request: sync can learn a mapping while
// connecting.
func (r *historyBackfillRun) requestIdentities(ctx context.Context) []types.JID {
	lidChat := r.a.wa.ResolvePNToLID(ctx, r.chat)
	pnChat := r.a.wa.ResolveLIDToPN(ctx, lidChat)
	if pnChat == lidChat {
		return []types.JID{lidChat}
	}
	if r.preferPN {
		return []types.JID{pnChat, lidChat}
	}
	return []types.JID{lidChat, pnChat}
}

func (r *historyBackfillRun) requestAnchor(ctx context.Context, anchor store.MessageInfo) (onDemandResponse, error) {
	ids := r.requestIdentities(ctx)
	resp, err := r.request(ctx, anchor, ids[0])
	if len(ids) < 2 || !errors.Is(err, errOnDemandResponseTimeout) || ctx.Err() != nil {
		return resp, err
	}
	r.a.opEmitWarning(ctx, "backfill_identity_retry",
		fmt.Sprintf("warning: no history response for anchor %s from %s; retrying with %s", anchor.MsgID, ids[0], ids[1]),
		map[string]any{
			"chat_jid":               r.a.canonicalStoreJID(ctx, r.chat).String(),
			"anchor_msg_id":          anchor.MsgID,
			"request_chat_jid":       ids[0].String(),
			"retry_request_chat_jid": ids[1].String(),
		})
	resp, err = r.request(ctx, anchor, ids[1])
	if err == nil {
		r.preferPN = ids[1].Server == types.DefaultUserServer
	}
	return resp, err
}

// requestBatches asks for up to opts.Requests batches older than the oldest
// local message, retrying an unanswered anchor once with the next local
// message (#373), and stops when a batch adds nothing older.
func (r *historyBackfillRun) requestBatches(ctx context.Context) error {
	a := r.a
	// Resolve the local identity only now: sync can migrate old LID rows
	// while connecting.
	chatStr := a.canonicalStoreJID(ctx, r.chat).String()
	for i := 0; i < r.opts.Requests; i++ {
		oldest, err := a.db.GetOldestMessageInfo(chatStr)
		if err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("no messages for %s in local DB; run `wacli sync` first", chatStr)
			}
			return err
		}

		resp, err := r.requestAnchor(ctx, oldest)
		if errors.Is(err, errOnDemandResponseTimeout) && ctx.Err() == nil {
			next, nextErr := a.db.GetNextMessageInfo(chatStr, oldest.MsgID)
			if nextErr != nil && !errors.Is(nextErr, sql.ErrNoRows) {
				return nextErr
			}
			if nextErr == nil {
				a.opEmitWarning(ctx, "backfill_anchor_retry",
					fmt.Sprintf("warning: no history response for anchor %s; retrying once with next local anchor %s", oldest.MsgID, next.MsgID),
					map[string]any{"chat_jid": chatStr, "anchor_msg_id": oldest.MsgID, "retry_anchor_msg_id": next.MsgID})
				resp, err = r.requestAnchor(ctx, next)
			}
		}
		if err != nil {
			return err
		}

		a.opEmitOrPrint(ctx, "backfill_response", map[string]any{
			"chat_jid":       chatStr,
			"conversations":  resp.conversations,
			"messages":       resp.messages,
			"responses_seen": r.responsesSeen,
		}, "On-demand history sync: %d conversations, %d messages.\n", resp.conversations, resp.messages)

		newOldest, err := a.db.GetOldestMessageInfo(chatStr)
		// A retry's newer anchor is not progress past the original oldest row.
		if err == nil && newOldest.MsgID == oldest.MsgID {
			a.opEmitOrPrint(ctx, "backfill_stopped", map[string]any{
				"chat_jid": chatStr,
				"reason":   "no_older_messages_added",
			}, "No older messages were added (stopping).\n")
			return nil
		}
		if resp.messages <= 0 {
			a.opEmitOrPrint(ctx, "backfill_stopped", map[string]any{
				"chat_jid": chatStr,
				"reason":   "no_messages_returned",
			}, "No messages returned (stopping).\n")
			return nil
		}
		if resp.endType == waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY {
			a.opEmitOrPrint(ctx, "backfill_stopped", map[string]any{
				"chat_jid": chatStr,
				"reason":   "start_of_history_reached",
			}, "Reached start of chat history (stopping).\n")
			return nil
		}
	}
	return nil
}

// waitIdle returns once no on-demand response has arrived for IdleExit, or
// when ctx ends. Like the direct path's idle exit, running out of time here
// is not an error: the requests have completed.
func (r *historyBackfillRun) waitIdle(ctx context.Context) {
	for {
		idle := time.Since(time.Unix(0, r.lastEvent.Load()))
		if idle >= r.opts.IdleExit {
			return
		}
		timer := time.NewTimer(r.opts.IdleExit - idle)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (r *historyBackfillRun) result(added, synced int64) BackfillResult {
	return BackfillResult{
		ChatJID:        r.a.canonicalStoreJID(r.ctx, r.chat).String(),
		RequestsSent:   r.requestsSent,
		ResponsesSeen:  r.responsesSeen,
		MessagesAdded:  added,
		MessagesSynced: synced,
	}
}

func normalizeBackfillOptions(opts BackfillOptions) BackfillOptions {
	if opts.Count <= 0 {
		opts.Count = DefaultBackfillCount
	}
	if opts.Requests <= 0 {
		opts.Requests = DefaultBackfillRequests
	}
	if opts.WaitPerRequest <= 0 {
		opts.WaitPerRequest = 60 * time.Second
	}
	if opts.IdleExit <= 0 {
		opts.IdleExit = 5 * time.Second
	}
	return opts
}

func validateBackfillOptions(opts BackfillOptions) error {
	if opts.Count > MaxBackfillCount {
		return fmt.Errorf("--count must be <= %d (got %d)", MaxBackfillCount, opts.Count)
	}
	if opts.Requests > MaxBackfillRequests {
		return fmt.Errorf("--requests must be <= %d (got %d)", MaxBackfillRequests, opts.Requests)
	}
	return nil
}
