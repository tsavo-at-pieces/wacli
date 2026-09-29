package wa

import (
	"slices"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Fictional identities only.
var (
	testChatPN    = types.NewJID("15550000001", types.DefaultUserServer)
	testChatLID   = types.NewJID("100000000001", types.HiddenUserServer)
	testGroup     = types.NewJID("120363000000000001", types.GroupServer)
	testGroupUser = types.NewJID("15550000002", types.DefaultUserServer)
)

func onlyMutation(t *testing.T, patch appstate.PatchInfo, collection appstate.WAPatchName, version int32) appstate.MutationInfo {
	t.Helper()
	if patch.Type != collection {
		t.Fatalf("collection = %s, want %s", patch.Type, collection)
	}
	if len(patch.Mutations) != 1 {
		t.Fatalf("mutations = %d, want 1", len(patch.Mutations))
	}
	m := patch.Mutations[0]
	if m.Version != version {
		t.Fatalf("version = %d, want %d", m.Version, version)
	}
	return m
}

func TestBuildDeleteChatPatch(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	m := onlyMutation(t, BuildDeleteChatPatch(testChatPN, at, nil, true), appstate.WAPatchRegularHigh, 6)
	if want := []string{"deleteChat", testChatPN.String(), "1"}; !slices.Equal(m.Index, want) {
		t.Fatalf("index = %q, want %q", m.Index, want)
	}
	r := m.Value.GetDeleteChatAction().GetMessageRange()
	if r.GetLastMessageTimestamp() != at.Unix() || len(r.GetMessages()) != 0 {
		t.Fatalf("range = %+v, want a keyless range at %d", r, at.Unix())
	}
	if idx := onlyMutation(t, BuildDeleteChatPatch(testChatPN, at, nil, false), appstate.WAPatchRegularHigh, 6).Index; idx[2] != "0" {
		t.Fatalf("delete media flag = %q", idx[2])
	}
}

func TestBuildClearChatPatch(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	key := &waCommon.MessageKey{RemoteJID: proto.String(testGroup.String()), ID: proto.String("MSG01"), FromMe: proto.Bool(true)}
	for _, tc := range []struct {
		starred, media bool
		index          []string
	}{
		{false, false, []string{"clearChat", testGroup.String(), "0", "0"}},
		{true, false, []string{"clearChat", testGroup.String(), "1", "0"}},
		{false, true, []string{"clearChat", testGroup.String(), "0", "1"}},
	} {
		m := onlyMutation(t, BuildClearChatPatch(testGroup, at, key, tc.starred, tc.media), appstate.WAPatchRegularHigh, 6)
		if !slices.Equal(m.Index, tc.index) {
			t.Fatalf("index = %q, want %q", m.Index, tc.index)
		}
		r := m.Value.GetClearChatAction().GetMessageRange()
		if r.GetLastMessageTimestamp() != at.Unix() || len(r.GetMessages()) != 1 || r.GetMessages()[0].GetKey().GetID() != "MSG01" {
			t.Fatalf("range = %+v", r)
		}
		if ClearChatDeletesStarred(m.Index) != tc.starred {
			t.Fatalf("ClearChatDeletesStarred(%q) = %t", m.Index, !tc.starred)
		}
	}
	if ClearChatDeletesStarred([]string{"deleteChat", testGroup.String(), "1"}) {
		t.Fatal("a deleteChat index reads as clear-starred")
	}
	// A zero time means now, as whatsmeow's builders do.
	if ts := BuildClearChatPatch(testGroup, time.Time{}, nil, false, false).Mutations[0].Value.GetClearChatAction().GetMessageRange().GetLastMessageTimestamp(); ts < time.Now().Add(-time.Minute).Unix() {
		t.Fatalf("zero-time range = %d", ts)
	}
}

func TestBuildLockChatPatch(t *testing.T) {
	for _, locked := range []bool{true, false} {
		m := onlyMutation(t, BuildLockChatPatch(testChatLID, locked), appstate.WAPatchRegularLow, 7)
		if want := []string{"lock", testChatLID.String()}; !slices.Equal(m.Index, want) {
			t.Fatalf("index = %q, want %q", m.Index, want)
		}
		if m.Value.GetLockChatAction() == nil || m.Value.GetLockChatAction().GetLocked() != locked {
			t.Fatalf("lock action = %+v, want locked=%t", m.Value.GetLockChatAction(), locked)
		}
	}
}

func TestBuildStarPatchParticipant(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chat   types.JID
		sender types.JID
		fromMe bool
		index  []string
	}{
		{"own message in a DM", testChatPN, types.EmptyJID, true, []string{"star", testChatPN.String(), "MSG01", "1", "0"}},
		{"their message in a DM", testChatPN, testChatPN, false, []string{"star", testChatPN.String(), "MSG01", "0", "0"}},
		{"own message in a group", testGroup, types.EmptyJID, true, []string{"star", testGroup.String(), "MSG01", "1", "0"}},
		{"their message in a group", testGroup, testGroupUser, false, []string{"star", testGroup.String(), "MSG01", "0", testGroupUser.String()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := onlyMutation(t, BuildStarPatch(tc.chat, tc.sender, "MSG01", tc.fromMe, true), appstate.WAPatchRegularHigh, 2)
			if !slices.Equal(m.Index, tc.index) {
				t.Fatalf("index = %q, want %q", m.Index, tc.index)
			}
			if !m.Value.GetStarAction().GetStarred() {
				t.Fatal("star action not starred")
			}
		})
	}
	if onlyMutation(t, BuildStarPatch(testChatPN, testChatPN, "MSG01", false, false), appstate.WAPatchRegularHigh, 2).Value.GetStarAction().GetStarred() {
		t.Fatal("unstar sent starred=true")
	}
}

func TestBuildLabelEditPatch(t *testing.T) {
	order := int32(4)
	custom := waSyncAction.LabelEditAction_CUSTOM
	m := onlyMutation(t, BuildLabelEditPatch("12", LabelDefinition{Name: "Test list", Color: 3, OrderIndex: &order, Type: &custom}), appstate.WAPatchRegular, 3)
	if want := []string{"label_edit", "12"}; !slices.Equal(m.Index, want) {
		t.Fatalf("index = %q", m.Index)
	}
	a := m.Value.GetLabelEditAction()
	if a.GetName() != "Test list" || a.GetColor() != 3 || a.GetOrderIndex() != 4 || a.GetType() != custom || a.GetDeleted() {
		t.Fatalf("label action = %+v", a)
	}
	// Unset optional fields stay unset rather than becoming zero values.
	if a.IsActive != nil || a.PredefinedID != nil || a.IsImmutable != nil || a.MuteEndTimeMS != nil {
		t.Fatalf("label action set optional fields: %+v", a)
	}
	deleted := onlyMutation(t, BuildLabelEditPatch("12", LabelDefinition{Name: "Test list", Deleted: true}), appstate.WAPatchRegular, 3)
	if !deleted.Value.GetLabelEditAction().GetDeleted() {
		t.Fatal("delete did not set deleted")
	}
}

func TestBuildLabelChatPatch(t *testing.T) {
	patch := BuildLabelChatPatch("12", []types.JID{testChatLID, testChatPN}, false)
	if patch.Type != appstate.WAPatchRegular || len(patch.Mutations) != 2 {
		t.Fatalf("patch = %+v", patch)
	}
	for i, target := range []types.JID{testChatLID, testChatPN} {
		m := patch.Mutations[i]
		if want := []string{"label_jid", "12", target.String()}; !slices.Equal(m.Index, want) || m.Version != 3 {
			t.Fatalf("mutation %d = %q v%d", i, m.Index, m.Version)
		}
		if m.Value.GetLabelAssociationAction().GetLabeled() {
			t.Fatalf("mutation %d labeled a removal", i)
		}
	}
}

func TestBuildFavoritesPatch(t *testing.T) {
	m := onlyMutation(t, BuildFavoritesPatch([]string{testGroup.String(), testChatLID.String()}), appstate.WAPatchRegularHigh, 1)
	if want := []string{"favorites"}; !slices.Equal(m.Index, want) {
		t.Fatalf("index = %q", m.Index)
	}
	var ids []string
	for _, f := range m.Value.GetFavoritesAction().GetFavorites() {
		ids = append(ids, f.GetID())
	}
	if !slices.Equal(ids, []string{testGroup.String(), testChatLID.String()}) {
		t.Fatalf("favorites = %q", ids)
	}
	// An empty list is a valid value: it clears favorites.
	empty := onlyMutation(t, BuildFavoritesPatch(nil), appstate.WAPatchRegularHigh, 1)
	if empty.Value.GetFavoritesAction() == nil || len(empty.Value.GetFavoritesAction().GetFavorites()) != 0 {
		t.Fatalf("empty favorites = %+v", empty.Value)
	}
}

func TestMessageRangeBoundary(t *testing.T) {
	if _, ok := MessageRangeBoundary(nil); ok {
		t.Fatal("nil range has a boundary")
	}
	r := &waSyncAction.SyncActionMessageRange{
		LastMessageTimestamp: proto.Int64(1790000000),
		Messages:             []*waSyncAction.SyncActionMessage{{Timestamp: proto.Int64(1790000100)}},
	}
	if got, ok := MessageRangeBoundary(r); !ok || got.Unix() != 1790000100 {
		t.Fatalf("boundary = %s, %t; want the newest listed message", got, ok)
	}
	ms := &waSyncAction.SyncActionMessageRange{LastMessageTimestamp: proto.Int64(1790000000123)}
	if got, ok := MessageRangeBoundary(ms); !ok || got.Unix() != 1790000000 {
		t.Fatalf("millisecond boundary = %s, %t", got, ok)
	}
}

func TestBuildPinAndKeepMessages(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	key := MessageKey(testGroup, "MSG01", false, testGroupUser)
	if key.GetParticipant() != testGroupUser.String() || key.GetFromMe() || key.GetRemoteJID() != testGroup.String() {
		t.Fatalf("group key = %+v", key)
	}
	if own := MessageKey(testGroup, "MSG01", true, testGroupUser); own.Participant != nil {
		t.Fatalf("own group key names a participant: %+v", own)
	}
	if dm := MessageKey(testChatPN, "MSG01", false, testChatPN); dm.Participant != nil {
		t.Fatalf("DM key names a participant: %+v", dm)
	}

	pin := BuildPinInChatMessage(key, true, 7*24*time.Hour, now)
	if pin.GetPinInChatMessage().GetType() != waE2E.PinInChatMessage_PIN_FOR_ALL ||
		pin.GetPinInChatMessage().GetSenderTimestampMS() != now.UnixMilli() ||
		pin.GetMessageContextInfo().GetMessageAddOnDurationInSecs() != 604800 ||
		pin.GetPinInChatMessage().GetKey().GetID() != "MSG01" {
		t.Fatalf("pin = %+v", pin)
	}
	unpin := BuildPinInChatMessage(key, false, time.Hour, now)
	if unpin.GetPinInChatMessage().GetType() != waE2E.PinInChatMessage_UNPIN_FOR_ALL || unpin.GetMessageContextInfo().GetMessageAddOnDurationInSecs() != 0 {
		t.Fatalf("unpin = %+v", unpin)
	}
	keep := BuildKeepInChatMessage(key, true, now)
	if keep.GetKeepInChatMessage().GetKeepType() != waE2E.KeepType_KEEP_FOR_ALL || keep.GetKeepInChatMessage().GetTimestampMS() != now.UnixMilli() {
		t.Fatalf("keep = %+v", keep)
	}
	if BuildKeepInChatMessage(key, false, now).GetKeepInChatMessage().GetKeepType() != waE2E.KeepType_UNDO_KEEP_FOR_ALL {
		t.Fatal("unkeep type")
	}
}

func TestBuildVCardAndContactsMessage(t *testing.T) {
	card := ContactCard{Name: "Test, Person; Jr", Phone: "15550000002"}
	vcard := BuildVCard(card)
	for _, want := range []string{"BEGIN:VCARD\n", "VERSION:3.0\n", `FN:Test\, Person\; Jr`, "TEL;type=CELL;type=VOICE;waid=15550000002:+15550000002\n", "END:VCARD"} {
		if !strings.Contains(vcard, want) {
			t.Fatalf("vcard %q missing %q", vcard, want)
		}
	}
	one, err := BuildContactsMessage([]ContactCard{card})
	if err != nil || one.GetContactMessage().GetDisplayName() != card.Name {
		t.Fatalf("single contact = %+v, %v", one, err)
	}
	// The text wacli stores matches what sync stores for a received card.
	if got := ContactMessageText(one); got != "Contact: Test, Person; Jr (+15550000002)" {
		t.Fatalf("contact text = %q", got)
	}
	two, err := BuildContactsMessage([]ContactCard{card, {Name: "Sam", Phone: "15550000001"}})
	if err != nil || len(two.GetContactsArrayMessage().GetContacts()) != 2 || two.GetContactsArrayMessage().GetDisplayName() != "2 contacts" {
		t.Fatalf("contacts array = %+v, %v", two, err)
	}
	if _, err := BuildContactsMessage(nil); err == nil {
		t.Fatal("empty contact list accepted")
	}
}

func TestBuildEventMessage(t *testing.T) {
	start := time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC)
	msg := BuildEventMessage(EventDetails{Name: "Test event", Description: "Fictional plans", Start: start, End: start.Add(2 * time.Hour), Location: "Test place", JoinLink: "https://call.whatsapp.com/voice/FakeToken01"})
	ev := msg.GetEventMessage()
	if ev.GetName() != "Test event" || ev.GetStartTime() != start.Unix() || ev.GetEndTime() != start.Add(2*time.Hour).Unix() ||
		ev.GetLocation().GetName() != "Test place" || ev.GetDescription() != "Fictional plans" || ev.GetIsCanceled() ||
		ev.GetJoinLink() != "https://call.whatsapp.com/voice/FakeToken01" {
		t.Fatalf("event = %+v", ev)
	}
	if len(msg.GetMessageContextInfo().GetMessageSecret()) != 32 {
		t.Fatal("event has no 32-byte message secret for RSVPs")
	}
	bare := BuildEventMessage(EventDetails{Name: "Bare", Start: start}).GetEventMessage()
	if bare.EndTime != nil || bare.Location != nil || bare.Description != nil || bare.JoinLink != nil {
		t.Fatalf("bare event set optional fields: %+v", bare)
	}
	text := EventMessageText(ev)
	for _, want := range []string{"Event: Test event", "Starts: 2026-10-01T18:00:00Z, ends: 2026-10-01T20:00:00Z", "Location: Test place", "Fictional plans"} {
		if !strings.Contains(text, want) {
			t.Fatalf("event text %q missing %q", text, want)
		}
	}
}

func TestWrapViewOnce(t *testing.T) {
	img, err := WrapViewOnce(&waProto.Message{ImageMessage: &waE2E.ImageMessage{}})
	if err != nil || !img.GetViewOnceMessageV2().GetMessage().GetImageMessage().GetViewOnce() {
		t.Fatalf("image = %+v, %v", img, err)
	}
	vid, err := WrapViewOnce(&waProto.Message{VideoMessage: &waE2E.VideoMessage{}})
	if err != nil || !vid.GetViewOnceMessageV2().GetMessage().GetVideoMessage().GetViewOnce() {
		t.Fatalf("video = %+v, %v", vid, err)
	}
	voice, err := WrapViewOnce(&waProto.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true)}})
	if err != nil || !voice.GetViewOnceMessageV2Extension().GetMessage().GetAudioMessage().GetViewOnce() {
		t.Fatalf("voice = %+v, %v", voice, err)
	}
	for _, msg := range []*waProto.Message{
		{AudioMessage: &waE2E.AudioMessage{}},
		{DocumentMessage: &waE2E.DocumentMessage{}},
		{StickerMessage: &waE2E.StickerMessage{}},
	} {
		if _, err := WrapViewOnce(msg); err == nil {
			t.Fatalf("view once accepted %+v", msg)
		}
	}
}

func TestParseLiveMessageReadsPinKeepAndEvent(t *testing.T) {
	info := types.MessageInfo{
		MessageSource: types.MessageSource{Chat: testGroup, Sender: testGroupUser, IsGroup: true},
		ID:            "PIN01",
		Timestamp:     time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}
	key := MessageKey(testGroup, "MSG01", true, types.EmptyJID)
	now := time.Date(2026, 9, 1, 11, 59, 0, 0, time.UTC)

	pin := ParseLiveMessage(&events.Message{Info: info, Message: BuildPinInChatMessage(key, true, 24*time.Hour, now)})
	if pin.Pin == nil || !pin.Pin.Pinned || pin.Pin.TargetID != "MSG01" || !pin.Pin.TargetFromMe || pin.Pin.Duration != 24*time.Hour || !pin.Pin.ChangedAt.Equal(now) {
		t.Fatalf("pin = %+v", pin.Pin)
	}
	if pin.UnhandledPayload != "" {
		t.Fatalf("pin reported as unhandled: %q", pin.UnhandledPayload)
	}
	unpin := ParseLiveMessage(&events.Message{Info: info, Message: BuildPinInChatMessage(key, false, 0, now)})
	if unpin.Pin == nil || unpin.Pin.Pinned || unpin.Pin.Duration != 0 {
		t.Fatalf("unpin = %+v", unpin.Pin)
	}
	keep := ParseLiveMessage(&events.Message{Info: info, Message: BuildKeepInChatMessage(key, true, now)})
	if keep.Keep == nil || !keep.Keep.Kept || keep.Keep.TargetID != "MSG01" || keep.UnhandledPayload != "" {
		t.Fatalf("keep = %+v unhandled=%q", keep.Keep, keep.UnhandledPayload)
	}
	event := ParseLiveMessage(&events.Message{Info: info, Message: BuildEventMessage(EventDetails{Name: "Test event", Start: now})})
	if !strings.HasPrefix(event.Text, "Event: Test event") || event.UnhandledPayload != "" {
		t.Fatalf("event text = %q unhandled=%q", event.Text, event.UnhandledPayload)
	}
}
