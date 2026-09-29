package wa

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/sqliteutil"
	"go.mau.fi/whatsmeow/appstate"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waServerSync"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Fictional identities only.
var (
	bookPN  = types.NewJID("15550000001", types.DefaultUserServer)
	bookLID = types.NewJID("100000000001", types.HiddenUserServer)
)

func TestBuildContactSavePatchUsesContactIndexInCriticalUnblockLow(t *testing.T) {
	patch := buildContactSavePatch(bookPN, bookLID, " Sam ", " Sam Example ", true)
	if patch.Type != appstate.WAPatchCriticalUnblockLow {
		t.Fatalf("collection = %q, want critical_unblock_low", patch.Type)
	}
	if len(patch.Mutations) != 1 {
		t.Fatalf("mutations = %d, want 1", len(patch.Mutations))
	}
	m := patch.Mutations[0]
	if !slices.Equal(m.Index, []string{"contact", "15550000001@s.whatsapp.net"}) || m.Version != 2 {
		t.Fatalf("index/version = %q/%d", m.Index, m.Version)
	}
	act := m.Value.GetContactAction()
	if act.GetFullName() != "Sam Example" || act.GetFirstName() != "Sam" || act.GetLidJID() != "100000000001@lid" ||
		!act.GetSaveOnPrimaryAddressbook() || act.PnJID != nil || act.Username != nil {
		t.Fatalf("contact action = %+v", act)
	}
}

func TestBuildContactSavePatchOmitsUnknownFields(t *testing.T) {
	act := buildContactSavePatch(bookPN, types.JID{}, "", "Sam Example", false).Mutations[0].Value.GetContactAction()
	if act.FirstName != nil || act.LidJID != nil || act.SaveOnPrimaryAddressbook != nil {
		t.Fatalf("contact action = %+v, want only the full name", act)
	}
}

func TestContactRemovalMutationsCoverEachIdentity(t *testing.T) {
	got := contactRemovalMutations([]types.JID{bookPN, bookLID})
	if len(got) != 2 {
		t.Fatalf("mutations = %d, want 2", len(got))
	}
	for i, want := range []string{bookPN.String(), bookLID.String()} {
		if !slices.Equal(got[i].Index, []string{appstate.IndexContact, want}) || got[i].Version != 2 || got[i].Value.GetContactAction() == nil {
			t.Fatalf("mutation %d = %+v", i, got[i])
		}
	}
}

// The hand-encoded REMOVE must pass whatsmeow's own decoder with every MAC
// and the LTHash checked, exactly as other linked devices will check it.
func TestEncodeAppStateRemovalPassesWhatsmeowVerification(t *testing.T) {
	ctx := context.Background()
	device, keyID := newAppStateFixture(t)
	proc := appstate.NewProcessor(device, waLog.Noop)
	name := appstate.WAPatchCriticalUnblockLow

	saved, err := proc.EncodePatch(ctx, keyID, appstate.HashState{}, buildContactSavePatch(bookPN, bookLID, "Sam", "Sam Example", false))
	if err != nil {
		t.Fatalf("encode save: %v", err)
	}
	afterSave, mutations := applyFixturePatch(t, proc, name, appstate.HashState{}, saved)
	if len(mutations) != 1 || mutations[0].Operation != waServerSync.SyncdMutation_SET || mutations[0].Action.GetContactAction().GetFullName() != "Sam Example" {
		t.Fatalf("saved mutations = %+v", mutations)
	}

	stores := appStateStores{state: device.AppState, keys: device.AppStateKeys}
	removal, err := encodeAppStateRemoval(ctx, stores, name, contactRemovalMutations([]types.JID{bookPN, bookLID}), time.UnixMilli(1790000000000))
	if err != nil {
		t.Fatalf("encode removal: %v", err)
	}
	if removal.fromVersion != afterSave.Version {
		t.Fatalf("removal from version %d, want %d", removal.fromVersion, afterSave.Version)
	}
	// Only the phone-number entry exists; the LID candidate is skipped.
	if !slices.EqualFunc(removal.removed, [][]string{{"contact", bookPN.String()}}, slices.Equal) {
		t.Fatalf("removed = %q", removal.removed)
	}

	afterRemove, mutations := applyFixturePatch(t, proc, name, afterSave, removal.patch)
	if len(mutations) != 1 || mutations[0].Operation != waServerSync.SyncdMutation_REMOVE ||
		!slices.Equal(mutations[0].Index, []string{"contact", bookPN.String()}) || mutations[0].Version != 2 {
		t.Fatalf("removal mutations = %+v", mutations)
	}
	if mutations[0].Action.GetTimestamp() != 1790000000000 || mutations[0].Action.GetContactAction() == nil {
		t.Fatalf("removal action = %+v", mutations[0].Action)
	}
	// SET then REMOVE of one entry leaves the collection's LTHash empty again.
	if afterRemove.Hash != ([128]byte{}) {
		t.Fatal("LTHash after removing the only entry is not empty")
	}
	if mac, err := device.AppState.GetAppStateMutationMAC(ctx, string(name), mutations[0].IndexMAC); err != nil || mac != nil {
		t.Fatalf("stored value MAC after removal = %x, %v; want none", mac, err)
	}

	_, err = encodeAppStateRemoval(ctx, stores, name, contactRemovalMutations([]types.JID{bookPN}), time.Now())
	if !errors.Is(err, errNothingToRemove) {
		t.Fatalf("second removal error = %v, want errNothingToRemove", err)
	}
}

func TestEncodeAppStateRemovalRequiresAppStateKey(t *testing.T) {
	ctx := context.Background()
	container, err := sqlstore.New(ctx, "sqlite3", sqliteutil.FileURI(filepath.Join(t.TempDir(), "session.db"), "_foreign_keys=on"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = container.Close() })
	device := saveFixtureDevice(t, container)
	_, err = encodeAppStateRemoval(ctx, appStateStores{state: device.AppState, keys: device.AppStateKeys}, appstate.WAPatchCriticalUnblockLow, contactRemovalMutations([]types.JID{bookPN}), time.Now())
	if err == nil || err.Error() != "no app state keys found" {
		t.Fatalf("error = %v, want missing key", err)
	}
}

func TestAppStateSendErrorReadsCollectionResult(t *testing.T) {
	response := func(attrs waBinary.Attrs, children ...waBinary.Node) *waBinary.Node {
		collection := waBinary.Node{Tag: "collection", Attrs: attrs}
		if len(children) > 0 {
			collection.Content = children
		}
		return &waBinary.Node{Tag: "iq", Content: []waBinary.Node{{Tag: "sync", Content: []waBinary.Node{collection}}}}
	}
	name := appstate.WAPatchCriticalUnblockLow

	if conflict, err := appStateSendError(response(waBinary.Attrs{"name": string(name)}), name); conflict || err != nil {
		t.Fatalf("success = %t, %v", conflict, err)
	}
	conflict, err := appStateSendError(response(waBinary.Attrs{"type": "error"}, waBinary.Node{Tag: "error", Attrs: waBinary.Attrs{"code": "409"}}), name)
	if !conflict || err == nil {
		t.Fatalf("409 = %t, %v; want a retryable conflict", conflict, err)
	}
	conflict, err = appStateSendError(response(waBinary.Attrs{"type": "error"}, waBinary.Node{Tag: "error", Attrs: waBinary.Attrs{"code": "400"}}), name)
	if conflict || err == nil {
		t.Fatalf("400 = %t, %v; want a final error", conflict, err)
	}
	if _, err := appStateSendError(&waBinary.Node{Tag: "iq"}, name); err == nil {
		t.Fatal("missing collection accepted")
	}
}

func newAppStateFixture(t *testing.T) (*wastore.Device, []byte) {
	t.Helper()
	ctx := context.Background()
	container, err := sqlstore.New(ctx, "sqlite3", sqliteutil.FileURI(filepath.Join(t.TempDir(), "session.db"), "_foreign_keys=on"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = container.Close() })
	device := saveFixtureDevice(t, container)
	keyID := []byte("fictional-key-01")
	if err := device.AppStateKeys.PutAppStateSyncKey(ctx, keyID, wastore.AppStateSyncKey{
		Data:        bytes.Repeat([]byte{0x5a}, 32),
		Fingerprint: []byte("fictional"),
		Timestamp:   1790000000000,
	}); err != nil {
		t.Fatal(err)
	}
	return device, keyID
}

func saveFixtureDevice(t *testing.T, container *sqlstore.Container) *wastore.Device {
	t.Helper()
	device := container.NewDevice()
	jid := types.JID{User: "15550000000", Device: 1, Server: types.DefaultUserServer}
	device.ID = &jid
	device.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte("fictional fixture"), AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := device.Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	return device
}

// applyFixturePatch plays the server: it stamps the next version on an
// encoded patch and decodes it with MAC validation, storing the result.
func applyFixturePatch(t *testing.T, proc *appstate.Processor, name appstate.WAPatchName, state appstate.HashState, encoded []byte) (appstate.HashState, []appstate.Mutation) {
	t.Helper()
	var patch waServerSync.SyncdPatch
	if err := proto.Unmarshal(encoded, &patch); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	patch.Version = &waServerSync.SyncdVersion{Version: proto.Uint64(state.Version + 1)}
	mutations, newState, err := proc.DecodePatches(context.Background(), &appstate.PatchList{Name: name, Patches: []*waServerSync.SyncdPatch{&patch}}, state, true)
	if err != nil {
		t.Fatalf("whatsmeow rejected the patch: %v", err)
	}
	return newState, mutations
}
