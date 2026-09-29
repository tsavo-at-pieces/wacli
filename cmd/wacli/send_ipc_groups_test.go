package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/types"
)

func testJID(raw string) types.JID {
	jid, err := types.ParseJID(raw)
	if err != nil {
		panic(err)
	}
	return jid
}

// fakeGroupsWA adds the group calls to the management fake.
// Groups it knows answer GetGroupInfo, and changes apply to them as WhatsApp
// would apply them, so the refreshed local snapshot shows the result.
type fakeGroupsWA struct {
	*fakeManagementWA
	mu             sync.Mutex
	groups         map[types.JID]*types.GroupInfo
	requests       []types.GroupParticipantRequest
	requestResults []types.GroupParticipant
}

func (f *fakeGroupsWA) GetGroupInfo(_ context.Context, group types.JID) (*types.GroupInfo, error) {
	f.log.add("group-info %s", group)
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.groups[group]
	if !ok {
		return nil, errors.New("not a participant of this fictional group")
	}
	clone := *info
	return &clone, nil
}

// update applies change to a known group.
func (f *fakeGroupsWA) update(group types.JID, change func(*types.GroupInfo)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if info, ok := f.groups[group]; ok {
		change(info)
	}
}

func (f *fakeGroupsWA) SetGroupTopic(_ context.Context, group types.JID, previousID, topic string) error {
	f.log.add("topic %s prev=%q %q", group, previousID, topic)
	f.update(group, func(g *types.GroupInfo) { g.Topic, g.TopicID = topic, "TOPIC02" })
	return nil
}

func (f *fakeGroupsWA) SetGroupAnnounce(_ context.Context, group types.JID, on bool) error {
	f.log.add("announce %s %t", group, on)
	f.update(group, func(g *types.GroupInfo) { g.IsAnnounce = on })
	return nil
}

func (f *fakeGroupsWA) SetGroupLocked(_ context.Context, group types.JID, on bool) error {
	f.log.add("locked %s %t", group, on)
	f.update(group, func(g *types.GroupInfo) { g.IsLocked = on })
	return nil
}

func (f *fakeGroupsWA) GetGroupRequestParticipants(_ context.Context, group types.JID) ([]types.GroupParticipantRequest, error) {
	f.log.add("requests %s", group)
	return f.requests, nil
}

func (f *fakeGroupsWA) UpdateGroupRequestParticipants(_ context.Context, group types.JID, users []types.JID, action wa.GroupParticipantRequestAction) ([]types.GroupParticipant, error) {
	f.log.add("requests %s %s %v", action, group, users)
	return f.requestResults, nil
}

// ResolveLIDToPN knows one fictional LID/phone pair, as a session would.
func (f *fakeGroupsWA) ResolveLIDToPN(_ context.Context, jid types.JID) types.JID {
	if jid.ToNonAD() == testJID(mgmtLID) {
		return testJID(mgmtPhone)
	}
	return jid
}

// fakeGroupsApp is the management fake with the group WhatsApp fake.
type fakeGroupsApp struct {
	*fakeManagementApp
	gwa *fakeGroupsWA
}

func (f *fakeGroupsApp) WA() app.WAClient { return f.gwa }

func newFakeGroupsApp(t *testing.T, storeDir string) *fakeGroupsApp {
	t.Helper()
	base := newFakeManagementApp(t, storeDir)
	gwa := &fakeGroupsWA{fakeManagementWA: base.wa, groups: map[types.JID]*types.GroupInfo{}}
	return &fakeGroupsApp{fakeManagementApp: base, gwa: gwa}
}

// testGroupInfo is a fictional group with a description, an admin known by
// LID and one member known by phone number.
func testGroupInfo() *types.GroupInfo {
	info := &types.GroupInfo{
		JID:          testJID(mgmtGroup),
		OwnerJID:     testJID(mgmtPhone),
		GroupCreated: time.Unix(1790000000, 0).UTC(),
		Participants: []types.GroupParticipant{
			{JID: testJID(mgmtLID), IsAdmin: true},
			{JID: testJID(mgmtDialed)},
		},
	}
	info.GroupName.Name = "Test <group>"
	info.Topic = "Old fictional topic"
	info.TopicID = "TOPIC01"
	return info
}

func seedTestGroup(t *testing.T, f *fakeGroupsApp) {
	t.Helper()
	info := testGroupInfo()
	f.gwa.groups[info.JID] = info
}

// groupsDaemon plays `sync --follow` for one store: it holds the lock and
// serves the production delegate socket with the management executor, which
// hands group kinds on to executeDelegatedGroupKind.
type groupsDaemon struct {
	storeDir string
	fake     *fakeGroupsApp
	mu       sync.Mutex
	requests []sendDelegateRequest
}

func startGroupsDaemon(t *testing.T, seed func(*testing.T, *fakeGroupsApp)) *groupsDaemon {
	t.Helper()
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	storeDir := shortPresenceDelegateStoreDir(t)
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatalf("lock store: %v", err)
	}
	t.Cleanup(func() { _ = lk.Release() })
	d := &groupsDaemon{storeDir: storeDir, fake: newFakeGroupsApp(t, storeDir)}
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

func (d *groupsDaemon) run(t *testing.T, asJSON bool, args []string) (string, string, error) {
	t.Helper()
	return runGroupsHelper(t, d.storeDir, asJSON, "", args)
}

func (d *groupsDaemon) received() []sendDelegateRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.requests)
}

// runGroupsHelper runs the CLI in a child process against storeDir, with
// stdin as its input.
func runGroupsHelper(t *testing.T, storeDir string, asJSON bool, stdin string, args []string) (string, string, error) {
	t.Helper()
	global := []string{"--store", storeDir, "--timeout", "5s"}
	if asJSON {
		global = append(global, "--json")
	}
	rawArgs, err := json.Marshal(append(global, args...))
	if err != nil {
		t.Fatalf("marshal helper args: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPresenceDelegateHelper$")
	cmd.Env = append(os.Environ(),
		presenceDelegateHelperEnv+"=1",
		presenceDelegateHelperArgsEnv+"="+string(rawArgs),
	)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	// The helper is a test binary, which reports its own pass after the command.
	return strings.TrimSuffix(stdout.String(), "PASS\n"), stderr.String(), err
}

// jsonString is s as encoding/json writes it.
func jsonString(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func seedPruneRows(t *testing.T, db *store.DB) {
	t.Helper()
	now := time.Now().UTC()
	created := now.AddDate(0, 0, -400)
	for _, g := range []struct {
		jid, name      string
		lastTS, leftAt time.Time
	}{
		{"120363000000000011@g.us", "Old left", now.AddDate(0, 0, -200), now.AddDate(0, 0, -200)},
		{"120363000000000012@g.us", "Recent left", now.AddDate(0, 0, -30), now.AddDate(0, 0, -30)},
		{"120363000000000013@g.us", "Old active", now.AddDate(0, 0, -220), time.Time{}},
	} {
		if err := db.UpsertGroup(g.jid, g.name, mgmtPhone, created); err != nil {
			t.Fatalf("seed group: %v", err)
		}
		if err := db.UpsertChat(g.jid, "group", g.name, g.lastTS); err != nil {
			t.Fatalf("seed chat: %v", err)
		}
		if !g.leftAt.IsZero() {
			if err := db.MarkGroupLeft(g.jid, g.leftAt); err != nil {
				t.Fatalf("seed left: %v", err)
			}
		}
	}
}

func wantStoredGroup(jid string, check func(store.Group) bool) func(*testing.T, *fakeGroupsApp) {
	return func(t *testing.T, f *fakeGroupsApp) {
		t.Helper()
		groups, err := f.db.ListGroups("", 50)
		if err != nil {
			t.Fatalf("list groups: %v", err)
		}
		for _, g := range groups {
			if g.JID == jid {
				if !check(g) {
					t.Fatalf("stored group = %+v", g)
				}
				return
			}
		}
		t.Fatalf("group %s not stored: %+v", jid, groups)
	}
}

type groupCommandCase struct {
	name string
	kind string
	args []string
	// write reports whether the command changes WhatsApp or the store.
	write bool
	seed  func(*testing.T, *fakeGroupsApp)
	// request checks the fields that reached the sync process.
	request func(*testing.T, sendDelegateRequest)
	calls   []string
	// human and json are what the direct command prints for the same result.
	human string
	json  string
	after func(*testing.T, *fakeGroupsApp)
}

func groupCommandCases(t *testing.T) []groupCommandCase {
	info := testGroupInfo()
	requestedAt := time.Unix(1790000000, 0).UTC()
	requests := []types.GroupParticipantRequest{
		{JID: testJID(mgmtLID), RequestedAt: requestedAt},
		{JID: testJID(mgmtDialed), RequestedAt: requestedAt.Add(time.Hour)},
	}
	created := info.GroupCreated.Local().Format(time.RFC3339)
	summary := "JID: " + mgmtGroup + "\nName: Test <group>\nOwner: " + mgmtPhone + "\nType: group\nCreated: " + created + "\nParticipants: 2\n"
	refresh := "group-info " + mgmtGroup

	cases := []groupCommandCase{
		{
			name:    "groups info",
			kind:    groupInfoKind,
			args:    []string{"groups", "info", "--jid", mgmtGroup},
			write:   true,
			seed:    seedTestGroup,
			request: wantTo(mgmtGroup),
			calls:   []string{refresh},
			human:   summary,
			json:    directJSON(t, info),
			after: func(t *testing.T, f *fakeGroupsApp) {
				wantStoredGroup(mgmtGroup, func(g store.Group) bool { return g.Name == "Test <group>" && g.OwnerJID == mgmtPhone })(t, f)
				ps, err := f.db.ListGroupParticipants(mgmtGroup)
				roles := map[string]string{}
				for _, p := range ps {
					roles[p.UserJID] = p.Role
				}
				if err != nil || len(ps) != 2 || roles[mgmtPhone] != "admin" || roles[mgmtDialed] != "member" {
					t.Fatalf("stored participants = %+v, %v; want the LID admin stored by phone", ps, err)
				}
			},
		},
		{
			name:  "groups topic",
			kind:  groupTopicKind,
			args:  []string{"groups", "topic", "--jid", mgmtGroup, "--text", "New fictional topic"},
			write: true,
			seed:  seedTestGroup,
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.To != mgmtGroup || req.Topic != "New fictional topic" {
					t.Fatalf("topic request = %+v", req)
				}
			},
			calls: []string{refresh, `topic ` + mgmtGroup + ` prev="TOPIC01" "New fictional topic"`, refresh},
			human: "OK\n",
			json:  directJSON(t, map[string]any{"jid": mgmtGroup, "topic": "New fictional topic"}),
		},
		{
			name:  "groups description clears",
			kind:  groupTopicKind,
			args:  []string{"groups", "description", "--jid", mgmtGroup, "--text", ""},
			write: true,
			seed:  seedTestGroup,
			calls: []string{refresh, `topic ` + mgmtGroup + ` prev="TOPIC01" ""`, refresh},
			human: "OK\n",
			json:  directJSON(t, map[string]any{"jid": mgmtGroup, "topic": ""}),
		},
		{
			name:  "groups announce-only on",
			kind:  groupAnnounceOnlyKind,
			args:  []string{"groups", "announce-only", "--jid", mgmtGroup, "--on"},
			write: true,
			seed:  seedTestGroup,
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.To != mgmtGroup || !req.Enabled {
					t.Fatalf("announce-only request = %+v", req)
				}
			},
			calls: []string{"announce " + mgmtGroup + " true", refresh},
			human: "OK\n",
			json:  directJSON(t, map[string]any{"jid": mgmtGroup, "announce_only": true}),
		},
		{
			name:  "groups locked off",
			kind:  groupLockedKind,
			args:  []string{"groups", "locked", "--jid", mgmtGroup, "--off"},
			write: true,
			seed:  seedTestGroup,
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.Enabled {
					t.Fatalf("locked --off request = %+v", req)
				}
			},
			calls: []string{"locked " + mgmtGroup + " false", refresh},
			human: "OK\n",
			json:  directJSON(t, map[string]any{"jid": mgmtGroup, "locked": false}),
		},
		{
			name:    "groups requests list",
			kind:    groupRequestsListKind,
			args:    []string{"groups", "requests", "list", "--jid", mgmtGroup},
			seed:    func(t *testing.T, f *fakeGroupsApp) { f.gwa.requests = requests },
			request: wantTo(mgmtGroup),
			calls:   []string{"requests " + mgmtGroup},
			human: mgmtLID + "\t" + requestedAt.Local().Format("2006-01-02 15:04:05") + "\t+15550000001\n" +
				mgmtDialed + "\t" + requestedAt.Add(time.Hour).Local().Format("2006-01-02 15:04:05") + "\t+12025550142\n",
			json: directJSON(t, []requestListEntry{
				{JID: mgmtLID, PhoneNumber: "+15550000001", RequestedAt: requestedAt},
				{JID: mgmtDialed, PhoneNumber: "+12025550142", RequestedAt: requestedAt.Add(time.Hour)},
			}),
		},
		{
			name:  "groups invite link get",
			kind:  groupInviteLinkGetKind,
			args:  []string{"groups", "invite", "link", "get", "--jid", mgmtGroup},
			calls: []string{"invite-link " + mgmtGroup + " reset=false"},
			human: mgmtInviteLink + "\n",
			json:  directJSON(t, map[string]any{"jid": mgmtGroup, "link": mgmtInviteLink}),
		},
		{
			name:  "groups prune",
			kind:  groupsPruneKind,
			args:  []string{"groups", "prune", "--days", "180", "--confirm"},
			write: true,
			seed:  func(t *testing.T, f *fakeGroupsApp) { seedPruneRows(t, f.db) },
			request: func(t *testing.T, req sendDelegateRequest) {
				if !slices.Equal(req.Groups, []string{"120363000000000011@g.us"}) || req.PruneDays != 180 || req.IncludeActive {
					t.Fatalf("prune request = %+v", req)
				}
			},
			human: "", // progress and the summary go to stderr
			json:  directJSON(t, map[string]any{"deleted": 1}),
			after: func(t *testing.T, f *fakeGroupsApp) {
				if _, err := f.db.GetChat("120363000000000011@g.us"); err == nil {
					t.Fatal("old left group should be deleted")
				}
				for _, jid := range []string{"120363000000000012@g.us", "120363000000000013@g.us"} {
					if _, err := f.db.GetChat(jid); err != nil {
						t.Fatalf("%s should survive: %v", jid, err)
					}
				}
			},
		},
	}
	for _, action := range []string{"approve", "reject"} {
		kind := groupRequestsApproveKind
		if action == "reject" {
			kind = groupRequestsRejectKind
		}
		cases = append(cases, groupCommandCase{
			name:  "groups requests " + action,
			kind:  kind,
			args:  []string{"groups", "requests", action, "--jid", mgmtGroup, "--user", "+1 202 555 0142", "--user", mgmtLID},
			write: true,
			seed: func(t *testing.T, f *fakeGroupsApp) {
				seedTestGroup(t, f)
				f.gwa.requestResults = testParticipants()
			},
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.To != mgmtGroup || !slices.Equal(req.Users, []string{"+1 202 555 0142", mgmtLID}) {
					t.Fatalf("requests request = %+v", req)
				}
			},
			calls: []string{"requests " + action + " " + mgmtGroup + " [" + mgmtDialed + " " + mgmtLID + "]", refresh},
			human: "OK\n",
			json:  directJSON(t, testParticipants()),
		})
	}
	return cases
}

// With the store locked by sync --follow, each group command runs in that
// process and prints what the direct command prints, with and without --json.
func TestGroupCommandsDelegateToFollowProcess(t *testing.T) {
	for _, tc := range groupCommandCases(t) {
		for _, asJSON := range []bool{false, true} {
			mode := "human"
			if asJSON {
				mode = "json"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				d := startGroupsDaemon(t, tc.seed)
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
func TestGroupCommandsExplainOlderFollowProcessRejection(t *testing.T) {
	for _, tc := range groupCommandCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			skipPresenceDelegateSocketTestOnUnsupportedOS(t)
			storeDir := shortPresenceDelegateStoreDir(t)
			if tc.seed != nil {
				tc.seed(t, newFakeGroupsApp(t, storeDir))
			}
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

			stdout, stderr, err := runGroupsHelper(t, storeDir, false, "", tc.args)
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

// Read-only mode stops every group command before it reaches a running sync
// process. Commands that only read from WhatsApp stop too: read-only never
// connects, directly or through the sync process.
func TestGroupCommandsHonorReadOnlyBeforeDelegating(t *testing.T) {
	for _, viaEnv := range []bool{false, true} {
		for _, tc := range groupCommandCases(t) {
			name := tc.name + "/flag"
			if viaEnv {
				name = tc.name + "/env"
			}
			t.Run(name, func(t *testing.T) {
				d := startGroupsDaemon(t, tc.seed)
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
				want := "read-only mode: command would connect to WhatsApp"
				if tc.write {
					want = "read-only mode: command would intentionally modify"
				}
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %v, want %q", err, want)
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

// The production dispatcher sends every group kind on to its executor instead
// of rejecting it.
func TestExecuteDelegatedSendRoutesGroupKinds(t *testing.T) {
	a, err := app.New(app.Options{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	t.Cleanup(a.Close)
	for kind := range groupKindExecutors {
		t.Run(kind, func(t *testing.T) {
			// Reaching app use proves the kind was routed; an unpaired app
			// may fail or panic after that.
			defer func() { _ = recover() }()
			_, err := executeDelegatedSend(context.Background(), a, sendDelegateRequest{
				Version: sendDelegateVersion, Kind: kind, TimeoutMS: 200,
			})
			if err != nil && strings.Contains(err.Error(), "unsupported send kind") {
				t.Fatalf("kind %q rejected: %v", kind, err)
			}
		})
	}
}

// Every kind a command sends has an executor, and every executor has a
// command in the table above.
func TestGroupKindExecutorsMatchCommandCases(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range groupCommandCases(t) {
		if _, ok := groupKindExecutors[tc.kind]; !ok {
			t.Fatalf("case %q sends kind %q, which has no executor", tc.name, tc.kind)
		}
		covered[tc.kind] = true
	}
	for kind := range groupKindExecutors {
		if !covered[kind] {
			t.Fatalf("kind %q has no command case", kind)
		}
	}
	// No group kind shadows one the management switch already handles.
	fake := newTestManagementApp(t)
	for kind := range groupKindExecutors {
		if strings.HasPrefix(kind, groupParticipantsKindPrefix) || kind == groupCreateKind || kind == groupsRefreshKind {
			t.Fatalf("kind %q collides with an existing management kind", kind)
		}
	}
	if _, err := executeDelegatedManagement(context.Background(), fake, sendDelegateRequest{Kind: "group_bogus"}); err == nil || !strings.Contains(err.Error(), `unsupported send kind "group_bogus"`) {
		t.Fatalf("unknown group kind error = %v", err)
	}
}

// Bad input fails in the caller before anything reaches the sync process.
func TestGroupCommandsValidateBeforeDelegating(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"topic on a user JID", []string{"groups", "topic", "--jid", mgmtPhone, "--text", "x"}, "expected group JID"},
		{"toggle without a mode", []string{"groups", "locked", "--jid", mgmtGroup}, "exactly one of --on or --off"},
		{"requests approve with a bad user", []string{"groups", "requests", "approve", "--jid", mgmtGroup, "--user", "not a number"}, "invalid phone number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := startGroupsDaemon(t, nil)
			stdout, stderr, err := d.run(t, false, tt.args)
			if err == nil || stdout != "" || !strings.Contains(stderr, tt.want) {
				t.Fatalf("err=%v stdout=%q stderr=%q, want a failure mentioning %q", err, stdout, stderr, tt.want)
			}
			if reqs := d.received(); len(reqs) != 0 {
				t.Fatalf("invalid command reached the sync process: %+v", reqs)
			}
		})
	}
}

// Without contention for the lock, a group command runs directly and never
// talks to a delegate socket, even one that is listening.
func TestLiveGroupCommandRunsDirectlyWhenStoreIsFree(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	storeDir := shortPresenceDelegateStoreDir(t)
	var mu sync.Mutex
	var got []sendDelegateRequest
	stop, err := startSendDelegateServerForStore(context.Background(), storeDir, sendSpacing{}, func(_ context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, req)
		return sendDelegateResponse{OK: true}, nil
	})
	if err != nil {
		t.Fatalf("start delegate server: %v", err)
	}
	defer stop()

	stdout, stderr, err := runGroupsHelper(t, storeDir, false, "", []string{"groups", "announce-only", "--jid", mgmtGroup, "--on"})
	if err == nil || stdout != "" || !strings.Contains(stderr, "not authenticated") {
		t.Fatalf("err=%v stdout=%q stderr=%q, want the direct path's auth error", err, stdout, stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("unlocked store delegated %+v", got)
	}
}

// The description change names the current description's ID, read live,
// so WhatsApp does not reject it as a conflicting edit.
func TestExecuteGroupTopicNamesCurrentDescription(t *testing.T) {
	fake := newFakeGroupsApp(t, t.TempDir())
	seedTestGroup(t, fake)
	if _, err := executeGroupTopic(context.Background(), fake, sendDelegateRequest{Kind: groupTopicKind, To: mgmtGroup, Topic: "First"}); err != nil {
		t.Fatal(err)
	}
	// The second change follows the ID the first one produced.
	if _, err := executeGroupTopic(context.Background(), fake, sendDelegateRequest{Kind: groupTopicKind, To: mgmtGroup, Topic: "Second"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"group-info " + mgmtGroup, `topic ` + mgmtGroup + ` prev="TOPIC01" "First"`, "group-info " + mgmtGroup,
		"group-info " + mgmtGroup, `topic ` + mgmtGroup + ` prev="TOPIC02" "Second"`, "group-info " + mgmtGroup,
	}
	if calls := fake.list(); !slices.Equal(calls, want) {
		t.Fatalf("calls = %q\nwant %q", calls, want)
	}
}

func TestExecuteGroupTopicStopsWhenCurrentDescriptionIsUnknown(t *testing.T) {
	fake := newFakeGroupsApp(t, t.TempDir()) // the group is unknown to WhatsApp
	_, err := executeGroupTopic(context.Background(), fake, sendDelegateRequest{Kind: groupTopicKind, To: mgmtGroup, Topic: "x"})
	if err == nil || !strings.Contains(err.Error(), "read current group description") {
		t.Fatalf("error = %v, want the lookup failure", err)
	}
	if calls := fake.list(); !slices.Equal(calls, []string{"group-info " + mgmtGroup}) {
		t.Fatalf("calls = %q, want no description change", calls)
	}
}

// Executors check a request themselves: a sync process must not trust that
// the caller validated it.
func TestExecuteGroupKindsRejectInvalidRequests(t *testing.T) {
	fake := newFakeGroupsApp(t, t.TempDir())
	seedTestGroup(t, fake)
	tests := []struct {
		name string
		req  sendDelegateRequest
		want string
	}{
		{"toggle on a user", sendDelegateRequest{Kind: groupLockedKind, To: mgmtPhone, Enabled: true}, "expected group JID"},
		{"requests with a bad user", sendDelegateRequest{Kind: groupRequestsApproveKind, To: mgmtGroup, Users: []string{"not a number"}}, "invalid phone number"},
		{"prune with nothing confirmed", sendDelegateRequest{Kind: groupsPruneKind, PruneDays: 30}, "no confirmed groups"},
		{"prune with negative days", sendDelegateRequest{Kind: groupsPruneKind, PruneDays: -1, Groups: []string{mgmtGroup}}, "must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok, err := executeDelegatedGroupKind(context.Background(), fake, tt.req)
			if !ok || err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ok=%t err=%v, want %q", ok, err, tt.want)
			}
		})
	}
	if calls := fake.list(); len(calls) != 0 {
		t.Fatalf("rejected requests reached WhatsApp: %q", calls)
	}
}

// The sync process deletes only confirmed groups that are still prunable when
// it runs, whatever else the request names.
func TestExecuteGroupsPruneDeletesOnlyConfirmedGroupsStillPrunable(t *testing.T) {
	fake := newFakeGroupsApp(t, t.TempDir())
	seedPruneRows(t, fake.db)
	resp, err := executeGroupsPrune(context.Background(), fake, sendDelegateRequest{
		Kind:      groupsPruneKind,
		PruneDays: 180,
		// Recent left is not prunable at 180 days; old active is prunable only
		// with --include-active; the last one is unknown.
		Groups: []string{"120363000000000011@g.us", "120363000000000012@g.us", "120363000000000013@g.us", "120363000000000099@g.us"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var deleted []prunedGroup
	if err := decodeGroupResult(resp.Result, &deleted); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 1 || !reflect.DeepEqual(deleted, []prunedGroup{{JID: "120363000000000011@g.us", Name: "Old left"}}) {
		t.Fatalf("pruned = %d %+v", resp.Count, deleted)
	}
	for _, jid := range []string{"120363000000000012@g.us", "120363000000000013@g.us"} {
		if _, err := fake.db.GetChat(jid); err != nil {
			t.Fatalf("%s should survive: %v", jid, err)
		}
	}
	// A group confirmed but no longer prunable leaves an empty, valid result.
	resp, err = executeGroupsPrune(context.Background(), fake, sendDelegateRequest{Kind: groupsPruneKind, PruneDays: 180, Groups: []string{"120363000000000011@g.us"}})
	if err != nil || resp.Count != 0 || string(resp.Result) != "[]" {
		t.Fatalf("second prune = %+v, %v", resp, err)
	}
}

// Without --confirm the caller asks before handing the deletion to the sync
// process, and a refusal sends nothing.
func TestGroupsPruneDelegatedAsksFirst(t *testing.T) {
	for _, tc := range []struct {
		answer  string
		deleted bool
	}{
		{"n\n", false},
		{"", false},
		{"y\n", true},
	} {
		t.Run(fmt.Sprintf("answer %q", tc.answer), func(t *testing.T) {
			d := startGroupsDaemon(t, func(t *testing.T, f *fakeGroupsApp) { seedPruneRows(t, f.db) })
			stdout, stderr, err := runGroupsHelper(t, d.storeDir, false, tc.answer, []string{"groups", "prune", "--days", "180"})
			if err != nil || stdout != "" || !strings.Contains(stderr, "About to delete 1 group(s)") {
				t.Fatalf("err=%v stdout=%q stderr=%q", err, stdout, stderr)
			}
			if got := len(d.received()); got != map[bool]int{false: 0, true: 1}[tc.deleted] {
				t.Fatalf("sync process received %d requests", got)
			}
			_, getErr := d.fake.db.GetChat("120363000000000011@g.us")
			if tc.deleted != (getErr != nil) {
				t.Fatalf("deleted=%t but lookup error = %v", tc.deleted, getErr)
			}
			if tc.deleted && !strings.Contains(stderr, "Deleted Old left") || !tc.deleted && !strings.Contains(stderr, "Aborted.") {
				t.Fatalf("stderr = %q", stderr)
			}
		})
	}
}

// A dry run only reads the store, so it works while another process holds
// the lock, with or without a delegate socket.
func TestGroupsPruneDryRunReadsWhileStoreIsLocked(t *testing.T) {
	storeDir := shortPresenceDelegateStoreDir(t)
	seedPruneRows(t, newFakeGroupsApp(t, storeDir).db)
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatalf("lock store: %v", err)
	}
	defer lk.Release()

	stdout, stderr, err := runGroupsHelper(t, storeDir, true, "", []string{"groups", "prune", "--days", "180", "--dry-run"})
	if err != nil || !strings.Contains(stdout, `"would_delete":1`) || !strings.Contains(stdout, "120363000000000011@g.us") {
		t.Fatalf("err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
}

// With the lock held and no sync process to delegate to, prune keeps the lock
// error and does not ask for a confirmation it could not act on.
func TestGroupsPruneWithoutFollowProcessKeepsLockError(t *testing.T) {
	storeDir := shortPresenceDelegateStoreDir(t)
	seedPruneRows(t, newFakeGroupsApp(t, storeDir).db)
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatalf("lock store: %v", err)
	}
	defer lk.Release()

	stdout, stderr, err := runGroupsHelper(t, storeDir, false, "y\n", []string{"--timeout", "750ms", "groups", "prune", "--days", "180"})
	if err == nil || stdout != "" || !strings.Contains(stderr, "store is locked") || strings.Contains(stderr, "About to delete") {
		t.Fatalf("err=%v stdout=%q stderr=%q, want the original lock error and no prompt", err, stdout, stderr)
	}
}

// Group reads served from the local store never wait for the lock.
func TestLocalGroupReadsDoNotTakeStoreLock(t *testing.T) {
	storeDir := shortPresenceDelegateStoreDir(t)
	f := newFakeGroupsApp(t, storeDir)
	if err := persistGroupInfo(context.Background(), f.db, f.gwa, testGroupInfo()); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatalf("lock store: %v", err)
	}
	defer lk.Release()

	for _, args := range [][]string{
		{"groups", "list"},
		{"groups", "participants", "list", "--jid", mgmtGroup},
	} {
		stdout, stderr, err := runGroupsHelper(t, storeDir, true, "", append([]string{"--read-only"}, args...))
		if err != nil || !strings.Contains(stdout, `"success":true`) || !strings.Contains(stdout, mgmtGroup) {
			t.Fatalf("%v: err=%v stdout=%q stderr=%q", args, err, stdout, stderr)
		}
	}
}

func TestSendDelegateRequestPreservesGroupFieldsInJSON(t *testing.T) {
	tests := []struct {
		name string
		req  sendDelegateRequest
		keys []string
	}{
		{
			name: "topic and toggle",
			req:  sendDelegateRequest{Kind: groupTopicKind, To: mgmtGroup, Topic: "Fictional <topic>", Enabled: true},
			// encoding/json escapes <, > and & in strings.
			keys: []string{`"topic":` + jsonString(t, "Fictional <topic>"), `"enabled":true`},
		},
		{
			name: "prune",
			req:  sendDelegateRequest{Kind: groupsPruneKind, Groups: []string{mgmtGroup, mgmtJoined}, PruneDays: 180, IncludeActive: true},
			keys: []string{`"groups":["` + mgmtGroup + `","` + mgmtJoined + `"]`, `"prune_days":180`, `"include_active":true`},
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

	// Requests of the existing kinds do not grow the new keys.
	raw, err := json.Marshal(sendDelegateRequest{Version: sendDelegateVersion, Kind: groupLeaveKind, To: mgmtGroup})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"version":1,"kind":"group_leave","to":"`+mgmtGroup+`"}` {
		t.Fatalf("leave request = %s, want only its own fields", raw)
	}
}

// A result relayed by the sync process prints exactly as the direct value
// does, and decodes back to the same value for human output.
func TestGroupResultsRoundTripUnchanged(t *testing.T) {
	values := map[string]any{
		"group info": testGroupInfo(),
		"requests":   []requestListEntry{{JID: mgmtLID, PhoneNumber: "+15550000001", RequestedAt: time.Unix(1790000000, 0).UTC()}},
		"pruned":     []prunedGroup{{JID: mgmtGroup, Name: "Test <group>"}},
	}
	for name, v := range values {
		t.Run(name, func(t *testing.T) {
			raw, err := encodeGroupResult(v)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := json.Marshal(sendDelegateResponse{OK: true, Result: raw})
			if err != nil {
				t.Fatal(err)
			}
			var resp sendDelegateResponse
			if err := json.Unmarshal(wire, &resp); err != nil {
				t.Fatal(err)
			}
			if a, b := directJSON(t, groupResultJSON(resp.Result)), directJSON(t, v); a != b {
				t.Fatalf("relayed result prints %s, direct prints %s", a, b)
			}
			decoded := reflect.New(reflect.TypeOf(v))
			if err := decodeGroupResult(resp.Result, decoded.Interface()); err != nil {
				t.Fatal(err)
			}
			again, err := json.Marshal(decoded.Elem().Interface())
			if err != nil {
				t.Fatal(err)
			}
			if string(again) != string(raw) {
				t.Fatalf("decoded result re-encodes as %s, want %s", again, raw)
			}
		})
	}
	// A missing result prints and decodes as null.
	if got := directJSON(t, groupResultJSON(nil)); got != directJSON(t, nil) {
		t.Fatalf("missing result prints %s", got)
	}
	var entries []requestListEntry
	if err := decodeGroupResult(nil, &entries); err != nil || entries != nil {
		t.Fatalf("missing result decodes to %v, %v", entries, err)
	}
}
