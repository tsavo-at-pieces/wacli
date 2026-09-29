package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// Fictional identities only.
const (
	scChannel      = "120363000000000001@newsletter"
	scOtherChannel = "120363000000000002@newsletter"
	scInvite       = "https://whatsapp.com/channel/FakeInviteCode01"
)

// fakeStatusChannelsApp is the sync process for the status, channel and call
// kinds: the management fake plus status mutes, with a WhatsApp fake that
// answers the channel, status and call calls.
type fakeStatusChannelsApp struct {
	*fakeManagementApp
	scwa *fakeStatusChannelsWA
}

func newFakeStatusChannelsApp(t *testing.T, storeDir string) *fakeStatusChannelsApp {
	t.Helper()
	m := newFakeManagementApp(t, storeDir)
	return &fakeStatusChannelsApp{fakeManagementApp: m, scwa: &fakeStatusChannelsWA{fakeManagementWA: m.wa, log: m.callLog}}
}

func (f *fakeStatusChannelsApp) WA() app.WAClient { return f.scwa }

// MuteStatus records the call and the local row, as App.MuteStatus does after
// WhatsApp accepts the patch.
func (f *fakeStatusChannelsApp) MuteStatus(_ context.Context, jid types.JID, mute bool) error {
	f.add("status-mute %s %t", jid, mute)
	contact := jid
	if jid.Server == types.HiddenUserServer {
		contact = types.NewJID("15550000001", types.DefaultUserServer)
	}
	return f.db.SetStatusMute(store.SetStatusMuteParams{JID: contact.String(), IndexJID: jid.String(), Muted: mute, UpdatedAt: time.Now()})
}

type fakeStatusChannelsWA struct {
	*fakeManagementWA
	log *callLog
}

func testChannelMeta(jid, name string) *types.NewsletterMetadata {
	id, _ := types.ParseJID(jid)
	return &types.NewsletterMetadata{
		ID:    id,
		State: types.WrappedNewsletterState{Type: types.NewsletterStateActive},
		ThreadMeta: types.NewsletterThreadMetadata{
			Name:            types.NewsletterText{Text: name},
			Description:     types.NewsletterText{Text: "Fictional description"},
			SubscriberCount: 42,
		},
		ViewerMeta: &types.NewsletterViewerMetadata{Role: types.NewsletterRoleSubscriber, Mute: types.NewsletterMuteOff},
	}
}

func testChannelPosts() []*types.NewsletterMessage {
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	return []*types.NewsletterMessage{
		{
			MessageServerID: 101, MessageID: "POST01", Type: "text", Timestamp: ts, ViewsCount: 7,
			ReactionCounts: map[string]int{"👍": 3, "🎉": 1},
			Message:        &waProto.Message{Conversation: proto.String("Fictional update")},
		},
		{
			MessageServerID: 102, MessageID: "POST02", Type: "media", Timestamp: ts.Add(time.Hour), ViewsCount: 2,
			Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String("Fictional caption"), Mimetype: proto.String("image/jpeg")}},
		},
	}
}

func (f *fakeStatusChannelsWA) ResolvePNToLID(_ context.Context, jid types.JID) types.JID {
	if jid.ToNonAD().String() == mgmtPhone {
		return types.NewJID("100000000001", types.HiddenUserServer)
	}
	return jid
}

func (f *fakeStatusChannelsWA) GetStatusPrivacy(context.Context) ([]types.StatusPrivacy, error) {
	f.log.add("status-privacy")
	return []types.StatusPrivacy{
		{Type: types.StatusPrivacyTypeBlacklist, IsDefault: true, List: []types.JID{types.NewJID("15550000002", types.DefaultUserServer)}},
		{Type: types.StatusPrivacyTypeWhitelist, List: []types.JID{types.NewJID("100000000003", types.HiddenUserServer)}},
	}, nil
}

func (f *fakeStatusChannelsWA) GetSubscribedNewsletters(context.Context) ([]*types.NewsletterMetadata, error) {
	f.log.add("subscribed-channels")
	return []*types.NewsletterMetadata{testChannelMeta(scChannel, "Test channel"), nil, testChannelMeta(scOtherChannel, "Other test channel")}, nil
}

func (f *fakeStatusChannelsWA) GetNewsletterInfo(_ context.Context, jid types.JID) (*types.NewsletterMetadata, error) {
	f.log.add("channel-info %s", jid)
	return testChannelMeta(jid.String(), "Test channel"), nil
}

func (f *fakeStatusChannelsWA) GetNewsletterInfoWithInvite(_ context.Context, key string) (*types.NewsletterMetadata, error) {
	f.log.add("channel-invite %s", key)
	return testChannelMeta(scChannel, "Test channel"), nil
}

func (f *fakeStatusChannelsWA) FollowNewsletter(_ context.Context, jid types.JID) error {
	f.log.add("follow %s", jid)
	return nil
}

func (f *fakeStatusChannelsWA) UnfollowNewsletter(_ context.Context, jid types.JID) error {
	f.log.add("unfollow %s", jid)
	return nil
}

func (f *fakeStatusChannelsWA) NewsletterToggleMute(_ context.Context, jid types.JID, mute bool) error {
	f.log.add("channel-mute %s %t", jid, mute)
	return nil
}

func (f *fakeStatusChannelsWA) NewsletterSendReaction(_ context.Context, jid types.JID, serverID types.MessageServerID, reaction string) (types.MessageID, error) {
	f.log.add("channel-react %s %d %q", jid, serverID, reaction)
	return "REACT01", nil
}

func (f *fakeStatusChannelsWA) GetNewsletterMessages(_ context.Context, jid types.JID, count int, before types.MessageServerID) ([]*types.NewsletterMessage, error) {
	f.log.add("channel-messages %s count=%d before=%d", jid, count, before)
	return testChannelPosts(), nil
}

func (f *fakeStatusChannelsWA) NewsletterMarkViewed(_ context.Context, jid types.JID, ids []types.MessageServerID) error {
	f.log.add("channel-viewed %s %v", jid, ids)
	return nil
}

func (f *fakeStatusChannelsWA) CreateNewsletter(_ context.Context, name, description string) (*types.NewsletterMetadata, error) {
	f.log.add("channel-create %q %q", name, description)
	meta := testChannelMeta(scOtherChannel, name)
	meta.ViewerMeta.Role = types.NewsletterRoleOwner
	return meta, nil
}

func (f *fakeStatusChannelsWA) RejectCall(_ context.Context, from types.JID, callID string) error {
	f.log.add("reject-call %s %s", from, callID)
	return nil
}

func (f *fakeStatusChannelsWA) Upload(_ context.Context, data []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	kind := "other"
	if mediaType == whatsmeow.MediaImage {
		kind = "image"
	}
	f.log.add("upload %s", kind)
	return whatsmeow.UploadResponse{
		URL:        "https://example.invalid/fake-upload",
		DirectPath: "/v/t62.fake/status01",
		MediaKey:   bytes.Repeat([]byte{7}, 32),
		FileLength: uint64(len(data)),
	}, nil
}

func (f *fakeStatusChannelsWA) SendProtoMessage(_ context.Context, to types.JID, msg *waProto.Message) (types.MessageID, error) {
	if img := msg.GetImageMessage(); img != nil {
		f.log.add("send %s image mime=%s caption=%q", to, img.GetMimetype(), img.GetCaption())
		return "STATUS02", nil
	}
	ext := msg.GetExtendedTextMessage()
	f.log.add("send %s text=%q argb=%x font=%d", to, ext.GetText(), ext.GetBackgroundArgb(), ext.GetFont())
	return "STATUS01", nil
}

// statusChannelsDaemon plays `sync --follow` for one store with the
// production delegate server and management dispatcher.
type statusChannelsDaemon struct {
	storeDir string
	fake     *fakeStatusChannelsApp
	mu       sync.Mutex
	requests []sendDelegateRequest
}

func startStatusChannelsDaemon(t *testing.T, seed func(*testing.T, *fakeStatusChannelsApp)) *statusChannelsDaemon {
	t.Helper()
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	storeDir := shortPresenceDelegateStoreDir(t)
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatalf("lock store: %v", err)
	}
	t.Cleanup(func() { _ = lk.Release() })
	d := &statusChannelsDaemon{storeDir: storeDir, fake: newFakeStatusChannelsApp(t, storeDir)}
	if seed != nil {
		seed(t, d.fake)
	}
	stop, err := startSendDelegateServerForStore(context.Background(), storeDir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		d.mu.Lock()
		d.requests = append(d.requests, req)
		d.mu.Unlock()
		if req.Version != sendDelegateVersion {
			return sendDelegateResponse{}, fmt.Errorf("unsupported send delegate version %d", req.Version)
		}
		return executeDelegatedManagement(ctx, d.fake, req)
	})
	if err != nil {
		t.Fatalf("start delegate server: %v", err)
	}
	t.Cleanup(stop)
	return d
}

func (d *statusChannelsDaemon) received() []sendDelegateRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.requests)
}

// writeStatusTestPNG writes a tiny fictional image for status uploads.
func writeStatusTestPNG(t *testing.T, dir string) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 200, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fictional-status.png")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type statusChannelsCase struct {
	name string
	kind string
	// args builds the command line; dir is a scratch directory outside the store.
	args    func(dir string) []string
	seed    func(*testing.T, *fakeStatusChannelsApp)
	request func(*testing.T, sendDelegateRequest)
	calls   []string
	// direct runs the command core and printer the direct command runs once
	// connected.
	direct func(context.Context, *fakeStatusChannelsApp, *rootFlags, string) error
	json   string
	after  func(*testing.T, *fakeStatusChannelsApp)
}

func fixedArgs(args ...string) func(string) []string {
	return func(string) []string { return args }
}

func seedStatusMute(t *testing.T, f *fakeStatusChannelsApp) {
	t.Helper()
	if err := f.db.SetStatusMute(store.SetStatusMuteParams{JID: mgmtPhone, IndexJID: mgmtLID, Muted: true}); err != nil {
		t.Fatalf("seed status mute: %v", err)
	}
}

func wantStatusMute(jid string, muted bool, index string) func(*testing.T, *fakeStatusChannelsApp) {
	return func(t *testing.T, f *fakeStatusChannelsApp) {
		m, ok, err := f.db.FindStatusMute(jid)
		if err != nil || !ok || m.Muted != muted || m.IndexJID != index {
			t.Fatalf("status mute for %s = %+v, %t, %v; want muted=%t index=%s", jid, m, ok, err, muted, index)
		}
	}
}

func wantNewsletterChats(jids ...string) func(*testing.T, *fakeStatusChannelsApp) {
	return func(t *testing.T, f *fakeStatusChannelsApp) {
		for _, jid := range jids {
			c, err := f.db.GetChat(jid)
			if err != nil || c.Kind != "newsletter" {
				t.Fatalf("chat %s = %+v, %v; want a newsletter chat", jid, c, err)
			}
		}
	}
}

func statusChannelsCases() []statusChannelsCase {
	channelJSON := `{"jid":"` + scChannel + `","name":"Test channel","description":"Fictional description","role":"subscriber","mute":"off","state":"active","subscribers":42}`
	otherJSON := `{"jid":"` + scOtherChannel + `","name":"Other test channel","description":"Fictional description","role":"subscriber","mute":"off","state":"active","subscribers":42}`
	return []statusChannelsCase{
		{
			name: "send status text",
			kind: statusSendKind,
			args: fixedArgs("send", "status", "--message", "Fictional status", "--background-color", "#1f7a8c", "--font", "2", "--post-send-wait", "0"),
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.Message != "Fictional status" || req.BackgroundColor != "#1f7a8c" || req.Font == nil || *req.Font != 2 || req.File != "" {
					t.Fatalf("status request = %+v", req)
				}
			},
			calls: []string{`send status@broadcast text="Fictional status" argb=ff1f7a8c font=2`},
			direct: func(ctx context.Context, a *fakeStatusChannelsApp, flags *rootFlags, _ string) error {
				font := int32(2)
				res, err := sendStatusUpdate(ctx, a, statusSendOptions{message: "Fictional status", backgroundColor: "#1f7a8c", font: &font})
				if err != nil {
					return err
				}
				return writeStatusSent(flags, res)
			},
			json: `{"success":true,"data":{"id":"STATUS01","sent":true,"to":"status@broadcast"},"error":null}` + "\n",
			after: func(t *testing.T, f *fakeStatusChannelsApp) {
				s, err := f.db.GetStatusMessage("STATUS01")
				if err != nil || !s.FromMe || s.Text != "Fictional status" || s.BackgroundColor != "#1f7a8c" || s.Font != 2 {
					t.Fatalf("stored status = %+v, %v", s, err)
				}
			},
		},
		{
			name: "send status file",
			kind: statusSendKind,
			args: func(dir string) []string {
				return []string{"send", "status", "--file", filepath.Join(dir, "fictional-status.png"), "--message", "Fictional caption", "--post-send-wait", "0"}
			},
			request: func(t *testing.T, req sendDelegateRequest) {
				if !filepath.IsAbs(req.File) || req.Message != "Fictional caption" || req.Font != nil {
					t.Fatalf("status file request = %+v", req)
				}
			},
			calls: []string{"upload image", `send status@broadcast image mime=image/png caption="Fictional caption"`},
			direct: func(ctx context.Context, a *fakeStatusChannelsApp, flags *rootFlags, dir string) error {
				res, err := sendStatusUpdate(ctx, a, statusSendOptions{message: "Fictional caption", file: filepath.Join(dir, "fictional-status.png")})
				if err != nil {
					return err
				}
				return writeStatusSent(flags, res)
			},
			json: `{"success":true,"data":{"id":"STATUS02","media":"image","mime_type":"image/png","sent":true,"to":"status@broadcast"},"error":null}` + "\n",
			after: func(t *testing.T, f *fakeStatusChannelsApp) {
				s, err := f.db.GetStatusMessage("STATUS02")
				if err != nil || s.MediaType != "image" || s.DirectPath != "/v/t62.fake/status01" || s.MediaCaption != "Fictional caption" {
					t.Fatalf("stored status = %+v, %v", s, err)
				}
			},
		},
		{
			name:    "status mute by phone",
			kind:    statusMuteKind,
			args:    fixedArgs("status", "mute", "--jid", "+1 555 000 0002"),
			request: wantTo("+1 555 000 0002"),
			calls:   []string{"status-mute " + mgmtOtherPhone + " true"},
			direct: func(ctx context.Context, a *fakeStatusChannelsApp, flags *rootFlags, _ string) error {
				res, err := setContactStatusMute(ctx, a, "+1 555 000 0002", recipientOptions{asJSON: flags.asJSON}, true)
				if err != nil {
					return err
				}
				return writeStatusMuteResult(flags, res)
			},
			json:  `{"success":true,"data":{"jid":"` + mgmtOtherPhone + `","muted":true},"error":null}` + "\n",
			after: wantStatusMute(mgmtOtherPhone, true, mgmtOtherPhone),
		},
		{
			name:  "status unmute reuses mirrored identity",
			kind:  statusUnmuteKind,
			args:  fixedArgs("status", "unmute", "--jid", mgmtPhone),
			seed:  seedStatusMute,
			calls: []string{"status-mute " + mgmtLID + " false"},
			direct: func(ctx context.Context, a *fakeStatusChannelsApp, flags *rootFlags, _ string) error {
				res, err := setContactStatusMute(ctx, a, mgmtPhone, recipientOptions{asJSON: flags.asJSON}, false)
				if err != nil {
					return err
				}
				return writeStatusMuteResult(flags, res)
			},
			json:  `{"success":true,"data":{"jid":"` + mgmtLID + `","muted":false},"error":null}` + "\n",
			after: wantStatusMute(mgmtPhone, false, mgmtLID),
		},
		{
			name: "status mute by name with pick",
			kind: statusMuteKind,
			args: fixedArgs("status", "mute", "--jid", "Sam", "--pick", "2"),
			seed: func(t *testing.T, f *fakeStatusChannelsApp) { seedMgmtContacts(t, f.fakeManagementApp) },
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.To != "Sam" || req.Pick != 2 {
					t.Fatalf("to/pick = %q/%d", req.To, req.Pick)
				}
			},
			calls: []string{"status-mute " + mgmtOtherPhone + " true"},
			direct: func(ctx context.Context, a *fakeStatusChannelsApp, flags *rootFlags, _ string) error {
				res, err := setContactStatusMute(ctx, a, "Sam", recipientOptions{pick: 2, asJSON: flags.asJSON}, true)
				if err != nil {
					return err
				}
				return writeStatusMuteResult(flags, res)
			},
			json: `{"success":true,"data":{"jid":"` + mgmtOtherPhone + `","muted":true},"error":null}` + "\n",
		},
		{
			name:  "status privacy",
			kind:  statusPrivacyKind,
			args:  fixedArgs("status", "privacy"),
			calls: []string{"status-privacy"},
			direct: func(ctx context.Context, a *fakeStatusChannelsApp, flags *rootFlags, _ string) error {
				rows, err := fetchStatusAudience(ctx, a)
				if err != nil {
					return err
				}
				return writeStatusAudience(flags, rows)
			},
			json: `{"success":true,"data":{"privacy":[{"type":"blacklist","default":true,"list":["15550000002@s.whatsapp.net"]},{"type":"whitelist","default":false,"list":["100000000003@lid"]}]},"error":null}` + "\n",
		},
		{
			name:  "channels list",
			kind:  channelsListKind,
			args:  fixedArgs("channels", "list"),
			calls: []string{"subscribed-channels"},
			direct: func(ctx context.Context, a *fakeStatusChannelsApp, flags *rootFlags, _ string) error {
				rows, err := listChannels(ctx, a)
				if err != nil {
					return err
				}
				return writeChannelsList(flags, rows)
			},
			json:  `{"success":true,"data":[` + channelJSON + `,` + otherJSON + `],"error":null}` + "\n",
			after: wantNewsletterChats(scChannel, scOtherChannel),
		},
		{
			name:    "channels info",
			kind:    channelInfoKind,
			args:    fixedArgs("channels", "info", "--jid", scChannel),
			request: wantTo(scChannel),
			calls:   []string{"channel-info " + scChannel},
			direct: func(ctx context.Context, a *fakeStatusChannelsApp, flags *rootFlags, _ string) error {
				row, err := fetchChannelInfo(ctx, a, scChannel)
				if err != nil {
					return err
				}
				return writeChannelInfo(flags, row)
			},
			json:  `{"success":true,"data":` + channelJSON + `,"error":null}` + "\n",
			after: wantNewsletterChats(scChannel),
		},
		{
			name: "channels join",
			kind: channelJoinKind,
			args: fixedArgs("channels", "join", "--invite", scInvite),
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.InviteCode != scInvite {
					t.Fatalf("invite = %q", req.InviteCode)
				}
			},
			calls: []string{"channel-invite " + scInvite, "follow " + scChannel},
			direct: func(ctx context.Context, a *fakeStatusChannelsApp, flags *rootFlags, _ string) error {
				row, err := joinChannel(ctx, a, scInvite)
				if err != nil {
					return err
				}
				return writeChannelJoined(flags, row)
			},
			json:  `{"success":true,"data":{"channel":` + channelJSON + `,"joined":true},"error":null}` + "\n",
			after: wantNewsletterChats(scChannel),
		},
		{
			name:    "channels leave",
			kind:    channelLeaveKind,
			args:    fixedArgs("channels", "leave", "--jid", scChannel),
			request: wantTo(scChannel),
			calls:   []string{"unfollow " + scChannel},
			direct: func(ctx context.Context, a *fakeStatusChannelsApp, flags *rootFlags, _ string) error {
				jid, err := leaveChannel(ctx, a, scChannel)
				if err != nil {
					return err
				}
				return writeChannelLeft(flags, jid.String())
			},
			json: `{"success":true,"data":{"jid":"` + scChannel + `","left":true},"error":null}` + "\n",
		},
		{
			name:  "channels mute",
			kind:  channelMuteKind,
			args:  fixedArgs("channels", "mute", "--jid", scChannel),
			calls: []string{"channel-mute " + scChannel + " true"},
			direct: func(ctx context.Context, a *fakeStatusChannelsApp, flags *rootFlags, _ string) error {
				jid, err := setChannelMute(ctx, a, scChannel, true)
				if err != nil {
					return err
				}
				return writeChannelMuted(flags, jid.String(), true)
			},
			json: `{"success":true,"data":{"jid":"` + scChannel + `","muted":true},"error":null}` + "\n",
		},
		{
			name:  "channels unmute",
			kind:  channelUnmuteKind,
			args:  fixedArgs("channels", "unmute", "--jid", scChannel),
			calls: []string{"channel-mute " + scChannel + " false"},
			direct: func(ctx context.Context, a *fakeStatusChannelsApp, flags *rootFlags, _ string) error {
				jid, err := setChannelMute(ctx, a, scChannel, false)
				if err != nil {
					return err
				}
				return writeChannelMuted(flags, jid.String(), false)
			},
			json: `{"success":true,"data":{"jid":"` + scChannel + `","muted":false},"error":null}` + "\n",
		},
		{
			name: "channels react",
			kind: channelReactKind,
			args: fixedArgs("channels", "react", "--jid", scChannel, "--server-id", "101", "--reaction", "👍"),
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.To != scChannel || !slices.Equal(req.ServerIDs, []int{101}) || req.Reaction != "👍" {
					t.Fatalf("react request = %+v", req)
				}
			},
			calls: []string{"channel-react " + scChannel + ` 101 "👍"`},
			direct: func(ctx context.Context, a *fakeStatusChannelsApp, flags *rootFlags, _ string) error {
				res, err := reactToChannelPost(ctx, a, scChannel, 101, "👍")
				if err != nil {
					return err
				}
				return writeChannelReaction(flags, res)
			},
			json: `{"success":true,"data":{"jid":"` + scChannel + `","server_id":101,"reaction":"👍","id":"REACT01"},"error":null}` + "\n",
		},
		{
			name:  "channels react remove",
			kind:  channelReactKind,
			args:  fixedArgs("channels", "react", "--jid", scChannel, "--server-id", "101", "--reaction", ""),
			calls: []string{"channel-react " + scChannel + ` 101 ""`},
			direct: func(ctx context.Context, a *fakeStatusChannelsApp, flags *rootFlags, _ string) error {
				res, err := reactToChannelPost(ctx, a, scChannel, 101, "")
				if err != nil {
					return err
				}
				return writeChannelReaction(flags, res)
			},
			json: `{"success":true,"data":{"jid":"` + scChannel + `","server_id":101,"reaction":"","id":"REACT01"},"error":null}` + "\n",
		},
		{
			name: "channels messages",
			kind: channelMessagesKind,
			args: fixedArgs("channels", "messages", "--jid", scChannel, "--count", "2", "--before", "200"),
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.To != scChannel || req.Count != 2 || req.BeforeServerID != 200 {
					t.Fatalf("messages request = %+v", req)
				}
			},
			calls: []string{"channel-messages " + scChannel + " count=2 before=200"},
			direct: func(ctx context.Context, a *fakeStatusChannelsApp, flags *rootFlags, _ string) error {
				res, err := fetchChannelMessages(ctx, a, scChannel, 2, 200)
				if err != nil {
					return err
				}
				return writeChannelMessages(flags, res)
			},
			json: `{"success":true,"data":{"jid":"` + scChannel + `","messages":[` +
				`{"server_id":102,"id":"POST02","timestamp":"2026-05-01T13:00:00Z","type":"media","views":2,"media_type":"image","caption":"Fictional caption","mime_type":"image/jpeg"},` +
				`{"server_id":101,"id":"POST01","timestamp":"2026-05-01T12:00:00Z","type":"text","views":7,"reactions":{"🎉":1,"👍":3},"text":"Fictional update"}` +
				`]},"error":null}` + "\n",
			after: func(t *testing.T, f *fakeStatusChannelsApp) {
				msgs, err := f.db.ListMessages(store.ListMessagesParams{ChatJID: scChannel, Limit: 10})
				if err != nil || len(msgs) != 0 {
					t.Fatalf("stored channel messages = %d, %v; want none", len(msgs), err)
				}
			},
		},
		{
			name: "channels mark-viewed",
			kind: channelMarkViewedKind,
			args: fixedArgs("channels", "mark-viewed", "--jid", scChannel, "--server-id", "101", "--server-id", "102"),
			request: func(t *testing.T, req sendDelegateRequest) {
				if !slices.Equal(req.ServerIDs, []int{101, 102}) {
					t.Fatalf("server IDs = %v", req.ServerIDs)
				}
			},
			calls: []string{"channel-viewed " + scChannel + " [101 102]"},
			direct: func(ctx context.Context, a *fakeStatusChannelsApp, flags *rootFlags, _ string) error {
				res, err := markChannelPostsViewed(ctx, a, scChannel, []int{101, 102})
				if err != nil {
					return err
				}
				return writeChannelViewed(flags, res)
			},
			json: `{"success":true,"data":{"jid":"` + scChannel + `","server_ids":[101,102],"viewed":true},"error":null}` + "\n",
		},
		{
			name: "channels create",
			kind: channelCreateKind,
			args: fixedArgs("channels", "create", "--name", "Test channel", "--description", "Fictional description"),
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.Name != "Test channel" || req.Description != "Fictional description" {
					t.Fatalf("create request = %+v", req)
				}
			},
			calls: []string{`channel-create "Test channel" "Fictional description"`},
			direct: func(ctx context.Context, a *fakeStatusChannelsApp, flags *rootFlags, _ string) error {
				row, err := createChannel(ctx, a, "Test channel", "Fictional description")
				if err != nil {
					return err
				}
				return writeChannelCreated(flags, row)
			},
			json:  `{"success":true,"data":{"channel":{"jid":"` + scOtherChannel + `","name":"Test channel","description":"Fictional description","role":"owner","mute":"off","state":"active","subscribers":42},"created":true},"error":null}` + "\n",
			after: wantNewsletterChats(scOtherChannel),
		},
		{
			name: "calls reject",
			kind: callRejectKind,
			args: fixedArgs("calls", "reject", "--from", "15550000001", "--call-id", "CALL01"),
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.To != "15550000001" || req.ID != "CALL01" {
					t.Fatalf("reject request = %+v", req)
				}
			},
			calls: []string{"reject-call " + mgmtPhone + " CALL01"},
			direct: func(ctx context.Context, a *fakeStatusChannelsApp, flags *rootFlags, _ string) error {
				res, err := rejectCall(ctx, a, "15550000001", "CALL01")
				if err != nil {
					return err
				}
				return writeCallRejected(flags, res)
			},
			json: `{"success":true,"data":{"rejected":true,"from":"` + mgmtPhone + `","call_id":"CALL01"},"error":null}` + "\n",
		},
	}
}

// directOutput runs a case's command core against a fresh fake sync process
// and captures what the direct command prints.
func directOutput(t *testing.T, tc statusChannelsCase, asJSON bool, dir string) string {
	t.Helper()
	storeDir := t.TempDir()
	fake := newFakeStatusChannelsApp(t, storeDir)
	if tc.seed != nil {
		tc.seed(t, fake)
	}
	flags := &rootFlags{storeDir: storeDir, asJSON: asJSON}
	var err error
	stdout := captureRootStdout(t, func() {
		captureRootStderr(t, func() { err = tc.direct(context.Background(), fake, flags, dir) })
	})
	if err != nil {
		t.Fatalf("direct command: %v", err)
	}
	return stdout
}

// With the store locked by sync --follow, each status, channel and call
// command runs in that process and prints exactly what it prints directly.
func TestStatusChannelsCallsDelegateToFollowProcess(t *testing.T) {
	for _, tc := range statusChannelsCases() {
		for _, asJSON := range []bool{false, true} {
			mode := "human"
			if asJSON {
				mode = "json"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				dir := t.TempDir()
				writeStatusTestPNG(t, dir)
				d := startStatusChannelsDaemon(t, tc.seed)
				global := []string{"--store", d.storeDir, "--timeout", "5s"}
				if asJSON {
					global = append(global, "--json")
				}
				stdout, stderr, err := runPresenceDelegateHelper(t, append(global, tc.args(dir)...))
				stdout = strings.TrimSuffix(stdout, "PASS\n")
				if err != nil {
					t.Fatalf("command failed: %v stdout=%q stderr=%q", err, stdout, stderr)
				}
				if strings.Contains(stderr, "store is locked") {
					t.Fatalf("command tried the direct store path: stderr=%q", stderr)
				}
				if want := directOutput(t, tc, asJSON, dir); stdout != want {
					t.Fatalf("delegated stdout = %q\ndirect prints     %q", stdout, want)
				}
				if asJSON && stdout != tc.json {
					t.Fatalf("stdout = %q\nwant     %q", stdout, tc.json)
				}

				reqs := d.received()
				if len(reqs) != 1 {
					t.Fatalf("sync process received %d requests, want 1", len(reqs))
				}
				req := reqs[0]
				if req.Version != sendDelegateVersion || req.Kind != tc.kind {
					t.Fatalf("version/kind = %d/%q, want %d/%q", req.Version, req.Kind, sendDelegateVersion, tc.kind)
				}
				if req.TimeoutMS != 5000 || req.DeadlineUnixMS == 0 {
					t.Fatalf("request budget = %dms, deadline %d", req.TimeoutMS, req.DeadlineUnixMS)
				}
				if tc.request != nil {
					tc.request(t, req)
				}
				if got := d.fake.list(); !slices.Equal(got, tc.calls) {
					t.Fatalf("sync process calls = %q\nwant %q", got, tc.calls)
				}
				if tc.after != nil {
					tc.after(t, d.fake)
				}
			})
		}
	}
}

// Human output for a few kinds, so the shared printers stay readable.
func TestStatusChannelsCallsHumanOutput(t *testing.T) {
	want := map[string]string{
		"send status text":                       "Sent status (id STATUS01)\n",
		"status unmute reuses mirrored identity": "status unmuted: " + mgmtLID + "\n",
		"status privacy":                         "blacklist (default): all contacts except 1\n  15550000002@s.whatsapp.net\nwhitelist: only 1 contacts\n  100000000003@lid\n",
		"channels leave":                         "Left channel " + scChannel + ".\n",
		"channels react":                         "Reacted 👍 to post 101 in " + scChannel + " (id REACT01)\n",
		"channels react remove":                  "Removed reaction from post 101 in " + scChannel + " (id REACT01)\n",
		"channels mark-viewed":                   "Marked 2 posts viewed in " + scChannel + ".\n",
		"calls reject":                           "Rejected call CALL01 from " + mgmtPhone + ".\n",
	}
	for _, tc := range statusChannelsCases() {
		expected, ok := want[tc.name]
		if !ok {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			if got := directOutput(t, tc, false, t.TempDir()); got != expected {
				t.Fatalf("human output = %q\nwant %q", got, expected)
			}
		})
	}
	var messages statusChannelsCase
	for _, tc := range statusChannelsCases() {
		if tc.name == "channels messages" {
			messages = tc
		}
	}
	got := directOutput(t, messages, false, t.TempDir())
	for _, part := range []string{"SERVER ID", "102", "image", "Fictional caption", "👍 3 🎉 1", "Fictional update"} {
		if !strings.Contains(got, part) {
			t.Fatalf("channel messages table missing %q:\n%s", part, got)
		}
	}
}

// A sync process started before a kind existed rejects it without running it.
func TestStatusChannelsCallsExplainOlderFollowProcessRejection(t *testing.T) {
	for _, tc := range statusChannelsCases() {
		t.Run(tc.name, func(t *testing.T) {
			skipPresenceDelegateSocketTestOnUnsupportedOS(t)
			dir := t.TempDir()
			writeStatusTestPNG(t, dir)
			storeDir := shortPresenceDelegateStoreDir(t)
			lk, err := lock.Acquire(storeDir)
			if err != nil {
				t.Fatalf("lock store: %v", err)
			}
			defer lk.Release()
			server := startPresenceDelegateTestSocket(t, storeDir, func(req sendDelegateRequest) sendDelegateResponse {
				return sendDelegateResponse{OK: false, Error: fmt.Sprintf("unsupported send kind %q", req.Kind)}
			})
			defer server.stop()

			stdout, stderr, err := runPresenceDelegateHelper(t, append([]string{"--store", storeDir, "--timeout", "5s"}, tc.args(dir)...))
			if err == nil || stdout != "" {
				t.Fatalf("older sync process: err=%v stdout=%q, want a failure and no success output", err, stdout)
			}
			if req := server.nextRequest(t); req.Kind != tc.kind {
				t.Fatalf("kind = %q, want %q", req.Kind, tc.kind)
			}
			for _, want := range []string{"does not support this command and did not run it", "restart `wacli sync`", fmt.Sprintf("unsupported send kind %q", tc.kind)} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr = %q, missing %q", stderr, want)
				}
			}
		})
	}
}

// Read-only mode stops every one of these commands before it reaches a
// running sync process.
func TestStatusChannelsCallsHonorReadOnlyBeforeDelegating(t *testing.T) {
	for _, viaEnv := range []bool{false, true} {
		for _, tc := range statusChannelsCases() {
			name := tc.name + "/flag"
			if viaEnv {
				name = tc.name + "/env"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				writeStatusTestPNG(t, dir)
				d := startStatusChannelsDaemon(t, tc.seed)
				args := []string{"--store", d.storeDir, "--timeout", "5s"}
				if viaEnv {
					t.Setenv("WACLI_READONLY", "1")
				} else {
					args = append(args, "--read-only")
				}
				var err error
				captureRootStderr(t, func() {
					captureRootStdout(t, func() { err = execute(append(args, tc.args(dir)...)) })
				})
				if err == nil || !strings.Contains(err.Error(), "read-only mode") {
					t.Fatalf("error = %v, want read-only rejection", err)
				}
				if reqs := d.received(); len(reqs) != 0 {
					t.Fatalf("read-only command reached the sync process: %+v", reqs)
				}
				if calls := d.fake.list(); len(calls) != 0 {
					t.Fatalf("read-only command ran %q", calls)
				}
			})
		}
	}
}

// The production dispatcher routes every new kind to its executor.
func TestExecuteDelegatedSendRoutesStatusChannelsKinds(t *testing.T) {
	a, err := app.New(app.Options{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	t.Cleanup(a.Close)
	for _, tc := range statusChannelsCases() {
		t.Run(tc.name, func(t *testing.T) {
			// Reaching app use proves the kind was routed; an unpaired app may
			// fail or panic after that.
			defer func() { _ = recover() }()
			_, err := executeDelegatedSend(context.Background(), a, sendDelegateRequest{
				Version: sendDelegateVersion, Kind: tc.kind, TimeoutMS: 200,
			})
			if err != nil && strings.Contains(err.Error(), "unsupported send kind") {
				t.Fatalf("kind %q rejected: %v", tc.kind, err)
			}
		})
	}
}

func TestExecuteDelegatedStatusChannelsCallsRejectsUnknownKindsAndApps(t *testing.T) {
	fake := newFakeStatusChannelsApp(t, t.TempDir())
	for _, kind := range []string{"status_list", "channel_delete", "call_accept", ""} {
		_, err := executeDelegatedStatusChannelsCalls(context.Background(), fake, sendDelegateRequest{Kind: kind})
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("unsupported send kind %q", kind)) {
			t.Fatalf("kind %q: error = %v", kind, err)
		}
	}
	// An app without status mute support rejects instead of panicking.
	_, err := executeDelegatedStatusChannelsCalls(context.Background(), newTestManagementApp(t), sendDelegateRequest{Kind: statusMuteKind, To: mgmtPhone})
	if err == nil || !strings.Contains(err.Error(), `unsupported send kind "status_mute"`) {
		t.Fatalf("app without MuteStatus: error = %v", err)
	}
	if calls := fake.list(); len(calls) != 0 {
		t.Fatalf("rejected kinds ran %q", calls)
	}
}

// The sync process checks what it can before calling WhatsApp.
func TestExecuteDelegatedStatusChannelsCallsValidateBeforeWhatsApp(t *testing.T) {
	fake := newFakeStatusChannelsApp(t, t.TempDir())
	seedMgmtContacts(t, fake.fakeManagementApp)
	tests := []struct {
		name string
		req  sendDelegateRequest
		want string
	}{
		{"status mute of a group", sendDelegateRequest{Kind: statusMuteKind, To: mgmtGroup}, "needs a contact"},
		{"status mute of an ambiguous name", sendDelegateRequest{Kind: statusMuteKind, To: "Sam"}, "use --pick N"},
		{"status send without content", sendDelegateRequest{Kind: statusSendKind}, "--message"},
		{"status send with a bad color", sendDelegateRequest{Kind: statusSendKind, Message: "hi", BackgroundColor: "blue"}, "--background-color"},
		{"channel info of a group", sendDelegateRequest{Kind: channelInfoKind, To: mgmtGroup}, "must be a channel"},
		{"channel react without server ID", sendDelegateRequest{Kind: channelReactKind, To: scChannel, Reaction: "x"}, "--server-id"},
		{"channel react with two server IDs", sendDelegateRequest{Kind: channelReactKind, To: scChannel, ServerIDs: []int{1, 2}, Reaction: "x"}, "--server-id"},
		{"channel messages without count", sendDelegateRequest{Kind: channelMessagesKind, To: scChannel}, "--count"},
		{"channel viewed without IDs", sendDelegateRequest{Kind: channelMarkViewedKind, To: scChannel}, "--server-id"},
		{"channel create without name", sendDelegateRequest{Kind: channelCreateKind}, "--name"},
		{"call reject from a group", sendDelegateRequest{Kind: callRejectKind, To: mgmtGroup, ID: "CALL01"}, "--from"},
		{"call reject without ID", sendDelegateRequest{Kind: callRejectKind, To: mgmtPhone}, "--call-id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := executeDelegatedStatusChannelsCalls(context.Background(), fake, tt.req)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
	if calls := fake.list(); len(calls) != 0 {
		t.Fatalf("rejected requests reached WhatsApp: %q", calls)
	}
}

func TestStatusChannelsCommandsValidateFlags(t *testing.T) {
	storeDir := t.TempDir()
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"status", "mute"}, "--jid is required"},
		{[]string{"status", "show"}, "--id is required"},
		{[]string{"status", "download", "--id", "ST01"}, "--output is required"},
		{[]string{"status", "list", "--limit", "0"}, "--limit"},
		{[]string{"status", "list", "--after", "yesterday"}, "unsupported time format"},
		{[]string{"channels", "react", "--jid", scChannel, "--server-id", "101"}, "--reaction is required"},
		{[]string{"channels", "react", "--jid", scChannel, "--reaction", "x"}, "--server-id is required"},
		{[]string{"channels", "messages", "--jid", scChannel, "--count", "0"}, "--count must be > 0"},
		{[]string{"channels", "mark-viewed", "--jid", scChannel}, "--server-id is required"},
		{[]string{"channels", "mark-viewed", "--jid", scChannel, "--server-id", "-3"}, "must be positive"},
		{[]string{"channels", "create"}, "--name is required"},
		{[]string{"calls", "reject", "--from", mgmtPhone}, "--from and --call-id are required"},
		{[]string{"send", "status"}, "--message or --file is required"},
		{[]string{"send", "status", "--message", "hi", "--background-color", "blue"}, "--background-color"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var err error
			captureRootStderr(t, func() {
				captureRootStdout(t, func() { err = execute(append([]string{"--store", storeDir}, tt.args...)) })
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestSendDelegateRequestPreservesStatusChannelFieldsInJSON(t *testing.T) {
	font := int32(0)
	tests := []struct {
		name string
		req  sendDelegateRequest
		keys []string
	}{
		{
			name: "status send keeps an explicit font 0",
			req:  sendDelegateRequest{Kind: statusSendKind, Message: "Fictional status", BackgroundColor: "#1f7a8c", Font: &font, PostSendWaitMS: 25},
			keys: []string{`"background_color":"#1f7a8c"`, `"font":0`},
		},
		{
			name: "channel posts",
			req:  sendDelegateRequest{Kind: channelMessagesKind, To: scChannel, Count: 5, BeforeServerID: 200, ServerIDs: []int{101, 102}},
			keys: []string{`"count":5`, `"before_server_id":200`, `"server_ids":[101,102]`},
		},
		{
			name: "channel create",
			req:  sendDelegateRequest{Kind: channelCreateKind, Name: "Test channel", Description: "Fictional description"},
			keys: []string{`"description":"Fictional description"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.Version = sendDelegateVersion
			raw, err := json.Marshal(tt.req)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			for _, key := range tt.keys {
				if !strings.Contains(string(raw), key) {
					t.Fatalf("encoded request %s missing %s", raw, key)
				}
			}
			var got sendDelegateRequest
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !reflect.DeepEqual(got, tt.req) {
				t.Fatalf("round trip = %+v\nwant %+v", got, tt.req)
			}
		})
	}
	// Without a font the key is absent, so the sync process leaves it unset.
	raw, err := json.Marshal(sendDelegateRequest{Version: sendDelegateVersion, Kind: statusSendKind, Message: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"version":1,"kind":"status_send","message":"hi"}` {
		t.Fatalf("status request = %s, want only its own fields", raw)
	}
}

func TestDelegatedResultRoundTripsCommandResults(t *testing.T) {
	res, err := fetchChannelMessages(context.Background(), newFakeStatusChannelsApp(t, t.TempDir()), scChannel, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := delegatedResult(res, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var wire sendDelegateResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	var got channelMessagesResult
	if err := decodeDelegatedResult(wire, &got); err != nil {
		t.Fatal(err)
	}
	if a, b := directJSON(t, got), directJSON(t, res); a != b {
		t.Fatalf("relayed result prints %s, direct prints %s", a, b)
	}
	if _, err := delegatedResult(nil, fmt.Errorf("boom")); err == nil {
		t.Fatal("delegatedResult hid the command error")
	}
	if err := decodeDelegatedResult(sendDelegateResponse{OK: true}, &got); err == nil || !strings.Contains(err.Error(), "restart `wacli sync`") {
		t.Fatalf("missing result error = %v", err)
	}
}

func TestStatusSendDelegateRequestMakesFileAbsolute(t *testing.T) {
	font := int32(3)
	req := statusSendOptions{message: "caption", file: "relative/photo.png", mimeOverride: "image/png", backgroundColor: "#000000", font: &font}.delegateRequest()
	if req.Kind != statusSendKind || !filepath.IsAbs(req.File) || !strings.HasSuffix(req.File, filepath.Join("relative", "photo.png")) {
		t.Fatalf("request = %+v", req)
	}
	if req.Message != "caption" || req.MIME != "image/png" || req.BackgroundColor != "#000000" || req.Font == nil || *req.Font != 3 {
		t.Fatalf("request = %+v", req)
	}
	if text := (statusSendOptions{message: "hi"}).delegateRequest(); text.File != "" {
		t.Fatalf("text request file = %q", text.File)
	}
}

func seedStatusStore(t *testing.T, storeDir string) {
	t.Helper()
	db, err := store.Open(filepath.Join(storeDir, "wacli.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for _, p := range []store.UpsertStatusMessageParams{
		{MsgID: "ST01", Timestamp: base, SenderJID: mgmtPhone, SenderName: "Sam", Text: "Fictional morning"},
		{MsgID: "ST02", Timestamp: base.Add(time.Hour), SenderJID: mgmtOtherPhone, SenderName: "Samantha",
			MediaType: "image", MediaCaption: "Fictional view", MimeType: "image/jpeg", DirectPath: "/v/t62.fake/st02",
			MediaKey: bytes.Repeat([]byte{7}, 32), FileLength: 4},
		{MsgID: "ST03", Timestamp: base.Add(2 * time.Hour), FromMe: true, SenderName: "me", Text: "Fictional mine"},
	} {
		if err := db.UpsertStatusMessage(p); err != nil {
			t.Fatalf("seed status: %v", err)
		}
	}
	if err := db.SetStatusMute(store.SetStatusMuteParams{JID: mgmtPhone, IndexJID: mgmtLID, Muted: true, UpdatedAt: base}); err != nil {
		t.Fatalf("seed mute: %v", err)
	}
}

// The local status reads take no lock and work in read-only mode, so they
// run beside a sync process that holds the store.
func TestStatusLocalReadsRunWhileStoreLocked(t *testing.T) {
	storeDir := t.TempDir()
	seedStatusStore(t, storeDir)
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatalf("lock store: %v", err)
	}
	defer lk.Release()

	run := func(args ...string) string {
		t.Helper()
		var err error
		out := captureRootStdout(t, func() {
			captureRootStderr(t, func() {
				err = execute(append([]string{"--store", storeDir, "--read-only", "--json"}, args...))
			})
		})
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out
	}

	var list struct {
		Data struct {
			Statuses []statusRecord `json:"statuses"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(run("status", "list")), &list); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range list.Data.Statuses {
		ids = append(ids, s.ID)
	}
	if !slices.Equal(ids, []string{"ST03", "ST02", "ST01"}) {
		t.Fatalf("status ids = %v", ids)
	}
	first := list.Data.Statuses[2]
	if !first.SenderMuted || first.SenderJID != mgmtPhone || first.Downloadable {
		t.Fatalf("muted text status = %+v", first)
	}
	media := list.Data.Statuses[1]
	if media.SenderMuted || !media.Downloadable || media.MediaType != "image" || media.FileLength != 4 {
		t.Fatalf("media status = %+v", media)
	}
	for _, key := range []string{"media_key", "direct_path", "file_sha256"} {
		if strings.Contains(run("status", "show", "--id", "ST02"), key) {
			t.Fatalf("status show exposes %s", key)
		}
	}

	if err := json.Unmarshal([]byte(run("status", "list", "--from", mgmtPhone, "--limit", "5")), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data.Statuses) != 1 || list.Data.Statuses[0].ID != "ST01" {
		t.Fatalf("--from statuses = %+v", list.Data.Statuses)
	}
	if err := json.Unmarshal([]byte(run("status", "list", "--after", "2026-05-01T12:30:00Z", "--before", "2026-05-01T13:30:00Z")), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data.Statuses) != 1 || list.Data.Statuses[0].ID != "ST02" {
		t.Fatalf("time-filtered statuses = %+v", list.Data.Statuses)
	}

	var mutes struct {
		Data struct {
			Mutes []store.StatusMute `json:"mutes"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(run("status", "mutes")), &mutes); err != nil {
		t.Fatal(err)
	}
	if len(mutes.Data.Mutes) != 1 || mutes.Data.Mutes[0].JID != mgmtPhone || mutes.Data.Mutes[0].IndexJID != mgmtLID {
		t.Fatalf("mutes = %+v", mutes.Data.Mutes)
	}

	var shown struct {
		Data statusRecord `json:"data"`
	}
	if err := json.Unmarshal([]byte(run("status", "show", "--id", "ST01")), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.Data.Text != "Fictional morning" || !shown.Data.SenderMuted {
		t.Fatalf("shown status = %+v", shown.Data)
	}

	var err2 error
	captureRootStderr(t, func() {
		captureRootStdout(t, func() { err2 = execute([]string{"--store", storeDir, "status", "show", "--id", "ST404"}) })
	})
	if err2 == nil || !strings.Contains(err2.Error(), "not found") {
		t.Fatalf("missing status error = %v", err2)
	}

	human := captureRootStdout(t, func() {
		captureRootStderr(t, func() { err2 = execute([]string{"--store", storeDir, "status", "list"}) })
	})
	if err2 != nil || !strings.Contains(human, "MUTED") || !strings.Contains(human, "muted") || !strings.Contains(human, "Fictional view") {
		t.Fatalf("status list human = %q, %v", human, err2)
	}
}

func TestStatusDownloadUsesDirectPathWithoutLock(t *testing.T) {
	storeDir := t.TempDir()
	seedStatusStore(t, storeDir)
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatalf("lock store: %v", err)
	}
	defer lk.Release()

	var gotPath, gotDirect string
	orig := downloadStatusMedia
	downloadStatusMedia = func(_ context.Context, directPath string, _, _, mediaKey []byte, fileLength uint64, mediaType string, target string) (int64, error) {
		gotDirect = directPath
		gotPath = target
		if mediaType != "image" || fileLength != 4 || len(mediaKey) != 32 {
			return 0, fmt.Errorf("unexpected media args %s %d %d", mediaType, fileLength, len(mediaKey))
		}
		return 4, os.WriteFile(target, []byte("fake"), 0o600)
	}
	t.Cleanup(func() { downloadStatusMedia = orig })

	outDir := t.TempDir()
	var runErr error
	stdout := captureRootStdout(t, func() {
		captureRootStderr(t, func() {
			runErr = execute([]string{"--store", storeDir, "--read-only", "--json", "status", "download", "--id", "ST02", "--output", outDir})
		})
	})
	if runErr != nil {
		t.Fatalf("status download: %v", runErr)
	}
	if gotDirect != "/v/t62.fake/st02" || filepath.Dir(gotPath) != outDir || filepath.Ext(gotPath) == "" {
		t.Fatalf("download direct=%q target=%q", gotDirect, gotPath)
	}
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Data["path"] != gotPath || resp.Data["bytes"] != float64(4) || resp.Data["id"] != "ST02" || resp.Data["downloaded"] != true {
		t.Fatalf("download response = %v", resp.Data)
	}

	captureRootStderr(t, func() {
		captureRootStdout(t, func() {
			runErr = execute([]string{"--store", storeDir, "status", "download", "--id", "ST01", "--output", outDir})
		})
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "no downloadable media") {
		t.Fatalf("text status download error = %v", runErr)
	}
}
