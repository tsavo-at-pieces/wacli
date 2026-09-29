package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
)

// Fictional identities only.
const (
	mgmtGroup      = "120363000000000001@g.us"
	mgmtJoined     = "120363000000000002@g.us"
	mgmtCreated    = "120363000000000003@g.us"
	mgmtParent     = "120363000000000009@g.us"
	mgmtPhone      = "15550000001@s.whatsapp.net"
	mgmtOtherPhone = "15550000002@s.whatsapp.net"
	mgmtLID        = "100000000001@lid"
	mgmtDialed     = "12025550142@s.whatsapp.net" // "+1 202 555 0142"
	mgmtInviteLink = "https://chat.whatsapp.com/FakeInviteCode01"
)

// callLog records what the fake sync process was asked to do, in order.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, fmt.Sprintf(format, args...))
}

func (l *callLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// fakeManagementApp stands in for the sync process that owns the store. The
// local database is real; WhatsApp and app-state writes are only recorded.
type fakeManagementApp struct {
	*callLog
	storeDir string
	db       *store.DB
	wa       *fakeManagementWA
}

func newFakeManagementApp(t *testing.T, storeDir string) *fakeManagementApp {
	t.Helper()
	db, err := store.Open(filepath.Join(storeDir, "wacli.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	log := &callLog{}
	return &fakeManagementApp{callLog: log, storeDir: storeDir, db: db, wa: &fakeManagementWA{log: log}}
}

func (f *fakeManagementApp) DB() *store.DB                                     { return f.db }
func (f *fakeManagementApp) StoreDir() string                                  { return f.storeDir }
func (f *fakeManagementApp) WA() app.WAClient                                  { return f.wa }
func (f *fakeManagementApp) Connect(context.Context, bool, func(string)) error { return nil }

func (f *fakeManagementApp) LocalResolver() (app.LocalResolver, error) {
	return nil, errors.New("the fake sync process has no live resolver")
}

func (f *fakeManagementApp) OpenSessionResolver() (app.SessionResolver, error) {
	f.add("open session resolver")
	return fakeSessionResolver{log: f.callLog, pn: types.NewJID("15550000001", types.DefaultUserServer), lid: types.NewJID("100000000001", types.HiddenUserServer)}, nil
}

func (f *fakeManagementApp) ArchiveChat(_ context.Context, jid types.JID, archive bool) error {
	f.add("archive %s %t", jid, archive)
	return nil
}

func (f *fakeManagementApp) PinChat(_ context.Context, jid types.JID, pin bool) error {
	f.add("pin %s %t", jid, pin)
	return nil
}

func (f *fakeManagementApp) MuteChat(_ context.Context, jid types.JID, mute bool, d time.Duration) error {
	f.add("mute %s %t %s", jid, mute, d)
	return nil
}

type fakeSessionResolver struct {
	log     *callLog
	pn, lid types.JID
}

func (r fakeSessionResolver) ResolveChatName(_ context.Context, chat types.JID, _ string) string {
	return chat.String()
}

func (r fakeSessionResolver) ResolveLIDToPN(_ context.Context, jid types.JID) types.JID {
	if jid.ToNonAD() == r.lid {
		return r.pn
	}
	return jid
}

func (r fakeSessionResolver) ResolvePNToLID(_ context.Context, jid types.JID) types.JID {
	if jid.ToNonAD() == r.pn {
		return r.lid
	}
	return jid
}

func (r fakeSessionResolver) Close() error {
	r.log.add("close session resolver")
	return nil
}

// fakeManagementWA stubs the WhatsApp calls the management kinds make; any
// other call panics through the embedded nil interface.
type fakeManagementWA struct {
	app.WAClient
	log          *callLog
	participants []types.GroupParticipant
	created      *types.GroupInfo
	joined       []*types.GroupInfo
	contacts     map[types.JID]types.ContactInfo
}

func (f *fakeManagementWA) LeaveGroup(_ context.Context, group types.JID) error {
	f.log.add("leave %s", group)
	return nil
}

func (f *fakeManagementWA) SetGroupName(_ context.Context, group types.JID, name string) error {
	f.log.add("rename %s %q", group, name)
	return nil
}

func (f *fakeManagementWA) UpdateGroupParticipants(_ context.Context, group types.JID, users []types.JID, action wa.GroupParticipantAction) ([]types.GroupParticipant, error) {
	f.log.add("participants %s %s %v", action, group, users)
	return f.participants, nil
}

func (f *fakeManagementWA) JoinGroupWithLink(_ context.Context, code string) (types.JID, error) {
	f.log.add("join %s", code)
	return types.ParseJID(mgmtJoined)
}

func (f *fakeManagementWA) GetGroupInviteLink(_ context.Context, group types.JID, reset bool) (string, error) {
	f.log.add("invite-link %s reset=%t", group, reset)
	return mgmtInviteLink, nil
}

func (f *fakeManagementWA) CreateGroup(_ context.Context, req wa.CreateGroupRequest) (*types.GroupInfo, error) {
	f.log.add("create %q %v announce=%t locked=%t approval=%t community=%t parent=%s",
		req.Name, req.Participants, req.IsAnnounce, req.IsLocked, req.IsJoinApprovalRequired, req.IsParent, req.LinkedParentJID)
	return f.created, nil
}

func (f *fakeManagementWA) GetJoinedGroups(context.Context) ([]*types.GroupInfo, error) {
	f.log.add("joined-groups")
	return f.joined, nil
}

func (f *fakeManagementWA) GetGroupInfo(_ context.Context, group types.JID) (*types.GroupInfo, error) {
	f.log.add("group-info %s", group)
	return nil, errors.New("no live group info in tests")
}

func (f *fakeManagementWA) GetAllContacts(context.Context) (map[types.JID]types.ContactInfo, error) {
	f.log.add("all-contacts")
	return f.contacts, nil
}

func (f *fakeManagementWA) RevokeMessage(_ context.Context, chat types.JID, id types.MessageID) (types.MessageID, error) {
	f.log.add("revoke %s %s", chat, id)
	return "REVOKE01", nil
}

func (f *fakeManagementWA) DeleteMessageForMe(_ context.Context, info types.MessageInfo, deleteMedia bool) error {
	f.log.add("delete-for-me %s %s sender=%s media=%t", info.Chat, info.ID, info.Sender, deleteMedia)
	return nil
}

func (f *fakeManagementWA) SendProtoMessage(_ context.Context, to types.JID, msg *waProto.Message) (types.MessageID, error) {
	f.log.add("send %s %q forwarded=%t", to, msg.GetExtendedTextMessage().GetText(), msg.GetExtendedTextMessage().GetContextInfo().GetIsForwarded())
	return "FWD01", nil
}

func (f *fakeManagementWA) ResolveLIDToPN(_ context.Context, jid types.JID) types.JID { return jid }
func (f *fakeManagementWA) ResolveChatName(_ context.Context, chat types.JID, _ string) string {
	return chat.String()
}

func (f *fakeManagementWA) IsOnWhatsApp(context.Context, []string) ([]types.IsOnWhatsAppResponse, error) {
	return nil, nil
}

func (f *fakeManagementWA) GetUserInfo(context.Context, []types.JID) (map[types.JID]types.UserInfo, error) {
	return map[types.JID]types.UserInfo{}, nil
}

// managementDaemon plays `sync --follow` for one store: it holds the lock and
// serves the production delegate socket with the management executor.
type managementDaemon struct {
	storeDir string
	fake     *fakeManagementApp
	mu       sync.Mutex
	requests []sendDelegateRequest
}

func startManagementDaemon(t *testing.T, seed func(*testing.T, *fakeManagementApp)) *managementDaemon {
	t.Helper()
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	storeDir := shortPresenceDelegateStoreDir(t)
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatalf("lock store: %v", err)
	}
	t.Cleanup(func() { _ = lk.Release() })
	d := &managementDaemon{storeDir: storeDir, fake: newFakeManagementApp(t, storeDir)}
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

func (d *managementDaemon) run(t *testing.T, asJSON bool, args []string) (string, string, error) {
	t.Helper()
	global := []string{"--store", d.storeDir, "--timeout", "5s"}
	if asJSON {
		global = append(global, "--json")
	}
	stdout, stderr, err := runPresenceDelegateHelper(t, append(global, args...))
	// The helper is a test binary, which reports its own pass after the command.
	return strings.TrimSuffix(stdout, "PASS\n"), stderr, err
}

func (d *managementDaemon) received() []sendDelegateRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.requests)
}

// directJSON is what out.WriteJSON prints for data, as the direct command does.
func directJSON(t *testing.T, data any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := out.WriteJSON(&buf, data); err != nil {
		t.Fatalf("encode expected output: %v", err)
	}
	return buf.String()
}

func seedMgmtContacts(t *testing.T, f *fakeManagementApp) {
	t.Helper()
	for _, c := range []struct{ jid, phone, name string }{
		{mgmtPhone, "15550000001", "Sam"},
		{mgmtOtherPhone, "15550000002", "Samantha"},
	} {
		if err := f.db.UpsertContact(c.jid, c.phone, "", c.name, "", ""); err != nil {
			t.Fatalf("seed contact: %v", err)
		}
	}
}

func seedMgmtMessage(t *testing.T, f *fakeManagementApp, fromMe bool) {
	t.Helper()
	now := time.Now().UTC()
	if err := f.db.UpsertChat(mgmtPhone, "dm", "Sam", now); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	sender := mgmtPhone
	if fromMe {
		sender = ""
	}
	if err := f.db.UpsertMessage(store.UpsertMessageParams{
		ChatJID:   mgmtPhone,
		MsgID:     "MSG01",
		SenderJID: sender,
		Timestamp: now.Add(-time.Minute),
		FromMe:    fromMe,
		Text:      "Fictional note",
	}); err != nil {
		t.Fatalf("seed message: %v", err)
	}
}

func testParticipants() []types.GroupParticipant {
	return []types.GroupParticipant{
		{JID: types.NewJID("12025550142", types.DefaultUserServer), PhoneNumber: types.NewJID("12025550142", types.DefaultUserServer)},
		{
			JID:        types.NewJID("100000000001", types.HiddenUserServer),
			LID:        types.NewJID("100000000001", types.HiddenUserServer),
			Error:      403,
			AddRequest: &types.GroupParticipantAddRequest{Code: "FakeAddCode01", Expiration: time.Unix(1790000000, 0).UTC()},
		},
	}
}

func testCreatedGroup() *types.GroupInfo {
	info := &types.GroupInfo{
		JID:          types.NewJID("120363000000000003", types.GroupServer),
		OwnerJID:     types.NewJID("15550000001", types.DefaultUserServer),
		GroupCreated: time.Unix(1790000000, 0).UTC(),
		Participants: []types.GroupParticipant{{JID: types.NewJID("12025550142", types.DefaultUserServer)}},
	}
	info.GroupName.Name = "Test <group>"
	info.IsAnnounce = true
	return info
}

type managementCommandCase struct {
	name string
	kind string
	args []string
	seed func(*testing.T, *fakeManagementApp)
	// request checks the fields that reached the sync process.
	request func(*testing.T, sendDelegateRequest)
	calls   []string
	// human and json are what the direct command prints for the same result.
	human string
	json  string
	after func(*testing.T, *fakeManagementApp)
}

func managementCommandCases(t *testing.T) []managementCommandCase {
	participantsJSON := directJSON(t, testParticipants())
	createdJSON := directJSON(t, testCreatedGroup())
	cases := []managementCommandCase{
		{
			name:    "chats archive",
			kind:    chatArchiveKind,
			args:    []string{"chats", "archive", "--chat", "+1 202 555 0142"},
			request: wantTo("+1 202 555 0142"),
			calls:   []string{"archive " + mgmtDialed + " true"},
			human:   "archive: " + mgmtDialed + "\n",
			json:    `{"success":true,"data":{"action":"archive","chat":"` + mgmtDialed + `","ok":true},"error":null}` + "\n",
		},
		{
			name:  "chats unarchive",
			kind:  chatUnarchiveKind,
			args:  []string{"chats", "unarchive", "--chat", mgmtPhone},
			calls: []string{"archive " + mgmtPhone + " false"},
			human: "unarchive: " + mgmtPhone + "\n",
			json:  `{"success":true,"data":{"action":"unarchive","chat":"` + mgmtPhone + `","ok":true},"error":null}` + "\n",
		},
		{
			name: "chats pin by name with pick",
			kind: chatPinKind,
			args: []string{"chats", "pin", "--chat", "Sam", "--pick", "2"},
			seed: seedMgmtContacts,
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.To != "Sam" || req.Pick != 2 {
					t.Fatalf("chat/pick = %q/%d, want the raw name and --pick for the sync process to resolve", req.To, req.Pick)
				}
			},
			calls: []string{"pin " + mgmtOtherPhone + " true"},
			human: "pin: " + mgmtOtherPhone + "\n",
			json:  `{"success":true,"data":{"action":"pin","chat":"` + mgmtOtherPhone + `","ok":true},"error":null}` + "\n",
		},
		{
			name:  "chats unpin",
			kind:  chatUnpinKind,
			args:  []string{"chats", "unpin", "--chat", mgmtPhone},
			calls: []string{"pin " + mgmtPhone + " false"},
			human: "unpin: " + mgmtPhone + "\n",
			json:  `{"success":true,"data":{"action":"unpin","chat":"` + mgmtPhone + `","ok":true},"error":null}` + "\n",
		},
		{
			name: "chats mute with duration",
			kind: chatMuteKind,
			args: []string{"chats", "mute", "--chat", mgmtPhone, "--duration", "90m"},
			request: func(t *testing.T, req sendDelegateRequest) {
				if time.Duration(req.MuteDurationNS) != 90*time.Minute {
					t.Fatalf("mute duration = %s, want 1h30m0s", time.Duration(req.MuteDurationNS))
				}
			},
			calls: []string{"mute " + mgmtPhone + " true 1h30m0s"},
			human: "mute: " + mgmtPhone + "\n",
			json:  `{"success":true,"data":{"action":"mute","chat":"` + mgmtPhone + `","ok":true},"error":null}` + "\n",
		},
		{
			name:  "chats unmute",
			kind:  chatUnmuteKind,
			args:  []string{"chats", "unmute", "--chat", mgmtPhone},
			calls: []string{"mute " + mgmtPhone + " false 0s"},
			human: "unmute: " + mgmtPhone + "\n",
			json:  `{"success":true,"data":{"action":"unmute","chat":"` + mgmtPhone + `","ok":true},"error":null}` + "\n",
		},
		{
			name: "groups leave",
			kind: groupLeaveKind,
			args: []string{"groups", "leave", "--jid", mgmtGroup},
			seed: func(t *testing.T, f *fakeManagementApp) {
				if err := f.db.UpsertGroup(mgmtGroup, "Test group", mgmtPhone, time.Now()); err != nil {
					t.Fatalf("seed group: %v", err)
				}
			},
			request: wantTo(mgmtGroup),
			calls:   []string{"leave " + mgmtGroup},
			human:   "OK\n",
			json:    `{"success":true,"data":{"jid":"` + mgmtGroup + `","left":true},"error":null}` + "\n",
			after: func(t *testing.T, f *fakeManagementApp) {
				left, err := f.db.ListLeftGroups()
				if err != nil || len(left) != 1 || left[0].JID != mgmtGroup {
					t.Fatalf("left groups = %+v, %v; want the group marked left locally", left, err)
				}
			},
		},
		{
			name: "groups rename",
			kind: groupRenameKind,
			args: []string{"groups", "rename", "--jid", mgmtGroup, "--name", "Test group"},
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.To != mgmtGroup || req.Name != "Test group" {
					t.Fatalf("rename request = %+v", req)
				}
			},
			calls: []string{"rename " + mgmtGroup + ` "Test group"`, "group-info " + mgmtGroup},
			human: "OK\n",
			json:  `{"success":true,"data":{"jid":"` + mgmtGroup + `","name":"Test group"},"error":null}` + "\n",
		},
		{
			name: "groups join",
			kind: groupJoinKind,
			args: []string{"groups", "join", "--code", "FakeInviteCode01"},
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.InviteCode != "FakeInviteCode01" {
					t.Fatalf("invite code = %q", req.InviteCode)
				}
			},
			calls: []string{"join FakeInviteCode01", "group-info " + mgmtJoined},
			human: "Joined: " + mgmtJoined + "\n",
			json:  `{"success":true,"data":{"jid":"` + mgmtJoined + `","joined":true},"error":null}` + "\n",
		},
		{
			name:    "groups invite link revoke",
			kind:    groupInviteRevokeKind,
			args:    []string{"groups", "invite", "link", "revoke", "--jid", mgmtGroup},
			request: wantTo(mgmtGroup),
			calls:   []string{"invite-link " + mgmtGroup + " reset=true"},
			human:   mgmtInviteLink + "\n",
			json:    `{"success":true,"data":{"jid":"` + mgmtGroup + `","link":"` + mgmtInviteLink + `","revoked":true},"error":null}` + "\n",
		},
		{
			name: "groups create",
			kind: groupCreateKind,
			args: []string{"groups", "create", "--name", "Test <group>", "--user", "+1 202 555 0142",
				"--announce-only", "--join-approval", "--linked-parent", mgmtParent},
			seed: func(t *testing.T, f *fakeManagementApp) { f.wa.created = testCreatedGroup() },
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.Name != "Test <group>" || !slices.Equal(req.Users, []string{"+1 202 555 0142"}) ||
					!req.AnnounceOnly || req.Locked || !req.JoinApproval || req.Community || req.LinkedParent != mgmtParent {
					t.Fatalf("create request = %+v", req)
				}
			},
			calls: []string{`create "Test <group>" [` + mgmtDialed + `] announce=true locked=false approval=true community=false parent=` + mgmtParent},
			human: "JID: " + mgmtCreated + "\nName: Test <group>\n",
			json:  createdJSON,
			after: func(t *testing.T, f *fakeManagementApp) {
				groups, err := f.db.ListGroups("", 10)
				if err != nil || len(groups) != 1 || groups[0].JID != mgmtCreated {
					t.Fatalf("stored groups = %+v, %v; want the created group", groups, err)
				}
			},
		},
		{
			name: "groups refresh",
			kind: groupsRefreshKind,
			args: []string{"groups", "refresh"},
			seed: func(t *testing.T, f *fakeManagementApp) {
				if err := f.db.UpsertGroup(mgmtParent, "Gone group", mgmtPhone, time.Now()); err != nil {
					t.Fatalf("seed group: %v", err)
				}
				joined := testCreatedGroup()
				f.wa.joined = []*types.GroupInfo{joined, nil}
			},
			calls: []string{"joined-groups"},
			human: "Imported 2 groups.\n",
			json:  `{"success":true,"data":{"groups":2},"error":null}` + "\n",
			after: func(t *testing.T, f *fakeManagementApp) {
				left, err := f.db.ListLeftGroups()
				if err != nil || len(left) != 1 || left[0].JID != mgmtParent {
					t.Fatalf("left groups = %+v, %v; want the group missing from WhatsApp marked left", left, err)
				}
			},
		},
		{
			name: "contacts alias set",
			kind: contactAliasSetKind,
			args: []string{"contacts", "alias", "set", "--jid", mgmtLID, "--alias", "Test alias"},
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.To != mgmtLID || req.Alias != "Test alias" {
					t.Fatalf("alias request = %+v", req)
				}
			},
			calls: []string{"open session resolver", "close session resolver"},
			human: "OK\n",
			json:  `{"success":true,"data":{"alias":"Test alias","jid":"` + mgmtLID + `"},"error":null}` + "\n",
			after: func(t *testing.T, f *fakeManagementApp) {
				aliases, err := f.db.ListContactAliases()
				if err != nil || aliases[mgmtPhone] != "Test alias" || aliases[mgmtLID] != "Test alias" {
					t.Fatalf("aliases = %v, %v; want both identities", aliases, err)
				}
			},
		},
		{
			name: "contacts alias rm",
			kind: contactAliasRmKind,
			args: []string{"contacts", "alias", "rm", "--jid", mgmtPhone},
			seed: func(t *testing.T, f *fakeManagementApp) {
				if err := f.db.SetAlias([]string{mgmtPhone, mgmtLID}, "Old alias"); err != nil {
					t.Fatalf("seed alias: %v", err)
				}
			},
			request: wantTo(mgmtPhone),
			calls:   []string{"open session resolver", "close session resolver"},
			human:   "OK\n",
			json:    `{"success":true,"data":{"jid":"` + mgmtPhone + `","removed":true},"error":null}` + "\n",
			after: func(t *testing.T, f *fakeManagementApp) {
				aliases, err := f.db.ListContactAliases()
				if err != nil || len(aliases) != 0 {
					t.Fatalf("aliases = %v, %v; want none left on either identity", aliases, err)
				}
			},
		},
		{
			name: "contacts tags add",
			kind: contactTagsAddKind,
			args: []string{"contacts", "tags", "add", "--jid", mgmtPhone, "--tag", "test-tag"},
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.To != mgmtPhone || req.Tag != "test-tag" {
					t.Fatalf("tag request = %+v", req)
				}
			},
			calls: []string{"open session resolver", "close session resolver"},
			human: "OK\n",
			json:  `{"success":true,"data":{"jid":"` + mgmtPhone + `","tag":"test-tag"},"error":null}` + "\n",
			after: wantTags([]string{"test-tag"}),
		},
		{
			name: "contacts tags rm",
			kind: contactTagsRmKind,
			args: []string{"contacts", "tags", "rm", "--jid", mgmtPhone, "--tag", "test-tag"},
			seed: func(t *testing.T, f *fakeManagementApp) {
				if err := f.db.AddTag([]string{mgmtPhone, mgmtLID}, "test-tag"); err != nil {
					t.Fatalf("seed tag: %v", err)
				}
			},
			calls: []string{"open session resolver", "close session resolver"},
			human: "OK\n",
			json:  `{"success":true,"data":{"jid":"` + mgmtPhone + `","removed":true,"tag":"test-tag"},"error":null}` + "\n",
			after: wantTags(nil),
		},
		{
			name: "contacts refresh",
			kind: contactsRefreshKind,
			args: []string{"contacts", "refresh"},
			seed: func(t *testing.T, f *fakeManagementApp) {
				f.wa.contacts = map[types.JID]types.ContactInfo{
					types.NewJID("15550000001", types.DefaultUserServer): {FullName: "Sam"},
					types.NewADJID("15550000002", 0, 3):                  {PushName: "Samantha"},
				}
			},
			calls: []string{"all-contacts"},
			human: "Imported 2 contacts.\n",
			json:  `{"success":true,"data":{"contacts":2},"error":null}` + "\n",
			after: func(t *testing.T, f *fakeManagementApp) {
				if c, err := f.db.GetContact(mgmtOtherPhone); err != nil || c.Name != "Samantha" {
					t.Fatalf("imported contact = %+v, %v; want the canonical phone row", c, err)
				}
			},
		},
		{
			name: "messages delete",
			kind: messageDeleteKind,
			args: []string{"messages", "delete", "--chat", mgmtPhone, "--id", "MSG01", "--post-send-wait", "25ms"},
			seed: func(t *testing.T, f *fakeManagementApp) { seedMgmtMessage(t, f, true) },
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.To != mgmtPhone || req.ID != "MSG01" || req.DeleteMedia || req.PostSendWaitMS != 25 {
					t.Fatalf("delete request = %+v", req)
				}
			},
			calls: []string{"revoke " + mgmtPhone + " MSG01"},
			human: "Deleted message MSG01 in " + mgmtPhone + " (id REVOKE01)\n",
			json:  `{"success":true,"data":{"id":"REVOKE01","revoked":true,"target":"MSG01","to":"` + mgmtPhone + `"},"error":null}` + "\n",
			after: wantMessageState(func(m store.Message) bool { return m.Revoked }),
		},
		{
			name: "messages delete for me with media",
			kind: messageDeleteForMeKind,
			args: []string{"messages", "delete", "--chat", mgmtPhone, "--id", "MSG01", "--for-me", "--delete-media", "--post-send-wait", "0"},
			seed: func(t *testing.T, f *fakeManagementApp) {
				seedMgmtMessage(t, f, false)
				media := filepath.Join(f.storeDir, "media-MSG01.jpg")
				if err := os.WriteFile(media, []byte("fake image"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := f.db.MarkMediaDownloaded(mgmtPhone, "MSG01", media, time.Now()); err != nil {
					t.Fatalf("seed media path: %v", err)
				}
			},
			request: func(t *testing.T, req sendDelegateRequest) {
				if !req.DeleteMedia || req.PostSendWaitMS != 0 {
					t.Fatalf("delete-for-me request = %+v", req)
				}
			},
			calls: []string{"delete-for-me " + mgmtPhone + " MSG01 sender=" + mgmtPhone + " media=true"},
			human: "Deleted message MSG01 for me in " + mgmtPhone + "\n",
			json:  `{"success":true,"data":{"deleted_for_me":true,"deleted_media":true,"target":"MSG01","to":"` + mgmtPhone + `"},"error":null}` + "\n",
			after: func(t *testing.T, f *fakeManagementApp) {
				wantMessageState(func(m store.Message) bool { return m.DeletedForMe })(t, f)
				if _, err := os.Stat(filepath.Join(f.storeDir, "media-MSG01.jpg")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("local media stat = %v, want it removed", err)
				}
			},
		},
		{
			name:  "messages revoke",
			kind:  messageRevokeKind,
			args:  []string{"messages", "revoke", "--chat", mgmtPhone, "--id", "MSG01", "--post-send-wait", "0"},
			seed:  func(t *testing.T, f *fakeManagementApp) { seedMgmtMessage(t, f, true) },
			calls: []string{"revoke " + mgmtPhone + " MSG01"},
			human: "Revoked message MSG01 in " + mgmtPhone + " (id REVOKE01)\n",
			json:  `{"success":true,"data":{"id":"REVOKE01","revoked":true,"target":"MSG01","to":"` + mgmtPhone + `"},"error":null}` + "\n",
			after: wantMessageState(func(m store.Message) bool { return m.Revoked }),
		},
		{
			name:  "messages revoke unknown message",
			kind:  messageRevokeKind,
			args:  []string{"messages", "revoke", "--chat", "+1 202 555 0142", "--id", "MSG404", "--post-send-wait", "0"},
			calls: []string{"revoke " + mgmtDialed + " MSG404"},
			human: "Revoked message MSG404 in " + mgmtDialed + " (id REVOKE01)\n",
			json:  `{"success":true,"data":{"id":"REVOKE01","revoked":true,"target":"MSG404","to":"` + mgmtDialed + `"},"error":null}` + "\n",
		},
		{
			name: "messages forward",
			kind: messageForwardKind,
			args: []string{"messages", "forward", "--chat", mgmtPhone, "--id", "MSG01", "--to", "Sam", "--pick", "2", "--post-send-wait", "0"},
			seed: func(t *testing.T, f *fakeManagementApp) {
				seedMgmtContacts(t, f)
				seedMgmtMessage(t, f, false)
			},
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.Chat != mgmtPhone || req.ID != "MSG01" || req.To != "Sam" || req.Pick != 2 {
					t.Fatalf("forward request = %+v", req)
				}
			},
			calls: []string{"send " + mgmtOtherPhone + ` "Fictional note" forwarded=true`},
			human: "Forwarded message MSG01 to " + mgmtOtherPhone + " (id FWD01)\n",
			json:  `{"success":true,"data":{"forwarded":true,"id":"FWD01","source":"MSG01","to":"` + mgmtOtherPhone + `"},"error":null}` + "\n",
			after: func(t *testing.T, f *fakeManagementApp) {
				if m, err := f.db.GetMessage(mgmtOtherPhone, "FWD01"); err != nil || !m.FromMe || !m.IsForwarded {
					t.Fatalf("stored forward = %+v, %v", m, err)
				}
			},
		},
	}
	for _, action := range []string{"add", "remove", "promote", "demote"} {
		cases = append(cases, managementCommandCase{
			name: "groups participants " + action,
			kind: groupParticipantsKindPrefix + action,
			args: []string{"groups", "participants", action, "--jid", mgmtGroup, "--user", "+1 202 555 0142", "--user", mgmtLID},
			seed: func(t *testing.T, f *fakeManagementApp) { f.wa.participants = testParticipants() },
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.To != mgmtGroup || !slices.Equal(req.Users, []string{"+1 202 555 0142", mgmtLID}) {
					t.Fatalf("participants request = %+v", req)
				}
			},
			calls: []string{"participants " + action + " " + mgmtGroup + " [" + mgmtDialed + " " + mgmtLID + "]", "group-info " + mgmtGroup},
			human: "OK\n",
			json:  participantsJSON,
		})
	}
	return cases
}

func wantTo(to string) func(*testing.T, sendDelegateRequest) {
	return func(t *testing.T, req sendDelegateRequest) {
		if req.To != to {
			t.Fatalf("delegated target = %q, want %q", req.To, to)
		}
	}
}

func wantTags(want []string) func(*testing.T, *fakeManagementApp) {
	return func(t *testing.T, f *fakeManagementApp) {
		for _, jid := range []string{mgmtPhone, mgmtLID} {
			tags, err := f.db.ListTags(jid)
			if err != nil || !slices.Equal(tags, want) {
				t.Fatalf("tags on %s = %v, %v; want %v", jid, tags, err, want)
			}
		}
	}
}

func wantMessageState(ok func(store.Message) bool) func(*testing.T, *fakeManagementApp) {
	return func(t *testing.T, f *fakeManagementApp) {
		m, err := f.db.GetMessage(mgmtPhone, "MSG01")
		if err != nil || !ok(m) {
			t.Fatalf("stored message = %+v, %v", m, err)
		}
	}
}

// With the store locked by sync --follow, each management command runs in that
// process and prints what the direct command prints, with and without --json.
func TestManagementCommandsDelegateToFollowProcess(t *testing.T) {
	for _, tc := range managementCommandCases(t) {
		for _, asJSON := range []bool{false, true} {
			mode := "human"
			if asJSON {
				mode = "json"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				d := startManagementDaemon(t, tc.seed)
				stdout, stderr, err := d.run(t, asJSON, tc.args)
				if err != nil {
					t.Fatalf("command failed: %v stdout=%q stderr=%q", err, stdout, stderr)
				}
				if strings.Contains(stderr, "store is locked") {
					t.Fatalf("command tried the direct store path: stderr=%q", stderr)
				}
				want := tc.human
				if asJSON {
					want = tc.json
				}
				if stdout != want {
					t.Fatalf("stdout = %q\nwant     %q", stdout, want)
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
					t.Fatalf("request budget = %dms, deadline %d; want the caller's --timeout and deadline", req.TimeoutMS, req.DeadlineUnixMS)
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

// A sync process started before a kind existed rejects it without running it.
// The caller reports that and how to recover, and prints no success.
func TestManagementCommandsExplainOlderFollowProcessRejection(t *testing.T) {
	for _, tc := range managementCommandCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			skipPresenceDelegateSocketTestOnUnsupportedOS(t)
			storeDir := shortPresenceDelegateStoreDir(t)
			lk, err := lock.Acquire(storeDir)
			if err != nil {
				t.Fatalf("lock store: %v", err)
			}
			defer lk.Release()
			server := startPresenceDelegateTestSocket(t, storeDir, func(req sendDelegateRequest) sendDelegateResponse {
				// The exact rejection an older executeDelegatedSend returns.
				return sendDelegateResponse{OK: false, Error: fmt.Sprintf("unsupported send kind %q", req.Kind)}
			})
			defer server.stop()

			stdout, stderr, err := runPresenceDelegateHelper(t, append([]string{"--store", storeDir, "--timeout", "5s"}, tc.args...))
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

// Read-only mode stops every management command before it reaches a running
// sync process that could perform it.
func TestManagementCommandsHonorReadOnlyBeforeDelegating(t *testing.T) {
	for _, viaEnv := range []bool{false, true} {
		for _, tc := range managementCommandCases(t) {
			name := tc.name + "/flag"
			if viaEnv {
				name = tc.name + "/env"
			}
			t.Run(name, func(t *testing.T) {
				d := startManagementDaemon(t, tc.seed)
				args := []string{"--store", d.storeDir, "--timeout", "5s"}
				if viaEnv {
					t.Setenv("WACLI_READONLY", "1")
				} else {
					args = append(args, "--read-only")
				}
				var err error
				captureRootStderr(t, func() {
					captureRootStdout(t, func() { err = execute(append(args, tc.args...)) })
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

func TestManagementCommandWithoutFollowProcessKeepsLockError(t *testing.T) {
	storeDir := shortPresenceDelegateStoreDir(t)
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatalf("lock store: %v", err)
	}
	defer lk.Release()

	stdout, stderr, err := runPresenceDelegateHelper(t, []string{"--store", storeDir, "--timeout", "750ms", "groups", "leave", "--jid", mgmtGroup})
	if err == nil || stdout != "" || !strings.Contains(stderr, "store is locked") || strings.Contains(stderr, "send delegate unavailable") {
		t.Fatalf("err=%v stdout=%q stderr=%q, want the original lock error", err, stdout, stderr)
	}
}

func TestSendDelegateRequestPreservesManagementFieldsInJSON(t *testing.T) {
	tests := []struct {
		name string
		req  sendDelegateRequest
		keys []string
	}{
		{
			name: "chat state",
			req:  sendDelegateRequest{Kind: chatMuteKind, To: "Sam", Pick: 2, MuteDurationNS: int64(90*time.Minute + time.Nanosecond)},
			keys: []string{`"mute_duration_ns":5400000000001`, `"pick":2`},
		},
		{
			name: "group create",
			req: sendDelegateRequest{Kind: groupCreateKind, Name: "Test group", Users: []string{"+1 202 555 0142", mgmtLID},
				AnnounceOnly: true, Locked: true, JoinApproval: true, Community: true, LinkedParent: mgmtParent},
			keys: []string{`"users":["+1 202 555 0142","` + mgmtLID + `"]`, `"announce_only":true`, `"locked":true`, `"join_approval":true`, `"community":true`, `"linked_parent":"` + mgmtParent + `"`},
		},
		{
			name: "group participants and join",
			req:  sendDelegateRequest{Kind: groupParticipantsKindPrefix + "add", To: mgmtGroup, Users: []string{mgmtPhone}, InviteCode: "FakeInviteCode01"},
			keys: []string{`"users":["` + mgmtPhone + `"]`, `"invite_code":"FakeInviteCode01"`},
		},
		{
			name: "contact metadata",
			req:  sendDelegateRequest{Kind: contactAliasSetKind, To: mgmtLID, Alias: "Test alias", Tag: "test-tag"},
			keys: []string{`"alias":"Test alias"`, `"tag":"test-tag"`},
		},
		{
			name: "message delete and forward",
			req:  sendDelegateRequest{Kind: messageForwardKind, Chat: mgmtPhone, To: "Sam", ID: "MSG01", DeleteMedia: true, PostSendWaitMS: 25},
			keys: []string{`"chat":"` + mgmtPhone + `"`, `"delete_media":true`, `"post_send_wait_ms":25`},
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

	// Requests of the existing kinds do not grow new keys.
	raw, err := json.Marshal(sendDelegateRequest{Version: sendDelegateVersion, Kind: "text", To: mgmtPhone, Message: "hi"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(raw) != `{"version":1,"kind":"text","to":"`+mgmtPhone+`","message":"hi"}` {
		t.Fatalf("text request = %s, want only its own fields", raw)
	}
}

func TestSendDelegateResponsePassesWhatsAppResultsThroughUnchanged(t *testing.T) {
	participants, err := json.Marshal(testParticipants())
	if err != nil {
		t.Fatal(err)
	}
	group, err := json.Marshal(testCreatedGroup())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(sendDelegateResponse{OK: true, Participants: participants, Group: group, Name: "Test <group>", Link: mgmtInviteLink, Count: 2, DeletedMedia: true})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got sendDelegateResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Name != "Test <group>" || got.Link != mgmtInviteLink || got.Count != 2 || !got.DeletedMedia {
		t.Fatalf("round trip = %+v", got)
	}
	// Printing the relayed bytes matches printing the whatsmeow values.
	if a, b := directJSON(t, got.Participants), directJSON(t, testParticipants()); a != b {
		t.Fatalf("relayed participants print %s, direct prints %s", a, b)
	}
	if a, b := directJSON(t, got.Group), directJSON(t, testCreatedGroup()); a != b {
		t.Fatalf("relayed group prints %s, direct prints %s", a, b)
	}
	// A nil result prints null either way.
	nilGroup, err := json.Marshal((*types.GroupInfo)(nil))
	if err != nil {
		t.Fatal(err)
	}
	if a, b := directJSON(t, json.RawMessage(nilGroup)), directJSON(t, (*types.GroupInfo)(nil)); a != b {
		t.Fatalf("relayed nil group prints %s, direct prints %s", a, b)
	}
}

func newTestManagementApp(t *testing.T) *fakeManagementApp {
	t.Helper()
	return newFakeManagementApp(t, t.TempDir())
}

func TestExecuteDelegatedManagementRejectsUnknownKind(t *testing.T) {
	fake := newTestManagementApp(t)
	for _, kind := range []string{"chat_archive_all", groupParticipantsKindPrefix + "ban", "contact_alias", ""} {
		_, err := executeDelegatedManagement(context.Background(), fake, sendDelegateRequest{Kind: kind, To: mgmtPhone})
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("unsupported send kind %q", kind)) {
			t.Fatalf("kind %q: error = %v, want unsupported kind", kind, err)
		}
	}
	if calls := fake.list(); len(calls) != 0 {
		t.Fatalf("unknown kinds ran %q", calls)
	}
}

// The production dispatcher sends every management kind on to its executor
// instead of rejecting it.
func TestExecuteDelegatedSendRoutesManagementKinds(t *testing.T) {
	a, err := app.New(app.Options{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	t.Cleanup(a.Close)
	for _, tc := range managementCommandCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			// Reaching app use proves the kind was routed; an unpaired app
			// may fail or panic after that.
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

// The sync process cannot prompt, so an ambiguous chat name fails the way the
// non-interactive direct command does, and --pick selects a match.
func TestExecuteDelegatedChatStateResolvesLikeNonInteractiveCommand(t *testing.T) {
	fake := newTestManagementApp(t)
	seedMgmtContacts(t, fake)

	_, err := executeDelegatedManagement(context.Background(), fake, sendDelegateRequest{Kind: chatArchiveKind, To: "Sam"})
	if err == nil || !strings.Contains(err.Error(), "matches 2 recipients; pass a JID or use --pick N") {
		t.Fatalf("ambiguous name error = %v", err)
	}
	if calls := fake.list(); len(calls) != 0 {
		t.Fatalf("ambiguous name changed a chat: %q", calls)
	}
	_, err = executeDelegatedManagement(context.Background(), fake, sendDelegateRequest{Kind: chatArchiveKind, To: "Sam", Pick: 3})
	if err == nil || !strings.Contains(err.Error(), "--pick 3 is out of range") {
		t.Fatalf("out-of-range pick error = %v", err)
	}
	resp, err := executeDelegatedManagement(context.Background(), fake, sendDelegateRequest{Kind: chatArchiveKind, To: "Sam", Pick: 1})
	if err != nil || resp.Chat != mgmtPhone || resp.Action != "archive" {
		t.Fatalf("picked archive = %+v, %v", resp, err)
	}
}

func TestExecuteDelegatedChatMuteUsesExactDuration(t *testing.T) {
	fake := newTestManagementApp(t)
	d := 90*time.Minute + time.Nanosecond
	if _, err := executeDelegatedManagement(context.Background(), fake, sendDelegateRequest{Kind: chatMuteKind, To: mgmtPhone, MuteDurationNS: int64(d)}); err != nil {
		t.Fatal(err)
	}
	// Omitting --duration mutes forever, as it does directly.
	if _, err := executeDelegatedManagement(context.Background(), fake, sendDelegateRequest{Kind: chatMuteKind, To: mgmtPhone}); err != nil {
		t.Fatal(err)
	}
	want := []string{"mute " + mgmtPhone + " true " + d.String(), "mute " + mgmtPhone + " true 0s"}
	if calls := fake.list(); !slices.Equal(calls, want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
}

func TestExecuteDelegatedMessageDeleteChecksBeforeWhatsApp(t *testing.T) {
	fake := newTestManagementApp(t)
	seedMgmtContacts(t, fake)
	seedMgmtMessage(t, fake, false) // someone else's message

	tests := []struct {
		name string
		req  sendDelegateRequest
		want string
	}{
		{"delete media for everyone", sendDelegateRequest{Kind: messageDeleteKind, To: mgmtPhone, ID: "MSG01", DeleteMedia: true}, "--delete-media requires --for-me"},
		{"delete another sender's message for everyone", sendDelegateRequest{Kind: messageDeleteKind, To: mgmtPhone, ID: "MSG01"}, "was not sent by me"},
		{"revoke another sender's message", sendDelegateRequest{Kind: messageRevokeKind, To: mgmtPhone, ID: "MSG01"}, "was not sent by me"},
		{"delete a message not in the store", sendDelegateRequest{Kind: messageDeleteForMeKind, To: mgmtPhone, ID: "MSG404"}, "no rows"},
		{"forward to an ambiguous name without --pick", sendDelegateRequest{Kind: messageForwardKind, Chat: mgmtPhone, ID: "MSG01", To: "Sam"}, "use --pick N"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := executeDelegatedManagement(context.Background(), fake, tt.req)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
	if calls := fake.list(); len(calls) != 0 {
		t.Fatalf("rejected requests reached WhatsApp: %q", calls)
	}
}

// Management kinds share the send queue, so #446 holds for them too: one still
// queued when its caller gives up is refused and never runs.
func TestProductionServerDropsQueuedManagementAfterCallerTimeout(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	storeDir := shortPresenceDelegateStoreDir(t)

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var executed atomic.Int32
	stop, err := startSendDelegateServerForStore(context.Background(), storeDir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		executed.Add(1)
		if req.Kind == "text" {
			started <- struct{}{}
			<-release
		}
		return sendDelegateResponse{OK: true}, nil
	})
	if err != nil {
		t.Fatalf("start delegate server: %v", err)
	}
	defer stop()

	slowDone := make(chan error, 1)
	go func() {
		_, err := delegateSend(context.Background(), &rootFlags{storeDir: storeDir, timeout: 10 * time.Second}, sendDelegateRequest{Kind: "text", Message: "slow"})
		slowDone <- err
	}()
	<-started

	queued := &rootFlags{storeDir: storeDir, timeout: sendDelegateReplyMargin + 300*time.Millisecond}
	_, err = delegateSend(context.Background(), queued, sendDelegateRequest{Kind: messageDeleteKind, To: mgmtPhone, ID: "MSG01"})
	if err == nil || !strings.Contains(err.Error(), "it was not sent") {
		t.Fatalf("queued delete error = %v, want an explicit refusal", err)
	}
	close(release)
	if err := <-slowDone; err != nil {
		t.Fatalf("slow send: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // give a late dispatch the chance to happen
	if got := executed.Load(); got != 1 {
		t.Fatalf("executed %d operations, want only the slow one", got)
	}
}

func TestExplainUnsupportedDelegateKindOnlyWrapsItsOwnKind(t *testing.T) {
	rejected := fmt.Errorf("unsupported send kind %q", groupLeaveKind)
	err := explainUnsupportedDelegateKind(rejected, groupLeaveKind)
	if err == nil || !strings.Contains(err.Error(), "did not run it") || !errors.Is(err, rejected) {
		t.Fatalf("error = %v, want the restart hint wrapping the rejection", err)
	}
	for _, other := range []error{nil, errors.New("not connected"), fmt.Errorf("unsupported send kind %q", groupJoinKind)} {
		if got := explainUnsupportedDelegateKind(other, groupLeaveKind); got != other {
			t.Fatalf("unrelated error %v became %v", other, got)
		}
	}
}
