package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/transcribe"
	"github.com/openclaw/wacli/internal/transcripts"
	"github.com/spf13/cobra"
)

// All identities and text below are fictional.
const (
	tChat  = "15550000001@s.whatsapp.net"
	tOther = "15550000002@s.whatsapp.net"
	tLID   = "100000000001@lid"
)

// TestTranscribeHelperProcess is the fake engine the command tests run. It
// prints $WACLI_TEST_TRANSCRIPT, or fails when that is "FAIL".
func TestTranscribeHelperProcess(t *testing.T) {
	if os.Getenv("WACLI_TRANSCRIBE_CMD_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 2 {
		fmt.Fprintf(os.Stderr, "want exactly one wav argument, got %q", args)
		os.Exit(2)
	}
	if info, err := os.Stat(args[1]); err != nil || info.Size() == 0 {
		fmt.Fprintf(os.Stderr, "wav missing: %v", err)
		os.Exit(2)
	}
	text := os.Getenv("WACLI_TEST_TRANSCRIPT")
	if text == "FAIL" {
		fmt.Fprint(os.Stderr, "fake engine failure")
		os.Exit(4)
	}
	fmt.Fprintln(os.Stdout, text)
	os.Exit(0)
}

type transcribeHarness struct {
	storeDir  string
	tempRoot  string
	env       map[string]string
	downloads []string
	converts  int
}

func (h *transcribeHarness) deps(t *testing.T) transcribeDeps {
	t.Helper()
	t.Setenv("WACLI_TRANSCRIBE_CMD_HELPER", "1")
	return transcribeDeps{
		env: transcribe.Environment{
			Getenv: func(name string) string { return h.env[name] },
			LookPath: func(name string) (string, error) {
				if name == "ffmpeg" {
					return "/fake/ffmpeg", nil
				}
				return "", exec.ErrNotFound
			},
			HomeDir: func() (string, error) { return h.storeDir, nil },
		},
		download: func(ctx context.Context, info store.MediaDownloadInfo, target string) error {
			h.downloads = append(h.downloads, info.MsgID)
			if strings.Contains(info.DirectPath, "broken") {
				return errors.New("fake CDN returned 403")
			}
			return os.WriteFile(target, []byte("fake-ogg-bytes"), 0o600)
		},
		convert: func(ctx context.Context, ffmpeg, in, out string) error {
			h.converts++
			if ffmpeg != "/fake/ffmpeg" {
				return fmt.Errorf("unexpected ffmpeg %q", ffmpeg)
			}
			if info, err := os.Stat(filepath.Dir(out)); err != nil || info.Mode().Perm() != 0o700 {
				return fmt.Errorf("work dir is not private: %v, %v", info, err)
			}
			if data, err := os.ReadFile(in); err != nil || len(data) == 0 {
				return fmt.Errorf("audio input unreadable: %v", err)
			}
			return writeSilentWAV(out, 20000) // 1.25 s at 16 kHz
		},
		now:      time.Now,
		tempRoot: h.tempRoot,
	}
}

func writeSilentWAV(path string, samples int) error {
	le := binary.LittleEndian
	data := make([]byte, samples*2)
	var buf []byte
	buf = append(buf, "RIFF"...)
	buf = le.AppendUint32(buf, uint32(36+len(data)))
	buf = append(buf, "WAVEfmt "...)
	buf = le.AppendUint32(buf, 16)
	buf = le.AppendUint16(buf, 1)
	buf = le.AppendUint16(buf, 1)
	buf = le.AppendUint32(buf, 16000)
	buf = le.AppendUint32(buf, 32000)
	buf = le.AppendUint16(buf, 2)
	buf = le.AppendUint16(buf, 16)
	buf = append(buf, "data"...)
	buf = le.AppendUint32(buf, uint32(len(data)))
	buf = append(buf, data...)
	return os.WriteFile(path, buf, 0o600)
}

var transcribeFixtureBase = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)

// newTranscribeHarness seeds a store with audio in every state the command
// handles, plus non-audio messages.
func newTranscribeHarness(t *testing.T) *transcribeHarness {
	t.Helper()
	storeDir := t.TempDir()
	db, err := store.Open(filepath.Join(storeDir, "wacli.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	for _, jid := range []string{tChat, tOther} {
		if err := db.UpsertChat(jid, "dm", "Synthetic Contact", transcribeFixtureBase); err != nil {
			t.Fatalf("UpsertChat: %v", err)
		}
	}
	audio := func(id string, minutes int, directPath string) store.UpsertMessageParams {
		p := store.UpsertMessageParams{
			ChatJID: tChat, MsgID: id, SenderJID: tChat, SenderName: "Synthetic Contact",
			Timestamp: transcribeFixtureBase.Add(time.Duration(minutes) * time.Minute),
			Text:      "[Audio]", DisplayText: "Sent audio", MediaType: "audio", MimeType: "audio/ogg; codecs=opus",
		}
		if directPath != "" {
			p.DirectPath, p.MediaKey = directPath, []byte{1, 2, 3}
		}
		return p
	}
	rows := []store.UpsertMessageParams{
		audio("voice-local", 1, ""),
		audio("voice-download", 2, "/v/fake-ok"),
		audio("voice-broken", 3, "/v/broken"),
		audio("voice-nosource", 4, ""),
		{ChatJID: tChat, MsgID: "text-1", SenderJID: tChat, SenderName: "Synthetic Contact", Timestamp: transcribeFixtureBase.Add(5 * time.Minute), Text: "made-up note about lunch", DisplayText: "made-up note about lunch"},
		{ChatJID: tChat, MsgID: "photo-1", SenderJID: tChat, SenderName: "Synthetic Contact", Timestamp: transcribeFixtureBase.Add(6 * time.Minute), Text: "", DisplayText: "Sent image", MediaType: "image"},
	}
	for _, row := range rows {
		if err := db.UpsertMessage(row); err != nil {
			t.Fatalf("UpsertMessage %s: %v", row.MsgID, err)
		}
	}
	local := filepath.Join(storeDir, "media", "voice-local.ogg")
	if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("fake-local-ogg"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkMediaDownloaded(tChat, "voice-local", local, transcribeFixtureBase); err != nil {
		t.Fatalf("MarkMediaDownloaded: %v", err)
	}
	return &transcribeHarness{
		storeDir: storeDir,
		tempRoot: t.TempDir(),
		env: map[string]string{
			transcribe.EnvEngine:  transcribe.EngineCommand,
			transcribe.EnvCommand: `'` + os.Args[0] + `' -test.run=^TestTranscribeHelperProcess$ -- {wav}`,
			transcribe.EnvModel:   "fake-model",
		},
	}
}

func runCommandJSON(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var err error
	out := captureRootStdout(t, func() { err = cmd.Execute() })
	return out, err
}

func (h *transcribeHarness) transcribe(t *testing.T, flags *rootFlags, args ...string) (string, error) {
	t.Helper()
	if flags == nil {
		flags = &rootFlags{storeDir: h.storeDir, asJSON: true}
	}
	return runCommandJSON(t, newMediaTranscribeCmdWithDeps(flags, h.deps(t)), args...)
}

func decodeData[T any](t *testing.T, raw string) T {
	t.Helper()
	var env struct {
		Success bool `json:"success"`
		Data    T    `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	if !env.Success {
		t.Fatalf("success=false: %s", raw)
	}
	return env.Data
}

func storedTranscript(t *testing.T, storeDir, chat, id string) (transcripts.Transcript, bool) {
	t.Helper()
	db, err := transcripts.OpenReadOnly(transcripts.PathFor(storeDir))
	if err != nil {
		if transcripts.IsNotExist(err) {
			return transcripts.Transcript{}, false
		}
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer db.Close()
	rows, err := db.ByMessageIDs([]string{id})
	if err != nil {
		t.Fatalf("ByMessageIDs: %v", err)
	}
	for _, tr := range rows {
		if tr.ChatJID == chat {
			return tr, true
		}
	}
	return transcripts.Transcript{}, false
}

func TestMediaTranscribeSingleCachesAndForces(t *testing.T) {
	h := newTranscribeHarness(t)
	t.Setenv("WACLI_TEST_TRANSCRIPT", "  made-up words about the picnic  ")

	raw, err := h.transcribe(t, nil, "--chat", tChat, "--id", "voice-local")
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	res := decodeData[transcribeResult](t, raw)
	if res.Cached || res.Transcript != "made-up words about the picnic" || res.Engine != "command" || res.Model != "fake-model" || res.Source != "local" || res.DurationMS != 1250 || res.Chat != tChat || res.ID != "voice-local" {
		t.Fatalf("first result = %+v", res)
	}
	if len(h.downloads) != 0 {
		t.Fatalf("downloaded %v although a local file exists", h.downloads)
	}
	info, err := os.Stat(transcripts.PathFor(h.storeDir))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("transcripts.db stat = %v, %v; want mode 0600", info, err)
	}

	// Cached: the engine is not run again, even when it would now fail or
	// is not configured at all.
	t.Setenv("WACLI_TEST_TRANSCRIPT", "FAIL")
	h.env[transcribe.EnvEngine] = "no-such-engine"
	raw, err = h.transcribe(t, nil, "--chat", tChat, "--id", "voice-local")
	if err != nil {
		t.Fatalf("cached transcribe: %v", err)
	}
	res = decodeData[transcribeResult](t, raw)
	if !res.Cached || res.Transcript != "made-up words about the picnic" || res.Engine != "command" {
		t.Fatalf("cached result = %+v", res)
	}
	if h.converts != 1 {
		t.Fatalf("converts = %d, want the cached call to skip conversion", h.converts)
	}

	// --force runs the engine again and replaces the stored transcript;
	// --engine overrides the environment.
	t.Setenv("WACLI_TEST_TRANSCRIPT", "second made-up pass")
	raw, err = h.transcribe(t, nil, "--chat", tChat, "--id", "voice-local", "--force", "--engine", "command")
	h.env[transcribe.EnvEngine] = transcribe.EngineCommand
	if err != nil {
		t.Fatalf("forced transcribe: %v", err)
	}
	res = decodeData[transcribeResult](t, raw)
	if res.Cached || res.Transcript != "second made-up pass" {
		t.Fatalf("forced result = %+v", res)
	}
	if stored, ok := storedTranscript(t, h.storeDir, tChat, "voice-local"); !ok || stored.Text != "second made-up pass" || stored.Model != "fake-model" {
		t.Fatalf("stored = %+v, %v", stored, ok)
	}

	// No local file: the audio is downloaded into the temp dir. Silence is a
	// valid (empty) transcript.
	t.Setenv("WACLI_TEST_TRANSCRIPT", "")
	raw, err = h.transcribe(t, nil, "--chat", "+1 555 000 0001", "--id", "voice-download")
	if err != nil {
		t.Fatalf("transcribe download: %v", err)
	}
	res = decodeData[transcribeResult](t, raw)
	if res.Source != "download" || !res.Empty || res.Transcript != "" {
		t.Fatalf("download result = %+v", res)
	}
	if strings.Join(h.downloads, ",") != "voice-download" {
		t.Fatalf("downloads = %v", h.downloads)
	}
	if stored, ok := storedTranscript(t, h.storeDir, tChat, "voice-download"); !ok || stored.Text != "" {
		t.Fatalf("empty transcript not stored: %+v, %v", stored, ok)
	}
	if leftovers, _ := os.ReadDir(h.tempRoot); len(leftovers) != 0 {
		t.Fatalf("temp dirs were not removed: %v", leftovers)
	}
}

func TestMediaTranscribeHumanOutput(t *testing.T) {
	h := newTranscribeHarness(t)
	t.Setenv("WACLI_TEST_TRANSCRIPT", "made-up words about the picnic")
	raw, err := h.transcribe(t, &rootFlags{storeDir: h.storeDir}, "--chat", tChat, "--id", "voice-local")
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if strings.TrimSpace(raw) != "made-up words about the picnic" {
		t.Fatalf("stdout = %q", raw)
	}
}

func TestMediaTranscribeRejectsNonAudioAndBadFlags(t *testing.T) {
	h := newTranscribeHarness(t)
	t.Setenv("WACLI_TEST_TRANSCRIPT", "unused")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{"--chat", tChat, "--id", "text-1"}, want: "not an audio message (it has no media)"},
		{args: []string{"--chat", tChat, "--id", "photo-1"}, want: `not an audio message (media type "image")`},
		{args: []string{"--chat", tChat, "--id", "no-such-id"}, want: "not found"},
		{args: []string{"--chat", tChat, "--id", "voice-nosource"}, want: "no local audio file and no download metadata"},
		{args: []string{"--chat", tChat}, want: "--chat and --id are required"},
		{args: []string{"--pending", "--id", "voice-local"}, want: "--id cannot be combined with --pending"},
		{args: []string{"--pending", "--force"}, want: "--force cannot be combined"},
		{args: []string{"--pending", "--limit", "-1"}, want: "--limit must be >= 0"},
		{args: []string{"--chat", tChat, "--id", "voice-local", "--limit", "3"}, want: "--limit only applies with --pending"},
		{args: []string{"--chat", tChat, "--id", "voice-local", "--engine", "siri"}, want: "unknown transcription engine"},
	} {
		_, err := h.transcribe(t, nil, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("args %v: error = %v, want %q", tc.args, err, tc.want)
		}
	}
	if _, err := os.Stat(transcripts.PathFor(h.storeDir)); !os.IsNotExist(err) {
		t.Fatalf("rejected requests created transcripts.db (stat err %v)", err)
	}
	if h.converts != 0 {
		t.Fatalf("converts = %d", h.converts)
	}
}

func TestMediaTranscribeMissingEngineBinaryIsActionable(t *testing.T) {
	h := newTranscribeHarness(t)
	h.env[transcribe.EnvEngine] = transcribe.EngineFluidAudio
	_, err := h.transcribe(t, nil, "--chat", tChat, "--id", "voice-local")
	if err == nil || !strings.Contains(err.Error(), transcribe.EnvFluidAudio) {
		t.Fatalf("error = %v, want it to name %s", err, transcribe.EnvFluidAudio)
	}
}

func resultStatuses(results []transcribeItemResult) string {
	parts := make([]string, 0, len(results))
	for _, r := range results {
		parts = append(parts, r.ID+"="+r.Status)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func TestMediaTranscribePendingCountsAndContinuesPastFailures(t *testing.T) {
	h := newTranscribeHarness(t)
	t.Setenv("WACLI_TEST_TRANSCRIPT", "made-up pending words")

	raw, err := h.transcribe(t, nil, "--pending")
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	rep := decodeData[transcribePendingReport](t, raw)
	if rep.Pending != 4 || rep.Attempted != 3 || rep.Transcribed != 2 || rep.Failed != 1 || rep.Skipped != 1 || rep.Empty != 0 || rep.Engine != "command" {
		t.Fatalf("report = %+v", rep)
	}
	if got := resultStatuses(rep.Results); got != "voice-broken=failed,voice-download=transcribed,voice-local=transcribed,voice-nosource=skipped" {
		t.Fatalf("statuses = %s", got)
	}
	for _, r := range rep.Results {
		if r.ID == "voice-broken" && !strings.Contains(r.Detail, "fake CDN returned 403") {
			t.Fatalf("failure detail = %q", r.Detail)
		}
	}
	// Newest first among never-attempted messages.
	if strings.Join(h.downloads, ",") != "voice-broken,voice-download" {
		t.Fatalf("download order = %v", h.downloads)
	}

	// A second run only sees what is still missing.
	raw, err = h.transcribe(t, nil, "--pending")
	if err != nil {
		t.Fatalf("second pending: %v", err)
	}
	rep = decodeData[transcribePendingReport](t, raw)
	if rep.Pending != 2 || rep.Attempted != 1 || rep.Failed != 1 || rep.Skipped != 1 || rep.Transcribed != 0 {
		t.Fatalf("second report = %+v", rep)
	}

	// A never-attempted message outranks earlier failures under --limit, even
	// though it is older.
	db, err := store.Open(filepath.Join(h.storeDir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: tOther, MsgID: "voice-fresh", SenderJID: tOther, Timestamp: transcribeFixtureBase.Add(-time.Hour), Text: "[Audio]", MediaType: "audio", DirectPath: "/v/fake-fresh", MediaKey: []byte{9}}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	raw, err = h.transcribe(t, nil, "--pending", "--limit", "1")
	if err != nil {
		t.Fatalf("limited pending: %v", err)
	}
	rep = decodeData[transcribePendingReport](t, raw)
	if rep.Pending != 3 || rep.Attempted != 1 || rep.Transcribed != 1 || !strings.Contains(resultStatuses(rep.Results), "voice-fresh=transcribed") {
		t.Fatalf("limited report = %+v", rep)
	}

	// Filters scope the pending set.
	raw, err = h.transcribe(t, nil, "--pending", "--chat", tOther)
	if err != nil {
		t.Fatalf("chat-scoped pending: %v", err)
	}
	if rep = decodeData[transcribePendingReport](t, raw); rep.Pending != 0 || rep.Attempted != 0 {
		t.Fatalf("chat-scoped report = %+v", rep)
	}
	raw, err = h.transcribe(t, nil, "--pending", "--before", "2026-04-01")
	if err != nil {
		t.Fatalf("date-scoped pending: %v", err)
	}
	if rep = decodeData[transcribePendingReport](t, raw); rep.Pending != 0 {
		t.Fatalf("date-scoped report = %+v", rep)
	}
}

func TestMediaTranscribePendingWithNothingToDoNeedsNoEngine(t *testing.T) {
	h := newTranscribeHarness(t)
	h.env[transcribe.EnvEngine] = "no-such-engine"
	raw, err := h.transcribe(t, nil, "--pending", "--chat", tOther)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if rep := decodeData[transcribePendingReport](t, raw); rep.Pending != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if _, err := os.Stat(transcripts.PathFor(h.storeDir)); !os.IsNotExist(err) {
		t.Fatalf("idle run created transcripts.db (stat err %v)", err)
	}
}

func storeFileSnapshot(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	snap := map[string][]byte{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "LOCK" || strings.HasPrefix(e.Name(), transcripts.FileName) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		snap[e.Name()] = data
	}
	return snap
}

func assertStoreUnchanged(t *testing.T, dir string, before map[string][]byte) {
	t.Helper()
	after := storeFileSnapshot(t, dir)
	if len(after) != len(before) {
		t.Fatalf("store files changed: before %d, after %d", len(before), len(after))
	}
	for name, data := range before {
		if !bytes.Equal(after[name], data) {
			t.Fatalf("%s changed", name)
		}
	}
}

// `sync --follow` holds the store LOCK for its whole run; transcription must
// work anyway and leave wacli.db (and the absent session.db) untouched.
func TestMediaTranscribeWorksWhileStoreLockIsHeld(t *testing.T) {
	h := newTranscribeHarness(t)
	t.Setenv("WACLI_TEST_TRANSCRIPT", "made-up words while syncing")
	lk, err := lock.Acquire(h.storeDir)
	if err != nil {
		t.Fatalf("lock.Acquire: %v", err)
	}
	defer lk.Release()
	before := storeFileSnapshot(t, h.storeDir)

	if _, err := h.transcribe(t, nil, "--chat", tChat, "--id", "voice-local"); err != nil {
		t.Fatalf("transcribe with lock held: %v", err)
	}
	if _, err := h.transcribe(t, nil, "--pending"); err != nil {
		t.Fatalf("pending with lock held: %v", err)
	}
	assertStoreUnchanged(t, h.storeDir, before)
	if _, err := os.Stat(filepath.Join(h.storeDir, "session.db")); !os.IsNotExist(err) {
		t.Fatalf("session.db was created (stat err %v)", err)
	}
	if locked, _, err := lock.Probe(h.storeDir); err != nil || !locked {
		t.Fatalf("lock no longer held: %v, %v", locked, err)
	}
	if _, ok := storedTranscript(t, h.storeDir, tChat, "voice-download"); !ok {
		t.Fatal("pending run stored nothing")
	}
}

// Transcription changes neither WhatsApp nor wacli.db, so --read-only allows it.
func TestMediaTranscribeAllowedInReadOnlyMode(t *testing.T) {
	h := newTranscribeHarness(t)
	t.Setenv("WACLI_TEST_TRANSCRIPT", "made-up read-only words")
	before := storeFileSnapshot(t, h.storeDir)
	raw, err := h.transcribe(t, &rootFlags{storeDir: h.storeDir, asJSON: true, readOnly: true}, "--chat", tChat, "--id", "voice-local")
	if err != nil {
		t.Fatalf("read-only transcribe: %v", err)
	}
	if res := decodeData[transcribeResult](t, raw); res.Transcript != "made-up read-only words" {
		t.Fatalf("result = %+v", res)
	}
	assertStoreUnchanged(t, h.storeDir, before)
}

func TestMediaTranscribeMissingStore(t *testing.T) {
	h := &transcribeHarness{storeDir: t.TempDir(), tempRoot: t.TempDir(), env: map[string]string{}}
	_, err := h.transcribe(t, nil, "--pending")
	if err == nil || !strings.Contains(err.Error(), "no message store") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(h.storeDir, "wacli.db")); !os.IsNotExist(statErr) {
		t.Fatalf("wacli.db was created")
	}
}
