package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Fictional identities only.
var (
	statusMutePN  = types.NewJID("15550000001", types.DefaultUserServer)
	statusMuteLID = types.NewJID("100000000001", types.HiddenUserServer)
)

func statusMuteEvent(jid types.JID, muted bool, ts time.Time) *events.UserStatusMute {
	return &events.UserStatusMute{
		JID:       jid,
		Timestamp: ts,
		Action:    &waSyncAction.UserStatusMuteAction{Muted: proto.Bool(muted)},
	}
}

func mustStatusMute(t *testing.T, db *store.DB, jid string) store.StatusMute {
	t.Helper()
	m, ok, err := db.FindStatusMute(jid)
	if err != nil || !ok {
		t.Fatalf("FindStatusMute(%s) = %+v, %t, %v", jid, m, ok, err)
	}
	return m
}

func TestMuteStatusSyncsRegularHighAndRecordsMute(t *testing.T) {
	for _, mute := range []bool{true, false} {
		t.Run(fmt.Sprintf("mute=%t", mute), func(t *testing.T) {
			a := newTestApp(t)
			f := newFakeWA()
			f.lids[statusMuteLID] = statusMutePN
			a.wa = f

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := a.MuteStatus(ctx, types.JID{User: statusMuteLID.User, Device: 7, Server: types.HiddenUserServer}, mute); err != nil {
				t.Fatalf("MuteStatus: %v", err)
			}

			f.mu.Lock()
			fetches := append([]fakeAppStateFetch(nil), f.appStateFetches...)
			f.mu.Unlock()
			if len(fetches) != 1 || fetches[0].name != string(appstate.WAPatchRegularHigh) || fetches[0].fullSync {
				t.Fatalf("app state fetches = %+v, want one regular_high delta before the write", fetches)
			}
			calls := f.statusMuteCalls()
			if !slices.Equal(calls, []fakeStatusMuteCall{{target: statusMuteLID, mute: mute}}) {
				t.Fatalf("status mute calls = %+v", calls)
			}
			// Stored by phone JID, with the LID WhatsApp's index uses.
			got := mustStatusMute(t, a.db, statusMutePN.String())
			if got.JID != statusMutePN.String() || got.IndexJID != statusMuteLID.String() || got.Muted != mute {
				t.Fatalf("stored status mute = %+v", got)
			}
		})
	}
}

func TestMuteStatusRejectsNonUserTargets(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	for _, jid := range []types.JID{
		types.NewJID("120363000000000001", types.GroupServer),
		types.NewJID("120363000000000001", types.NewsletterServer),
		types.StatusBroadcastJID,
	} {
		if err := a.MuteStatus(context.Background(), jid, true); err == nil {
			t.Fatalf("MuteStatus(%s) succeeded, want a user JID error", jid)
		}
	}
	if calls := f.statusMuteCalls(); len(calls) != 0 {
		t.Fatalf("non-user targets reached WhatsApp: %+v", calls)
	}
}

func TestMuteStatusFailureLeavesNoLocalMute(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	f.statusChannels.statusMuteErr = errors.New("send failed")
	a.wa = f

	if err := a.MuteStatus(context.Background(), statusMutePN, true); err == nil {
		t.Fatal("MuteStatus succeeded after a failed send")
	}
	if _, ok, err := a.db.FindStatusMute(statusMutePN.String()); err != nil || ok {
		t.Fatalf("FindStatusMute after failure = %t, %v; want no row", ok, err)
	}
}

// The patch whatsmeow reports back is persisted in order after the local row.
func TestMuteStatusPersistsPostSendEvent(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	other := types.NewJID("15550000002", types.DefaultUserServer)
	f.statusChannels.statusMuteEvent = func(types.JID, bool) any {
		return statusMuteEvent(other, true, time.Now())
	}

	if err := a.MuteStatus(context.Background(), statusMutePN, true); err != nil {
		t.Fatalf("MuteStatus: %v", err)
	}
	if got := mustStatusMute(t, a.db, other.String()); !got.Muted {
		t.Fatalf("post-send event not persisted: %+v", got)
	}
}

func TestUserStatusMuteEventsAreMirroredBySync(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	f.lids[statusMuteLID] = statusMutePN
	a.wa = f

	var messagesStored, lastEvent atomic.Int64
	a.addSyncEventHandler(
		context.Background(),
		SyncOptions{},
		&messagesStored,
		&lastEvent,
		make(chan struct{}, 1),
		make(chan struct{}, 1),
		make(chan staleReconnectRequest, 1),
		func(string, string) {},
		nil,
		nil,
		&syncPresence{},
		nil,
	)
	when := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	f.emit(statusMuteEvent(statusMuteLID, true, when))
	if err := a.appStatePersist.waitIdle(context.Background()); err != nil {
		t.Fatalf("waitIdle: %v", err)
	}
	got := mustStatusMute(t, a.db, statusMutePN.String())
	if got.IndexJID != statusMuteLID.String() || !got.Muted || !got.UpdatedAt.Equal(when) {
		t.Fatalf("mirrored mute = %+v", got)
	}
	if lastEvent.Load() == 0 {
		t.Fatal("status mute event did not count as sync activity")
	}

	f.emit(statusMuteEvent(statusMuteLID, false, when.Add(time.Minute)))
	if err := a.appStatePersist.waitIdle(context.Background()); err != nil {
		t.Fatalf("waitIdle: %v", err)
	}
	if got := mustStatusMute(t, a.db, statusMuteLID.String()); got.Muted {
		t.Fatalf("unmute not mirrored: %+v", got)
	}
	required, err := a.db.AppStateRecoveryRequired(string(appstate.WAPatchRegularHigh))
	if err != nil || required {
		t.Fatalf("recovery required = %t, %v; want the live marker cleared", required, err)
	}
}

// One-shot commands persist status mutes that arrive while connected.
func TestChatStatePersistenceHandlerMirrorsStatusMutes(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	f.connectEvents = []any{statusMuteEvent(statusMutePN, true, time.Now())}

	remove, err := a.AddChatStatePersistenceHandler(context.Background())
	if err != nil {
		t.Fatalf("AddChatStatePersistenceHandler: %v", err)
	}
	if err := a.Connect(context.Background(), false, nil); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	remove()
	if err := a.appStatePersist.waitIdle(context.Background()); err != nil {
		t.Fatalf("waitIdle: %v", err)
	}
	if got := mustStatusMute(t, a.db, statusMutePN.String()); !got.Muted {
		t.Fatalf("connect-time status mute not persisted: %+v", got)
	}
}

func TestUserStatusMuteEventBelongsToRegularHigh(t *testing.T) {
	got := appStateCollectionsForEvent(statusMuteEvent(statusMutePN, true, time.Now()))
	if !slices.Equal(got, []appstate.WAPatchName{appstate.WAPatchRegularHigh}) {
		t.Fatalf("collections = %v, want regular_high", got)
	}
}

func TestHandleUserStatusMuteEventIgnoresIncompleteEvents(t *testing.T) {
	a := newTestApp(t)
	a.wa = newFakeWA()
	for _, evt := range []*events.UserStatusMute{
		nil,
		{Action: &waSyncAction.UserStatusMuteAction{Muted: proto.Bool(true)}},
		{JID: statusMutePN},
	} {
		if err := a.handleUserStatusMuteEvent(context.Background(), evt); err != nil {
			t.Fatalf("handleUserStatusMuteEvent(%+v): %v", evt, err)
		}
	}
	mutes, err := a.db.ListStatusMutes(true)
	if err != nil || len(mutes) != 0 {
		t.Fatalf("mutes = %+v, %v; want none", mutes, err)
	}
}
