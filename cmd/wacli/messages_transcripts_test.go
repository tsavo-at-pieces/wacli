package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/transcripts"
	"go.mau.fi/whatsmeow/types"
)

func seedTranscript(t *testing.T, storeDir, chat, id, text string) {
	t.Helper()
	db, err := transcripts.Open(transcripts.PathFor(storeDir))
	if err != nil {
		t.Fatalf("transcripts.Open: %v", err)
	}
	defer db.Close()
	if err := db.Upsert(transcripts.Transcript{ChatJID: chat, MsgID: id, Text: text, Engine: "command", Model: "fake-model", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
}

type messagesPayload struct {
	Messages []store.Message `json:"messages"`
}

func findMessage(msgs []store.Message, id string) (store.Message, bool) {
	for _, m := range msgs {
		if m.MsgID == id {
			return m, true
		}
	}
	return store.Message{}, false
}

func transcriptOf(m store.Message) string {
	if m.Transcript == nil {
		return "<nil>"
	}
	return *m.Transcript
}

func TestMessagesCommandsSurfaceTranscripts(t *testing.T) {
	h := newTranscribeHarness(t)
	const text = "made-up words about the picnic"
	seedTranscript(t, h.storeDir, tChat, "voice-local", text)
	jsonFlags := func() *rootFlags { return &rootFlags{storeDir: h.storeDir, asJSON: true, timeout: time.Minute} }
	humanFlags := func() *rootFlags { return &rootFlags{storeDir: h.storeDir, timeout: time.Minute} }

	raw, err := runCommandJSON(t, newMessagesListCmd(jsonFlags()), "--chat", tChat)
	if err != nil {
		t.Fatalf("messages list: %v", err)
	}
	list := decodeData[messagesPayload](t, raw).Messages
	voice, ok := findMessage(list, "voice-local")
	if !ok || transcriptOf(voice) != text || voice.TranscriptEngine != "command" {
		t.Fatalf("list voice = %+v", voice)
	}
	if voice.Text != "[Audio]" || voice.DisplayText != "Sent audio" {
		t.Fatalf("transcript leaked into typed text fields: text=%q display=%q", voice.Text, voice.DisplayText)
	}
	if other, _ := findMessage(list, "voice-download"); other.Transcript != nil {
		t.Fatalf("untranscribed audio has a transcript: %+v", other)
	}
	if note, _ := findMessage(list, "text-1"); note.Transcript != nil || strings.Contains(raw, `"transcript":null`) {
		t.Fatalf("text message JSON carries a transcript: %s", raw)
	}

	raw, err = runCommandJSON(t, newMessagesListCmd(humanFlags()), "--chat", tChat)
	if err != nil {
		t.Fatalf("messages list human: %v", err)
	}
	if !strings.Contains(raw, "[Voice] "+text) {
		t.Fatalf("human list missing transcript:\n%s", raw)
	}

	raw, err = runCommandJSON(t, newMessagesShowCmd(jsonFlags()), "--chat", tChat, "--id", "voice-local")
	if err != nil {
		t.Fatalf("messages show: %v", err)
	}
	if shown := decodeData[store.Message](t, raw); transcriptOf(shown) != text || shown.TranscriptEngine != "command" {
		t.Fatalf("show = %+v", shown)
	}
	raw, err = runCommandJSON(t, newMessagesShowCmd(humanFlags()), "--chat", tChat, "--id", "voice-local")
	if err != nil {
		t.Fatalf("messages show human: %v", err)
	}
	if !strings.Contains(raw, "[Voice] "+text) || !strings.Contains(raw, "Transcribed by: command") {
		t.Fatalf("human show:\n%s", raw)
	}

	raw, err = runCommandJSON(t, newMessagesContextCmd(jsonFlags()), "--chat", tChat, "--id", "voice-download", "--before", "2", "--after", "0")
	if err != nil {
		t.Fatalf("messages context: %v", err)
	}
	if m, ok := findMessage(decodeData[[]store.Message](t, raw), "voice-local"); !ok || transcriptOf(m) != text {
		t.Fatalf("context voice = %+v", m)
	}

	raw, err = runCommandJSON(t, newMessagesStarredCmd(jsonFlags()))
	if err != nil {
		t.Fatalf("messages starred: %v", err)
	}

	output := filepath.Join(t.TempDir(), "export.json")
	if _, err := runCommandJSON(t, newMessagesExportCmd(jsonFlags()), "--chat", tChat, "--output", output); err != nil {
		t.Fatalf("messages export: %v", err)
	}
	exported, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := findMessage(decodeData[messagesPayload](t, string(exported)).Messages, "voice-local"); !ok || transcriptOf(m) != text {
		t.Fatalf("export voice = %+v", m)
	}
}

func TestMessagesSearchMatchesTranscripts(t *testing.T) {
	h := newTranscribeHarness(t)
	seedTranscript(t, h.storeDir, tChat, "voice-local", "made-up words about the Picnic on saturday")
	seedTranscript(t, h.storeDir, tChat, "voice-download", "nothing relevant here")
	search := func(args ...string) []store.Message {
		t.Helper()
		raw, err := runCommandJSON(t, newMessagesSearchCmd(&rootFlags{storeDir: h.storeDir, asJSON: true, timeout: time.Minute}), args...)
		if err != nil {
			t.Fatalf("messages search %v: %v", args, err)
		}
		return decodeData[messagesPayload](t, raw).Messages
	}

	got := search("picnic")
	if messageIDs(got) != "voice-local" {
		t.Fatalf("search picnic = %s", messageIDs(got))
	}
	if !strings.HasPrefix(got[0].Snippet, "[Voice] ") || !strings.Contains(got[0].Snippet, "[Picnic]") || transcriptOf(got[0]) == "<nil>" {
		t.Fatalf("snippet = %q transcript = %q", got[0].Snippet, transcriptOf(got[0]))
	}
	if ids := messageIDs(search("picnic saturday")); ids != "voice-local" {
		t.Fatalf("multi-word search = %s", ids)
	}
	if ids := messageIDs(search("picnic", "--type", "audio", "--has-media")); ids != "voice-local" {
		t.Fatalf("--type audio = %s", ids)
	}
	if ids := messageIDs(search("picnic", "--type", "text")); ids != "" {
		t.Fatalf("--type text returned transcript matches: %s", ids)
	}
	if ids := messageIDs(search("picnic", "--chat", tOther)); ids != "" {
		t.Fatalf("--chat other returned %s", ids)
	}
	if ids := messageIDs(search("picnic", "--after", "2026-04-02")); ids != "" {
		t.Fatalf("--after returned %s", ids)
	}
	if ids := messageIDs(search("picnic", "--starred")); ids != "" {
		t.Fatalf("--starred returned %s", ids)
	}

	// "made-up" matches the typed note and the transcript: both come back,
	// newest first, and --limit applies to the merged list.
	if ids := messageIDs(search("made-up")); ids != "text-1,voice-local" {
		t.Fatalf("merged search = %s", ids)
	}
	if ids := messageIDs(search("made-up", "--limit", "1")); ids != "text-1" {
		t.Fatalf("merged search with limit = %s", ids)
	}

	// An audio message whose own text matches is not duplicated; it gains
	// the transcript.
	if got := search("audio"); strings.Count(messageIDs(got), "voice-local") != 1 {
		t.Fatalf("duplicate rows: %s", messageIDs(got))
	} else if m, ok := findMessage(got, "voice-local"); ok && m.Transcript == nil {
		t.Fatalf("text match lost its transcript: %+v", m)
	}

	raw, err := runCommandJSON(t, newMessagesSearchCmd(&rootFlags{storeDir: h.storeDir, timeout: time.Minute}), "picnic")
	if err != nil {
		t.Fatalf("human search: %v", err)
	}
	if !strings.Contains(raw, "[Voice] made-up words about the [Picnic] on saturday") {
		t.Fatalf("human search:\n%s", raw)
	}
}

func TestMessagesReadCommandsDoNotCreateTranscriptsDB(t *testing.T) {
	h := newTranscribeHarness(t)
	flags := func() *rootFlags { return &rootFlags{storeDir: h.storeDir, asJSON: true, timeout: time.Minute} }
	for _, run := range []func() (string, error){
		func() (string, error) { return runCommandJSON(t, newMessagesListCmd(flags())) },
		func() (string, error) {
			return runCommandJSON(t, newMessagesShowCmd(flags()), "--chat", tChat, "--id", "voice-local")
		},
		func() (string, error) {
			return runCommandJSON(t, newMessagesContextCmd(flags()), "--chat", tChat, "--id", "voice-local")
		},
		func() (string, error) { return runCommandJSON(t, newMessagesSearchCmd(flags()), "audio") },
	} {
		raw, err := run()
		if err != nil {
			t.Fatalf("read command: %v", err)
		}
		if strings.Contains(raw, `"transcript`) {
			t.Fatalf("output mentions transcripts without a sidecar: %s", raw)
		}
	}
	if _, err := os.Stat(transcripts.PathFor(h.storeDir)); !os.IsNotExist(err) {
		t.Fatalf("read commands created transcripts.db (stat err %v)", err)
	}
}

// A transcript is keyed by the store's canonical phone-number chat; a row
// still stored under the chat's LID must find it, and transcribing that row
// must write the canonical key.
func TestTranscriptsMatchAcrossLIDAndPhoneNumber(t *testing.T) {
	h := newTranscribeHarness(t)
	db, err := store.Open(filepath.Join(h.storeDir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertChat(tLID, "dm", "Synthetic Contact", transcribeFixtureBase); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"voice-lid", "voice-lid-2"} {
		if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: tLID, MsgID: id, SenderJID: tLID, SenderName: "Synthetic Contact", Timestamp: transcribeFixtureBase.Add(10 * time.Minute), Text: "[Audio]", DisplayText: "Sent audio", MediaType: "audio", DirectPath: "/v/fake-lid", MediaKey: []byte{7}}); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	writeTestSessionLIDMap(t, filepath.Join(h.storeDir, "session.db"), "100000000001", "15550000001")
	seedTranscript(t, h.storeDir, tChat, "voice-lid", "made-up words under the phone number")

	raw, err := runCommandJSON(t, newMessagesListCmd(&rootFlags{storeDir: h.storeDir, asJSON: true, readOnly: true, timeout: time.Minute}), "--chat", tLID)
	if err != nil {
		t.Fatalf("messages list: %v", err)
	}
	if m, ok := findMessage(decodeData[messagesPayload](t, raw).Messages, "voice-lid"); !ok || transcriptOf(m) != "made-up words under the phone number" {
		t.Fatalf("LID row transcript = %+v", m)
	}

	t.Setenv("WACLI_TEST_TRANSCRIPT", "made-up words stored canonically")
	if _, err := h.transcribe(t, nil, "--chat", tLID, "--id", "voice-lid-2"); err != nil {
		t.Fatalf("transcribe LID row: %v", err)
	}
	if _, ok := storedTranscript(t, h.storeDir, tChat, "voice-lid-2"); !ok {
		t.Fatal("transcript for a LID row was not keyed by the phone-number chat")
	}
	// Asking again through the phone number finds the cached transcript.
	raw, err = h.transcribe(t, nil, "--chat", tChat, "--id", "voice-lid-2")
	if err != nil {
		t.Fatalf("transcribe via phone number: %v", err)
	}
	if res := decodeData[transcribeResult](t, raw); !res.Cached || res.Chat != tLID {
		t.Fatalf("result = %+v", res)
	}
}

type fakeTranscriptChatResolver struct{ pn, lid types.JID }

func (f fakeTranscriptChatResolver) ResolveLIDToPN(_ context.Context, jid types.JID) types.JID {
	if jid.User == f.lid.User && jid.Server == f.lid.Server {
		return f.pn
	}
	return jid
}

func (f fakeTranscriptChatResolver) ResolvePNToLID(_ context.Context, jid types.JID) types.JID {
	if jid.User == f.pn.User && jid.Server == f.pn.Server {
		return f.lid
	}
	return jid
}

func TestTranscriptChatsSameIdentity(t *testing.T) {
	opened := 0
	chats := &transcriptChats{ctx: context.Background(), open: func() chatIdentityResolver {
		opened++
		return fakeTranscriptChatResolver{pn: mustParseJID(t, tChat), lid: mustParseJID(t, tLID)}
	}}
	if !chats.same(tChat, tChat) || opened != 0 {
		t.Fatalf("identical JIDs needed the resolver (opened %d)", opened)
	}
	if !chats.same("15550000001:3@s.whatsapp.net", tChat) {
		t.Fatal("device suffix should not matter")
	}
	if !chats.same(tLID, tChat) || !chats.same(tChat, tLID) {
		t.Fatal("LID and phone number of one chat should match")
	}
	if chats.same(tOther, tChat) || chats.same(tOther, tLID) {
		t.Fatal("different chats matched")
	}
	if got := chats.canonical(tLID); got != tChat {
		t.Fatalf("canonical(LID) = %q", got)
	}
	if opened != 1 {
		t.Fatalf("resolver opened %d times, want once", opened)
	}

	noSession := &transcriptChats{ctx: context.Background(), open: func() chatIdentityResolver { return nil }}
	if noSession.same(tLID, tChat) || noSession.canonical(tLID) != tLID {
		t.Fatal("without a session the LID cannot be mapped")
	}
}

func TestVoiceMessageTextKeepsReplyQuote(t *testing.T) {
	text := "made-up voice words"
	empty := ""
	for _, tc := range []struct {
		msg  store.Message
		want string
	}{
		{msg: store.Message{MediaType: "audio", DisplayText: "Sent audio", Transcript: &text}, want: "[Voice] made-up voice words"},
		{msg: store.Message{MediaType: "audio", DisplayText: "> earlier message\nSent audio", Transcript: &text}, want: "> earlier message\n[Voice] made-up voice words"},
		{msg: store.Message{MediaType: "audio", Text: "[Audio]", Transcript: &empty}, want: "[Voice] (no speech detected)"},
		{msg: store.Message{MediaType: "audio", Revoked: true, Transcript: &text}, want: store.DeletedMessageDisplayText},
		{msg: store.Message{MediaType: "audio", DisplayText: "Sent audio"}, want: "Sent audio"},
	} {
		if got := messageText(tc.msg); got != tc.want {
			t.Fatalf("messageText(%+v) = %q, want %q", tc.msg, got, tc.want)
		}
	}
}

func TestTranscriptSnippet(t *testing.T) {
	long := strings.Repeat("filler ", 20) + "the PICNIC plan " + strings.Repeat("more ", 20)
	got := transcriptSnippet(long, "picnic")
	want := "[Voice] …filler filler filler filler filler the [PICNIC] plan more more more more more…"
	if got != want {
		t.Fatalf("snippet = %q, want %q", got, want)
	}
	if got := transcriptSnippet("bring the picnic blanket", "nic"); got != "[Voice] bring the pic[nic] blanket" {
		t.Fatalf("mid-word snippet = %q", got)
	}
	if got := transcriptSnippet("short made-up line", "absent"); got != "[Voice] short made-up line" {
		t.Fatalf("no-match snippet = %q", got)
	}
}
