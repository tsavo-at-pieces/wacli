package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/transcripts"
	"go.mau.fi/whatsmeow/types"
)

// chatIdentityResolver maps between phone-number and LID chat JIDs.
type chatIdentityResolver interface {
	ResolveLIDToPN(context.Context, types.JID) types.JID
	ResolvePNToLID(context.Context, types.JID) types.JID
}

// transcriptChats compares chat JIDs by the canonical identity the store
// uses (LID resolved to phone number, device suffix dropped). Transcripts are
// keyed by that identity, while a message row may still carry the other form
// of the same chat. The read-only session resolver is opened only when two
// JIDs differ textually.
type transcriptChats struct {
	ctx      context.Context
	open     func() chatIdentityResolver
	resolver chatIdentityResolver
	opened   bool
}

func newTranscriptChats(ctx context.Context, a *app.App) *transcriptChats {
	return &transcriptChats{ctx: ctx, open: func() chatIdentityResolver {
		if _, err := os.Stat(filepath.Join(a.StoreDir(), "session.db")); err != nil {
			return nil
		}
		resolver, err := a.ReadOnlyResolver()
		if err != nil {
			return nil
		}
		return resolver
	}}
}

func (c *transcriptChats) get() chatIdentityResolver {
	if !c.opened {
		c.opened = true
		if c.open != nil {
			c.resolver = c.open()
		}
	}
	return c.resolver
}

// canonical returns the phone-number form of chat when the session knows it.
func (c *transcriptChats) canonical(chat string) string {
	chat = strings.TrimSpace(chat)
	jid, err := types.ParseJID(chat)
	if err != nil {
		return chat
	}
	if jid.Server == types.HiddenUserServer {
		if r := c.get(); r != nil {
			jid = r.ResolveLIDToPN(c.ctx, jid)
		}
	}
	return canonicalMessageFilterJID(jid).String()
}

func (c *transcriptChats) lidForm(chat string) string {
	chat = strings.TrimSpace(chat)
	jid, err := types.ParseJID(chat)
	if err != nil {
		return chat
	}
	if jid.Server == types.DefaultUserServer {
		if r := c.get(); r != nil {
			jid = r.ResolvePNToLID(c.ctx, jid.ToNonAD())
		}
	}
	return canonicalMessageFilterJID(jid).String()
}

func (c *transcriptChats) same(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == b {
		return true
	}
	return c.canonical(a) == c.canonical(b) || c.lidForm(a) == c.lidForm(b)
}

func isAudioMessage(m store.Message) bool {
	return strings.EqualFold(strings.TrimSpace(m.MediaType), "audio")
}

func audioMessageIDs(msgs []store.Message) []string {
	var ids []string
	for _, m := range msgs {
		if isAudioMessage(m) {
			ids = append(ids, m.MsgID)
		}
	}
	return ids
}

// openTranscriptsForRead opens transcripts.db read-only. It returns nil when
// the sidecar does not exist (read commands never create it) and warns on
// stderr when it exists but cannot be read, so listing still works.
func openTranscriptsForRead(storeDir string) *transcripts.DB {
	tdb, err := transcripts.OpenReadOnly(transcripts.PathFor(storeDir))
	if err != nil {
		if !transcripts.IsNotExist(err) {
			warnTranscripts(err)
		}
		return nil
	}
	return tdb
}

func warnTranscripts(err error) {
	fmt.Fprintf(os.Stderr, "warning: transcripts unavailable: %s\n", sanitize(err.Error()))
}

// attachTranscripts fills Transcript on audio messages that have one.
func attachTranscripts(ctx context.Context, a *app.App, msgs []store.Message) []store.Message {
	ids := audioMessageIDs(msgs)
	if len(ids) == 0 {
		return msgs
	}
	tdb := openTranscriptsForRead(a.StoreDir())
	if tdb == nil {
		return msgs
	}
	defer tdb.Close()
	return attachTranscriptsFrom(tdb, newTranscriptChats(ctx, a), msgs)
}

func attachTranscriptsFrom(tdb *transcripts.DB, chats *transcriptChats, msgs []store.Message) []store.Message {
	ids := audioMessageIDs(msgs)
	if len(ids) == 0 {
		return msgs
	}
	rows, err := tdb.ByMessageIDs(ids)
	if err != nil {
		warnTranscripts(err)
		return msgs
	}
	byID := groupTranscriptsByID(rows)
	for i := range msgs {
		if !isAudioMessage(msgs[i]) {
			continue
		}
		if t, ok := matchTranscript(chats, msgs[i].ChatJID, byID[msgs[i].MsgID]); ok {
			setMessageTranscript(&msgs[i], t)
		}
	}
	return msgs
}

func groupTranscriptsByID(rows []transcripts.Transcript) map[string][]transcripts.Transcript {
	byID := make(map[string][]transcripts.Transcript, len(rows))
	for _, t := range rows {
		byID[t.MsgID] = append(byID[t.MsgID], t)
	}
	return byID
}

// matchTranscript picks the newest transcript stored for the same chat. More
// than one can exist when a chat's canonical identity changed between runs.
func matchTranscript(chats *transcriptChats, chatJID string, candidates []transcripts.Transcript) (transcripts.Transcript, bool) {
	var best transcripts.Transcript
	found := false
	for _, t := range candidates {
		if !chats.same(chatJID, t.ChatJID) {
			continue
		}
		if !found || t.CreatedAt.After(best.CreatedAt) {
			best, found = t, true
		}
	}
	return best, found
}

func setMessageTranscript(m *store.Message, t transcripts.Transcript) {
	text := t.Text
	m.Transcript = &text
	m.TranscriptEngine = t.Engine
}

// searchWithTranscripts attaches transcripts to regular search results and
// adds audio messages whose transcript matches the query. Transcript matches
// honor the same filters (--chat, --from, --after/--before, --has-media,
// --type, --forwarded, --starred); a --type other than audio excludes them.
// When any are added, the combined list is ordered newest first (FTS rank and
// transcript matches are not comparable) and cut to --limit.
func searchWithTranscripts(ctx context.Context, a *app.App, p store.SearchMessagesParams, base []store.Message) ([]store.Message, error) {
	tdb := openTranscriptsForRead(a.StoreDir())
	if tdb == nil {
		return base, nil
	}
	defer tdb.Close()
	chats := newTranscriptChats(ctx, a)
	base = attachTranscriptsFrom(tdb, chats, base)

	if msgType := strings.ToLower(strings.TrimSpace(p.Type)); msgType != "" && msgType != "audio" {
		return base, nil
	}
	hits, err := tdb.Search(p.Query)
	if err != nil {
		warnTranscripts(err)
		return base, nil
	}
	if len(hits) == 0 {
		return base, nil
	}
	byID := groupTranscriptsByID(hits)
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	candidates, err := a.DB().SearchAudioMessagesByID(ids, p)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(base))
	for _, m := range base {
		seen[m.ChatJID+"\x00"+m.MsgID] = struct{}{}
	}
	var extra []store.Message
	for _, m := range candidates {
		if _, ok := seen[m.ChatJID+"\x00"+m.MsgID]; ok {
			continue
		}
		t, ok := matchTranscript(chats, m.ChatJID, byID[m.MsgID])
		if !ok {
			continue
		}
		setMessageTranscript(&m, t)
		m.Snippet = transcriptSnippet(t.Text, p.Query)
		extra = append(extra, m)
	}
	if len(extra) == 0 {
		return base, nil
	}
	merged := append(append([]store.Message{}, base...), extra...)
	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].Timestamp.After(merged[j].Timestamp)
	})
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

// snippetWords is how many words of context a transcript snippet keeps on
// each side of the match, close to the FTS snippet's 12-token window.
const snippetWords = 6

// transcriptSnippet mirrors the FTS snippet style: the first matching word in
// brackets with a few words of context either side.
func transcriptSnippet(text, query string) string {
	flat := strings.Join(strings.Fields(text), " ")
	start, end := -1, -1
	for _, word := range strings.Fields(query) {
		word = strings.Trim(word, `"`)
		if word == "" {
			continue
		}
		loc := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(word)).FindStringIndex(flat)
		if loc != nil && (start < 0 || loc[0] < start) {
			start, end = loc[0], loc[1]
		}
	}
	if start < 0 {
		return voicePrefix + keepLeadingWords(flat, 2*snippetWords)
	}
	return voicePrefix + keepTrailingWords(flat[:start], snippetWords) + "[" + flat[start:end] + "]" + keepLeadingWords(flat[end:], snippetWords)
}

// keepTrailingWords keeps the last n words of single-spaced s.
func keepTrailingWords(s string, n int) string {
	spaces := 0
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ' ' {
			if spaces++; spaces > n {
				return "…" + s[i+1:]
			}
		}
	}
	return s
}

// keepLeadingWords keeps the first n words of single-spaced s.
func keepLeadingWords(s string, n int) string {
	spaces := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			if spaces++; spaces > n {
				return s[:i] + "…"
			}
		}
	}
	return s
}

const (
	voicePrefix             = "[Voice] "
	audioDisplayPlaceholder = "Sent audio"
)

// voiceMessageText shows a transcript in place of the "Sent audio"
// placeholder, keeping a reply's quoted prefix.
func voiceMessageText(m store.Message) string {
	voice := voicePrefix + "(no speech detected)"
	if text := strings.TrimSpace(*m.Transcript); text != "" {
		voice = voicePrefix + text
	}
	if display := strings.TrimSpace(m.DisplayText); strings.HasSuffix(display, audioDisplayPlaceholder) {
		return strings.TrimSuffix(display, audioDisplayPlaceholder) + voice
	}
	return voice
}
