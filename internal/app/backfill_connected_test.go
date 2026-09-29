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
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// A backfill run for another process streams its events to that process
// through the operation sink, and keeps them out of the sync run's own stream.
func TestBackfillHistoryConnectedStreamsEventsToOperationSink(t *testing.T) {
	a, f, chat, base := newBackfillRetryTest(t, "stalled", "fallback")
	var syncLog bytes.Buffer
	a.opts.Events = out.NewEventWriter(&syncLog, true)
	f.onDemandHistory = func(info types.MessageInfo, count int) *events.HistorySync {
		if info.ID == "stalled" {
			return nil
		}
		hs := backfillTestResponse(chat, "older", base.Add(-time.Second))
		hs.Data.Conversations[0].EndOfHistoryTransferType = waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY.Enum()
		return hs
	}
	var mu sync.Mutex
	var got []OperationEvent
	ctx := WithOperationEvents(context.Background(), func(ev OperationEvent) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev)
	})

	run := startFollowSync(t, a)
	res, err := a.BackfillHistoryConnected(ctx, backfillRetryOptions(chat))
	run.stop(t)
	if err != nil {
		t.Fatalf("BackfillHistoryConnected: %v", err)
	}
	if res.RequestsSent != 2 || res.ResponsesSeen != 1 || res.MessagesAdded != 1 {
		t.Fatalf("result = %+v", res)
	}

	mu.Lock()
	defer mu.Unlock()
	var names []string
	for _, ev := range got {
		name := ev.Event
		if code, _ := ev.Data["code"].(string); code != "" {
			name += ":" + code
		}
		names = append(names, name)
	}
	want := []string{"backfill_requesting", "warning:backfill_anchor_retry", "backfill_requesting", "backfill_response", "backfill_stopped"}
	if !slices.Equal(names, want) {
		t.Fatalf("streamed events = %v, want %v", names, want)
	}
	if !strings.HasPrefix(got[0].Human, "Requesting 50 older messages for "+chat) || !strings.HasSuffix(got[0].Human, "\n") {
		t.Fatalf("request line = %q", got[0].Human)
	}
	if !strings.HasPrefix(got[1].Human, "warning: no history response for anchor stalled") {
		t.Fatalf("warning line = %q", got[1].Human)
	}
	if got[4].Human != "Reached start of chat history (stopping).\n" {
		t.Fatalf("stop line = %q", got[4].Human)
	}
	if strings.Contains(syncLog.String(), "backfill_") {
		t.Fatalf("the sync run's own event stream got backfill events: %s", &syncLog)
	}
}

func TestBackfillHistoryConnectedRequiresConnection(t *testing.T) {
	a, f, chat, _ := newBackfillRetryTest(t, "anchor")
	_, err := a.BackfillHistoryConnected(context.Background(), backfillRetryOptions(chat))
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("error = %v, want not connected", err)
	}
	if f.connectCalls != 0 {
		t.Fatalf("backfill connected on its own (%d connects)", f.connectCalls)
	}
}

// The direct path's one-shot sync stays connected until idle, so a response
// that arrives in parts is stored in full. Through a running sync the backfill
// keeps its handler for the same idle period.
func TestBackfillHistoryConnectedStoresTrailingOnDemandParts(t *testing.T) {
	a, f, chat, base := newBackfillRetryTest(t, "anchor")
	notif := &waE2E.HistorySyncNotification{SyncType: waE2E.HistorySyncType_ON_DEMAND.Enum()}
	part := func(id string, ts time.Time) any {
		hs := backfillTestResponse(chat, id, ts)
		hs.Data.Conversations[0].EndOfHistoryTransferType = waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY.Enum()
		f.mu.Lock()
		f.downloadHistory = func(*waE2E.HistorySyncNotification) (*waHistorySync.HistorySync, error) { return hs.Data, nil }
		f.mu.Unlock()
		return &events.Message{Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{HistorySyncNotification: notif}}}
	}
	trailing := make(chan struct{})
	f.onDemandEvent = func(types.MessageInfo, int) any {
		first := part("older-1", base.Add(-time.Second))
		go func() {
			defer close(trailing)
			time.Sleep(50 * time.Millisecond)
			f.emit(part("older-2", base.Add(-2*time.Second)))
		}()
		return first
	}
	opts := backfillRetryOptions(chat)
	opts.IdleExit = 500 * time.Millisecond

	run := startFollowSync(t, a)
	res, err := a.BackfillHistoryConnected(context.Background(), opts)
	<-trailing
	run.stop(t)
	if err != nil {
		t.Fatalf("BackfillHistoryConnected: %v", err)
	}
	if res.MessagesAdded != 2 || res.MessagesSynced != 2 {
		t.Fatalf("result = %+v, want both parts stored", res)
	}
	oldest, err := a.db.GetOldestMessageInfo(chat)
	if err != nil || oldest.MsgID != "older-2" {
		t.Fatalf("oldest = %+v, err = %v", oldest, err)
	}
}

// Sync ignores ON_DEMAND notifications. Without a backfill in progress, a
// stray one is not downloaded, and a finished backfill leaves no handler that
// would download it.
func TestBackfillHistoryConnectedStopsHandlingAfterReturn(t *testing.T) {
	a, f, chat, base := newBackfillRetryTest(t, "anchor")
	f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
		hs := backfillTestResponse(chat, "older", base.Add(-time.Second))
		hs.Data.Conversations[0].EndOfHistoryTransferType = waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY.Enum()
		return hs
	}
	run := startFollowSync(t, a)
	defer run.stop(t)
	if _, err := a.BackfillHistoryConnected(context.Background(), backfillRetryOptions(chat)); err != nil {
		t.Fatalf("BackfillHistoryConnected: %v", err)
	}
	downloads := 0
	f.mu.Lock()
	f.downloadHistory = func(*waE2E.HistorySyncNotification) (*waHistorySync.HistorySync, error) {
		downloads++
		return nil, nil
	}
	f.mu.Unlock()
	notif := &waE2E.HistorySyncNotification{SyncType: waE2E.HistorySyncType_ON_DEMAND.Enum()}
	f.emit(&events.Message{Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{HistorySyncNotification: notif}}})
	if downloads != 0 {
		t.Fatalf("on-demand notification downloaded %d times after the backfill returned", downloads)
	}
	run.running(t)
}
