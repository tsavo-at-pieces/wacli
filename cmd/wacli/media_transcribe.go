package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/transcribe"
	"github.com/openclaw/wacli/internal/transcripts"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
)

// transcribeDownloadTimeout bounds one direct CDN download of a voice note.
const transcribeDownloadTimeout = 2 * time.Minute

var errNoAudioSource = errors.New("no local audio file and no download metadata (run `wacli sync` or `wacli media backfill` first)")

// transcribeDeps are the side effects `media transcribe` performs, so tests
// can run the command without WhatsApp, ffmpeg, or real audio.
type transcribeDeps struct {
	env      transcribe.Environment
	download func(ctx context.Context, info store.MediaDownloadInfo, target string) error
	convert  func(ctx context.Context, ffmpeg, in, out string) error
	now      func() time.Time
	// tempRoot is where per-message private temp dirs are made ("" = the
	// system temp dir).
	tempRoot string
}

func defaultTranscribeDeps() transcribeDeps {
	return transcribeDeps{
		env: transcribe.OSEnvironment(),
		// The same lock-free path as `wacli --read-only media download
		// --output`: a direct CDN fetch that needs no session store.
		download: func(ctx context.Context, info store.MediaDownloadInfo, target string) error {
			_, err := wa.DownloadMediaDirectToFile(ctx, info.DirectPath, info.FileEncSHA256, info.FileSHA256, info.MediaKey, info.FileLength, info.MediaType, target)
			return err
		},
		convert: transcribe.ConvertToWAV,
		now:     time.Now,
	}
}

func newMediaTranscribeCmd(flags *rootFlags) *cobra.Command {
	return newMediaTranscribeCmdWithDeps(flags, defaultTranscribeDeps())
}

type transcribeOptions struct {
	chat      string
	id        string
	force     bool
	engine    string
	pending   bool
	afterStr  string
	beforeStr string
	limit     int
}

func newMediaTranscribeCmdWithDeps(flags *rootFlags, deps transcribeDeps) *cobra.Command {
	var opts transcribeOptions

	cmd := &cobra.Command{
		Use:   "transcribe",
		Short: "Transcribe stored voice notes and other audio to text with a local engine",
		Long: "Convert a stored audio message with ffmpeg and run a local speech-to-text engine\n" +
			"on it. Transcripts are kept in transcripts.db next to wacli.db and shown by\n" +
			"`messages list/show/context/search/export`.\n\n" +
			"It never writes wacli.db or the WhatsApp session and takes no store lock, so it\n" +
			"runs while `sync --follow` is active and is allowed in --read-only mode.\n\n" +
			"Engines: " + strings.Join(transcribe.EngineNames(), ", ") + " (default " + transcribe.DefaultEngine + ";\n" +
			"select with --engine or " + transcribe.EnvEngine + ").",
		Example: "  wacli media transcribe --chat 15550000001@s.whatsapp.net --id ABC123\n" +
			"  wacli media transcribe --pending --limit 20 --json",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateTranscribeOptions(cmd, opts); err != nil {
				return err
			}
			ctx, cancel := mediaBulkContext(cmd, flags)
			defer cancel()

			a, err := openTranscribeStore(flags)
			if err != nil {
				return err
			}
			defer a.Close()

			if opts.pending {
				return runTranscribePending(ctx, a, flags, deps, opts)
			}
			return runTranscribeOne(ctx, a, flags, deps, opts)
		},
	}

	cmd.Flags().StringVar(&opts.chat, "chat", "", "chat JID (with --pending: limit to one chat)")
	cmd.Flags().StringVar(&opts.id, "id", "", "message ID of the audio message")
	cmd.Flags().BoolVar(&opts.force, "force", false, "transcribe again even if a transcript is stored")
	cmd.Flags().StringVar(&opts.engine, "engine", "", "engine: "+strings.Join(transcribe.EngineNames(), ", ")+" (default $"+transcribe.EnvEngine+" or "+transcribe.DefaultEngine+")")
	cmd.Flags().BoolVar(&opts.pending, "pending", false, "transcribe every stored audio message without a transcript, newest first")
	cmd.Flags().StringVar(&opts.afterStr, "after", "", "with --pending: only messages after time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&opts.beforeStr, "before", "", "with --pending: only messages before time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "with --pending: maximum messages to transcribe (0 = all)")
	return cmd
}

func validateTranscribeOptions(cmd *cobra.Command, opts transcribeOptions) error {
	if opts.pending {
		if strings.TrimSpace(opts.id) != "" {
			return fmt.Errorf("--id cannot be combined with --pending")
		}
		if opts.force {
			return fmt.Errorf("--force cannot be combined with --pending (pending messages have no transcript yet)")
		}
		if opts.limit < 0 {
			return fmt.Errorf("--limit must be >= 0")
		}
		return nil
	}
	if strings.TrimSpace(opts.chat) == "" || strings.TrimSpace(opts.id) == "" {
		return fmt.Errorf("--chat and --id are required (or use --pending)")
	}
	for _, name := range []string{"after", "before", "limit"} {
		if flag := cmd.Flags().Lookup(name); flag != nil && flag.Changed {
			return fmt.Errorf("--%s only applies with --pending", name)
		}
	}
	return nil
}

// openTranscribeStore opens wacli.db read-only whatever the --read-only
// setting: transcription must not take the store lock (a running
// `sync --follow` holds it) and must never write the message store.
func openTranscribeStore(flags *rootFlags) (*app.App, error) {
	storeDir, err := resolveStoreDir(flags)
	if err != nil {
		return nil, err
	}
	a, err := app.New(app.Options{
		StoreDir: storeDir,
		Version:  effectiveVersion(),
		JSON:     flags.asJSON,
		Events:   out.NewEventWriter(os.Stderr, flags.events),
		ReadOnly: true,
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("no message store at %s (run `wacli sync` first)", storeDir)
		}
		return nil, err
	}
	return a, nil
}

// transcriber runs one resolved engine over stored audio messages.
type transcriber struct {
	deps   transcribeDeps
	engine transcribe.Engine
	ffmpeg string
}

func newTranscriber(deps transcribeDeps, engineFlag string) (*transcriber, error) {
	engine, err := transcribe.ResolveEngine(engineFlag, deps.env)
	if err != nil {
		return nil, err
	}
	ffmpeg, err := transcribe.ResolveFFmpeg(deps.env)
	if err != nil {
		return nil, err
	}
	return &transcriber{deps: deps, engine: engine, ffmpeg: ffmpeg}, nil
}

type transcribeOutcome struct {
	text       string
	durationMS int64
	source     string
	elapsed    time.Duration
}

// run transcribes one message. Downloads and the converted WAV live in a
// private temp directory that is always removed.
func (t *transcriber) run(ctx context.Context, db *store.DB, chatJID, msgID string) (transcribeOutcome, error) {
	started := t.deps.now()
	tmpDir, err := os.MkdirTemp(t.deps.tempRoot, "wacli-transcribe-")
	if err != nil {
		return transcribeOutcome{}, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		return transcribeOutcome{}, fmt.Errorf("secure temp dir: %w", err)
	}

	audioPath, source, err := t.audioSource(ctx, db, chatJID, msgID, tmpDir)
	if err != nil {
		return transcribeOutcome{}, err
	}
	wavPath := filepath.Join(tmpDir, "audio.wav")
	if err := t.deps.convert(ctx, t.ffmpeg, audioPath, wavPath); err != nil {
		return transcribeOutcome{}, err
	}
	var durationMS int64
	if d, err := transcribe.WAVDuration(wavPath); err == nil {
		durationMS = d.Milliseconds()
	}
	text, err := t.engine.Run(ctx, wavPath)
	if err != nil {
		return transcribeOutcome{}, err
	}
	return transcribeOutcome{text: text, durationMS: durationMS, source: source, elapsed: t.deps.now().Sub(started)}, nil
}

// audioSource prefers a file already downloaded by sync or media backfill and
// otherwise fetches the media straight from the CDN into tmpDir.
func (t *transcriber) audioSource(ctx context.Context, db *store.DB, chatJID, msgID, tmpDir string) (string, string, error) {
	if path, ok := localAudioFile(db, chatJID, msgID); ok {
		return path, "local", nil
	}
	info, err := db.GetMediaDownloadInfo(chatJID, msgID)
	if err != nil {
		return "", "", fmt.Errorf("read media metadata: %w", err)
	}
	if strings.TrimSpace(info.DirectPath) == "" || len(info.MediaKey) == 0 {
		return "", "", errNoAudioSource
	}
	target := filepath.Join(tmpDir, "input"+audioExtension(info.MimeType))
	dctx, cancel := context.WithTimeout(ctx, transcribeDownloadTimeout)
	defer cancel()
	if err := t.deps.download(dctx, info, target); err != nil {
		return "", "", fmt.Errorf("download audio: %w", err)
	}
	return target, "download", nil
}

func localAudioFile(db *store.DB, chatJID, msgID string) (string, bool) {
	paths, err := db.MessageLocalMediaPaths(chatJID, msgID)
	if err != nil {
		return "", false
	}
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			return path, true
		}
	}
	return "", false
}

// audioExtension only hints ffmpeg; it probes the content either way.
func audioExtension(mimeType string) string {
	mt := strings.ToLower(mimeType)
	switch {
	case strings.Contains(mt, "ogg"), strings.Contains(mt, "opus"):
		return ".ogg"
	case strings.Contains(mt, "mp4"), strings.Contains(mt, "aac"), strings.Contains(mt, "m4a"):
		return ".m4a"
	case strings.Contains(mt, "mpeg"), strings.Contains(mt, "mp3"):
		return ".mp3"
	case strings.Contains(mt, "amr"):
		return ".amr"
	case strings.Contains(mt, "wav"):
		return ".wav"
	default:
		return ".audio"
	}
}

// transcriptSidecar opens transcripts.db for writing on first use, so a run
// that stores nothing never creates it.
type transcriptSidecar struct {
	path string
	db   *transcripts.DB
}

func (s *transcriptSidecar) writer() (*transcripts.DB, error) {
	if s.db == nil {
		db, err := transcripts.Open(s.path)
		if err != nil {
			return nil, err
		}
		s.db = db
	}
	return s.db, nil
}

func (s *transcriptSidecar) close() {
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
}

type transcribeResult struct {
	Chat       string `json:"chat"`
	ID         string `json:"id"`
	Transcript string `json:"transcript"`
	Empty      bool   `json:"empty"`
	Engine     string `json:"engine"`
	Model      string `json:"model,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	CreatedAt  string `json:"created_at"`
	Cached     bool   `json:"cached"`
	Source     string `json:"source,omitempty"`
	ElapsedMS  int64  `json:"elapsed_ms,omitempty"`
}

func runTranscribeOne(ctx context.Context, a *app.App, flags *rootFlags, deps transcribeDeps, opts transcribeOptions) error {
	chatJIDs, err := messageChatJIDFilter(ctx, a, opts.chat)
	if err != nil {
		return err
	}
	m, err := getMessageByChatFilter(a.DB(), chatJIDs, strings.TrimSpace(opts.id))
	if err != nil {
		if isNoRows(err) {
			return fmt.Errorf("message %s not found in chat %s", strings.TrimSpace(opts.id), strings.TrimSpace(opts.chat))
		}
		return err
	}
	if !isAudioMessage(m) {
		if strings.TrimSpace(m.MediaType) == "" {
			return fmt.Errorf("message %s is not an audio message (it has no media); only voice notes and other audio can be transcribed", m.MsgID)
		}
		return fmt.Errorf("message %s is not an audio message (media type %q); only voice notes and other audio can be transcribed", m.MsgID, m.MediaType)
	}

	chats := newTranscriptChats(ctx, a)
	if !opts.force {
		if cached, ok := cachedTranscript(a.StoreDir(), chats, m); ok {
			return writeTranscribeResult(flags, transcribeResultFrom(m, cached, true, transcribeOutcome{}))
		}
	}

	tr, err := newTranscriber(deps, opts.engine)
	if err != nil {
		return err
	}
	outcome, err := tr.run(ctx, a.DB(), m.ChatJID, m.MsgID)
	if err != nil {
		return err
	}
	sidecar := &transcriptSidecar{path: transcripts.PathFor(a.StoreDir())}
	defer sidecar.close()
	stored, err := storeTranscript(sidecar, chats, m.ChatJID, m.MsgID, tr.engine, outcome, deps.now())
	if err != nil {
		return err
	}
	return writeTranscribeResult(flags, transcribeResultFrom(m, stored, false, outcome))
}

func cachedTranscript(storeDir string, chats *transcriptChats, m store.Message) (transcripts.Transcript, bool) {
	tdb := openTranscriptsForRead(storeDir)
	if tdb == nil {
		return transcripts.Transcript{}, false
	}
	defer tdb.Close()
	rows, err := tdb.ByMessageIDs([]string{m.MsgID})
	if err != nil {
		warnTranscripts(err)
		return transcripts.Transcript{}, false
	}
	return matchTranscript(chats, m.ChatJID, rows)
}

func storeTranscript(sidecar *transcriptSidecar, chats *transcriptChats, chatJID, msgID string, engine transcribe.Engine, outcome transcribeOutcome, now time.Time) (transcripts.Transcript, error) {
	tdb, err := sidecar.writer()
	if err != nil {
		return transcripts.Transcript{}, err
	}
	t := transcripts.Transcript{
		ChatJID:    chats.canonical(chatJID),
		MsgID:      msgID,
		Text:       outcome.text,
		Engine:     engine.Name,
		Model:      engine.Model,
		DurationMS: outcome.durationMS,
		CreatedAt:  now.UTC().Truncate(time.Second),
	}
	if err := tdb.Upsert(t); err != nil {
		return transcripts.Transcript{}, fmt.Errorf("store transcript: %w", err)
	}
	return t, nil
}

func transcribeResultFrom(m store.Message, t transcripts.Transcript, cached bool, outcome transcribeOutcome) transcribeResult {
	return transcribeResult{
		Chat:       m.ChatJID,
		ID:         m.MsgID,
		Transcript: t.Text,
		Empty:      strings.TrimSpace(t.Text) == "",
		Engine:     t.Engine,
		Model:      t.Model,
		DurationMS: t.DurationMS,
		CreatedAt:  t.CreatedAt.UTC().Format(time.RFC3339),
		Cached:     cached,
		Source:     outcome.source,
		ElapsedMS:  outcome.elapsed.Milliseconds(),
	}
}

func writeTranscribeResult(flags *rootFlags, res transcribeResult) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, res)
	}
	if res.Cached {
		fmt.Fprintf(os.Stderr, "Stored transcript from %s (use --force to transcribe again).\n", sanitize(res.Engine))
	}
	if res.Empty {
		fmt.Fprintln(os.Stderr, "No speech detected.")
		return nil
	}
	fmt.Fprintln(os.Stdout, sanitizeBody(res.Transcript))
	return nil
}

type transcribeItemResult struct {
	Chat       string `json:"chat"`
	ID         string `json:"id"`
	Status     string `json:"status"`
	Detail     string `json:"detail,omitempty"`
	Source     string `json:"source,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	ElapsedMS  int64  `json:"elapsed_ms,omitempty"`
}

type transcribePendingReport struct {
	Pending     int                    `json:"pending"`
	Attempted   int                    `json:"attempted"`
	Transcribed int                    `json:"transcribed"`
	Empty       int                    `json:"empty"`
	Skipped     int                    `json:"skipped"`
	Failed      int                    `json:"failed"`
	Engine      string                 `json:"engine,omitempty"`
	Model       string                 `json:"model,omitempty"`
	Results     []transcribeItemResult `json:"results"`
}

const (
	transcribeStatusTranscribed = "transcribed"
	transcribeStatusEmpty       = "empty"
	transcribeStatusSkipped     = "skipped"
	transcribeStatusFailed      = "failed"
)

type pendingAudio struct {
	msg        store.AudioMessage
	failed     bool
	lastFailed time.Time
}

func runTranscribePending(ctx context.Context, a *app.App, flags *rootFlags, deps transcribeDeps, opts transcribeOptions) error {
	chatJIDs, err := messageChatJIDFilter(ctx, a, opts.chat)
	if err != nil {
		return err
	}
	after, before, err := messageTimeBounds(opts.afterStr, opts.beforeStr)
	if err != nil {
		return err
	}
	audio, err := a.DB().ListAudioMessages(store.ListAudioMessagesParams{ChatJIDs: chatJIDs, After: after, Before: before})
	if err != nil {
		return err
	}

	chats := newTranscriptChats(ctx, a)
	sidecar := &transcriptSidecar{path: transcripts.PathFor(a.StoreDir())}
	defer sidecar.close()
	done, failures, err := loadTranscribeState(sidecar)
	if err != nil {
		return err
	}

	report := transcribePendingReport{Results: []transcribeItemResult{}}
	var work []pendingAudio
	for _, m := range audio {
		if hasTranscriptFor(chats, done, m.ChatJID, m.MsgID) {
			continue
		}
		report.Pending++
		if _, ok := localAudioFile(a.DB(), m.ChatJID, m.MsgID); !ok && !m.Downloadable {
			report.Skipped++
			report.Results = append(report.Results, transcribeItemResult{Chat: m.ChatJID, ID: m.MsgID, Status: transcribeStatusSkipped, Detail: "no local audio file and no downloadable media"})
			continue
		}
		item := pendingAudio{msg: m}
		if f, ok := failures[transcripts.Key{ChatJID: chats.canonical(m.ChatJID), MsgID: m.MsgID}]; ok {
			item.failed, item.lastFailed = true, f.LastAttemptAt
		}
		work = append(work, item)
	}
	// Never-attempted messages go first (newest first), then earlier failures,
	// least recently tried first, so a --limit window cannot get stuck on
	// media that keeps failing.
	sort.SliceStable(work, func(i, j int) bool {
		if work[i].failed != work[j].failed {
			return !work[i].failed
		}
		if work[i].failed {
			return work[i].lastFailed.Before(work[j].lastFailed)
		}
		return false
	})
	if opts.limit > 0 && len(work) > opts.limit {
		work = work[:opts.limit]
	}

	if len(work) > 0 {
		tr, err := newTranscriber(deps, opts.engine)
		if err != nil {
			return err
		}
		report.Engine, report.Model = tr.engine.Name, tr.engine.Model
		for _, item := range work {
			if ctx.Err() != nil {
				break
			}
			report.Attempted++
			report.Results = append(report.Results, transcribePendingItem(ctx, a, tr, sidecar, chats, item.msg, &report))
		}
	}

	if err := writeTranscribePendingReport(flags, report); err != nil {
		return err
	}
	return ctx.Err()
}

func loadTranscribeState(sidecar *transcriptSidecar) (map[string][]string, map[transcripts.Key]transcripts.Failure, error) {
	done := map[string][]string{}
	failures := map[transcripts.Key]transcripts.Failure{}
	if _, err := os.Stat(sidecar.path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return done, failures, nil
		}
		return nil, nil, err
	}
	tdb, err := sidecar.writer()
	if err != nil {
		return nil, nil, err
	}
	keys, err := tdb.Keys()
	if err != nil {
		return nil, nil, fmt.Errorf("read transcripts: %w", err)
	}
	for _, k := range keys {
		done[k.MsgID] = append(done[k.MsgID], k.ChatJID)
	}
	failures, err = tdb.Failures()
	if err != nil {
		return nil, nil, fmt.Errorf("read transcript failures: %w", err)
	}
	return done, failures, nil
}

func hasTranscriptFor(chats *transcriptChats, done map[string][]string, chatJID, msgID string) bool {
	for _, stored := range done[msgID] {
		if chats.same(chatJID, stored) {
			return true
		}
	}
	return false
}

func transcribePendingItem(ctx context.Context, a *app.App, tr *transcriber, sidecar *transcriptSidecar, chats *transcriptChats, m store.AudioMessage, report *transcribePendingReport) transcribeItemResult {
	res := transcribeItemResult{Chat: m.ChatJID, ID: m.MsgID}
	outcome, err := tr.run(ctx, a.DB(), m.ChatJID, m.MsgID)
	if err == nil {
		_, err = storeTranscript(sidecar, chats, m.ChatJID, m.MsgID, tr.engine, outcome, tr.deps.now())
	}
	if err != nil {
		report.Failed++
		res.Status = transcribeStatusFailed
		res.Detail = err.Error()
		if ctx.Err() == nil {
			if tdb, openErr := sidecar.writer(); openErr == nil {
				_ = tdb.RecordFailure(chats.canonical(m.ChatJID), m.MsgID, err.Error(), tr.deps.now())
			}
		}
		return res
	}
	report.Transcribed++
	res.Status = transcribeStatusTranscribed
	if strings.TrimSpace(outcome.text) == "" {
		report.Empty++
		res.Status = transcribeStatusEmpty
	}
	res.Source = outcome.source
	res.DurationMS = outcome.durationMS
	res.ElapsedMS = outcome.elapsed.Milliseconds()
	return res
}

func writeTranscribePendingReport(flags *rootFlags, report transcribePendingReport) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, report)
	}
	fmt.Fprintf(os.Stdout, "Pending: %d  Attempted: %d  Transcribed: %d  Empty: %d  Skipped: %d  Failed: %d\n",
		report.Pending, report.Attempted, report.Transcribed, report.Empty, report.Skipped, report.Failed)
	for _, r := range report.Results {
		line := fmt.Sprintf("  %-11s %s/%s", r.Status, r.Chat, r.ID)
		if r.Detail != "" {
			line += "  " + r.Detail
		}
		fmt.Fprintln(os.Stdout, sanitize(line))
	}
	return nil
}
