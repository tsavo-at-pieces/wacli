package app

import (
	"context"
	"sync/atomic"
	"testing"

	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Fictional identities only.
var (
	bookEventPN  = types.NewJID("15550000001", types.DefaultUserServer)
	bookEventLID = types.NewJID("100000000001", types.HiddenUserServer)
)

func startContactEventSync(t *testing.T) (*App, *fakeWA) {
	t.Helper()
	a := newTestApp(t)
	f := newFakeWA()
	f.lids[bookEventLID] = bookEventPN
	a.wa = f
	var messagesStored, lastEvent atomic.Int64
	handlerID, _ := a.addSyncEventHandler(
		context.Background(),
		SyncOptions{Mode: SyncModeFollow},
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
	t.Cleanup(func() { f.RemoveEventHandler(handlerID) })
	return a, f
}

func contactName(t *testing.T, a *App, jid types.JID) string {
	t.Helper()
	c, err := a.db.GetContact(jid.String())
	if err != nil {
		t.Fatalf("GetContact %s: %v", jid, err)
	}
	return c.Name
}

func TestSyncMirrorsWhatsAppContactEntries(t *testing.T) {
	a, f := startContactEventSync(t)
	if err := a.db.UpsertContact(bookEventPN.String(), bookEventPN.User, "Sammy", "Old Name", "Old", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.db.UpsertContact(bookEventLID.String(), "", "", "Old Name", "", ""); err != nil {
		t.Fatal(err)
	}

	// A save from another device, keyed by LID, lands on the phone row and
	// renames the existing LID row.
	f.emit(&events.Contact{JID: bookEventLID, Action: &waSyncAction.ContactAction{
		FullName:  proto.String("Sam Example"),
		FirstName: proto.String("Sam"),
	}})
	if got := contactName(t, a, bookEventPN); got != "Sam Example" {
		t.Fatalf("phone row name = %q, want the saved name", got)
	}
	if got := contactName(t, a, bookEventLID); got != "Sam Example" {
		t.Fatalf("LID row name = %q, want the saved name", got)
	}

	// Names are replaced exactly: an entry without a name falls back to the
	// push name instead of keeping the old saved name.
	f.emit(&events.Contact{JID: bookEventPN, Action: &waSyncAction.ContactAction{LidJID: proto.String(bookEventLID.String())}})
	if got := contactName(t, a, bookEventPN); got != "Sammy" {
		t.Fatalf("phone row name = %q, want the push name after the saved name was cleared", got)
	}
	if got := contactName(t, a, bookEventLID); got != "" {
		t.Fatalf("LID row name = %q, want it cleared", got)
	}

	// Events without an action or JID are ignored.
	f.emit(&events.Contact{JID: bookEventPN})
	f.emit(&events.Contact{Action: &waSyncAction.ContactAction{FullName: proto.String("Nobody")}})
	if got := contactName(t, a, bookEventPN); got != "Sammy" {
		t.Fatalf("phone row name = %q after ignored events", got)
	}
}

func TestSyncMirrorsBlocklistChanges(t *testing.T) {
	a, f := startContactEventSync(t)
	blocked := func(jid types.JID) bool {
		t.Helper()
		got, err := a.db.AnyContactBlocked([]string{jid.String()})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	f.emit(&events.Blocklist{Changes: []events.BlocklistChange{{JID: bookEventLID, Action: events.BlocklistChangeActionBlock}}})
	if !blocked(bookEventLID) || !blocked(bookEventPN) {
		t.Fatal("block by LID did not mark both identities blocked")
	}
	// "modify" means refetch; it carries nothing to apply.
	f.emit(&events.Blocklist{Action: events.BlocklistActionModify})
	if !blocked(bookEventPN) {
		t.Fatal("modify notification changed the local block list")
	}
	f.emit(&events.Blocklist{Changes: []events.BlocklistChange{{JID: bookEventLID, Action: events.BlocklistChangeActionUnblock}}})
	if blocked(bookEventLID) || blocked(bookEventPN) {
		t.Fatal("unblock did not clear both identities")
	}
}
