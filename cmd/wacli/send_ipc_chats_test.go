package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// The fake sync process records chat, list and message changes instead of
// writing app state; internal/app tests cover the patches and persistence.

func (f *fakeManagementApp) DeleteChat(_ context.Context, jid types.JID, deleteMedia bool) (int64, error) {
	f.add("delete-chat %s media=%t", jid, deleteMedia)
	return 3, nil
}

func (f *fakeManagementApp) ClearChat(_ context.Context, jid types.JID, deleteStarred, deleteMedia bool) (int64, error) {
	f.add("clear-chat %s starred=%t media=%t", jid, deleteStarred, deleteMedia)
	return 2, nil
}

func (f *fakeManagementApp) LockChat(_ context.Context, jid types.JID, locked bool) error {
	f.add("lock %s %t", jid, locked)
	return nil
}

func (f *fakeManagementApp) SetChatFavorite(_ context.Context, jid types.JID, favorite bool) error {
	f.add("favorite %s %t", jid, favorite)
	return nil
}

func (f *fakeManagementApp) CreateChatList(_ context.Context, name string) (store.ChatList, error) {
	f.add("list-create %q", name)
	return store.ChatList{ID: "7", Name: name}, nil
}

func (f *fakeManagementApp) RenameChatList(_ context.Context, ref, name string) (store.ChatList, error) {
	f.add("list-rename %s %q", ref, name)
	return store.ChatList{ID: "7", Name: name}, nil
}

func (f *fakeManagementApp) DeleteChatList(_ context.Context, ref string) (store.ChatList, error) {
	f.add("list-delete %s", ref)
	return store.ChatList{ID: "7", Name: "Test list", Deleted: true}, nil
}

func (f *fakeManagementApp) SetChatListMember(_ context.Context, ref string, jid types.JID, member bool) (store.ChatList, error) {
	f.add("list-member %s %s %t", ref, jid, member)
	return store.ChatList{ID: "7", Name: "Test list"}, nil
}

func (f *fakeManagementApp) StarMessage(_ context.Context, ref app.MessageRef, starred bool) error {
	f.add("star %s %s fromMe=%t sender=%s %t", ref.Chat, ref.ID, ref.FromMe, ref.Sender, starred)
	return nil
}

func (f *fakeManagementWA) SetDisappearingTimer(_ context.Context, chat types.JID, timer time.Duration) error {
	f.log.add("disappearing %s %s", chat, timer)
	return nil
}

func (f *fakeManagementWA) SendEventMessage(_ context.Context, to types.JID, msg *waProto.Message) (types.MessageID, error) {
	ev := msg.GetEventMessage()
	f.log.add("send-event %s %q start=%d end=%d location=%q secret=%d", to, ev.GetName(), ev.GetStartTime(), ev.GetEndTime(), ev.GetLocation().GetName(), len(msg.GetMessageContextInfo().GetMessageSecret()))
	return "EVT01", nil
}

// describeChatsMessagesProto summarizes the protocol messages these commands
// send, for the fake's call log. Other messages keep the forward format.
func describeChatsMessagesProto(msg *waProto.Message) string {
	switch {
	case msg.GetPinInChatMessage() != nil:
		pin := msg.GetPinInChatMessage()
		return fmt.Sprintf("pin %s type=%s secs=%d fromMe=%t", pin.GetKey().GetID(), pin.GetType(), msg.GetMessageContextInfo().GetMessageAddOnDurationInSecs(), pin.GetKey().GetFromMe())
	case msg.GetKeepInChatMessage() != nil:
		keep := msg.GetKeepInChatMessage()
		return fmt.Sprintf("keep %s type=%s", keep.GetKey().GetID(), keep.GetKeepType())
	case msg.GetContactMessage() != nil || msg.GetContactsArrayMessage() != nil:
		return fmt.Sprintf("contacts %q", wa.ContactMessageText(msg))
	}
	return ""
}

func chatsMessagesCommandCases(t *testing.T) []managementCommandCase {
	t.Helper()
	jsonOK := func(data string) string { return `{"success":true,"data":` + data + `,"error":null}` + "\n" }
	fromMe := func(t *testing.T, f *fakeManagementApp) { seedMgmtMessage(t, f, true) }
	return []managementCommandCase{
		{
			name:  "chats delete",
			kind:  chatDeleteKind,
			args:  []string{"chats", "delete", "--chat", mgmtPhone, "--confirm"},
			calls: []string{"delete-chat " + mgmtPhone + " media=false"},
			human: "delete: " + mgmtPhone + " (kept 3 local messages as tombstones)\n",
			json:  jsonOK(`{"action":"delete","chat":"` + mgmtPhone + `","messages_hidden":3,"ok":true}`),
		},
		{
			name: "chats clear",
			kind: chatClearKind,
			args: []string{"chats", "clear", "--chat", "Sam", "--pick", "2", "--delete-starred", "--delete-media", "--confirm"},
			seed: seedMgmtContacts,
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.To != "Sam" || req.Pick != 2 || !req.DeleteStarred || !req.DeleteMedia {
					t.Fatalf("clear request = %+v", req)
				}
			},
			calls: []string{"clear-chat " + mgmtOtherPhone + " starred=true media=true"},
			human: "clear: " + mgmtOtherPhone + " (kept 2 local messages as tombstones)\n",
			json:  jsonOK(`{"action":"clear","chat":"` + mgmtOtherPhone + `","messages_hidden":2,"ok":true}`),
		},
		{
			name:  "chats lock",
			kind:  chatLockKind,
			args:  []string{"chats", "lock", "--chat", mgmtGroup},
			calls: []string{"lock " + mgmtGroup + " true"},
			human: "lock: " + mgmtGroup + "\n",
			json:  jsonOK(`{"action":"lock","chat":"` + mgmtGroup + `","ok":true}`),
		},
		{
			name:  "chats unlock",
			kind:  chatUnlockKind,
			args:  []string{"chats", "unlock", "--chat", mgmtGroup},
			calls: []string{"lock " + mgmtGroup + " false"},
			human: "unlock: " + mgmtGroup + "\n",
			json:  jsonOK(`{"action":"unlock","chat":"` + mgmtGroup + `","ok":true}`),
		},
		{
			name: "chats disappearing",
			kind: chatDisappearingKind,
			args: []string{"chats", "disappearing", "--chat", mgmtGroup, "--duration", "7d"},
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.Duration != "7d" {
					t.Fatalf("duration = %q", req.Duration)
				}
			},
			calls: []string{"disappearing " + mgmtGroup + " 168h0m0s"},
			human: "disappearing: " + mgmtGroup + " (7d)\n",
			json:  jsonOK(`{"action":"disappearing","chat":"` + mgmtGroup + `","duration":"7d","ok":true}`),
		},
		{
			name:  "chats disappearing off",
			kind:  chatDisappearingKind,
			args:  []string{"chats", "disappearing", "--chat", mgmtGroup, "--duration", "off"},
			calls: []string{"disappearing " + mgmtGroup + " 0s"},
			human: "disappearing: " + mgmtGroup + " (off)\n",
			json:  jsonOK(`{"action":"disappearing","chat":"` + mgmtGroup + `","duration":"off","ok":true}`),
		},
		{
			name:  "chats favorite",
			kind:  chatFavoriteKind,
			args:  []string{"chats", "favorite", "--chat", mgmtGroup},
			calls: []string{"favorite " + mgmtGroup + " true"},
			human: "favorite: " + mgmtGroup + "\n",
			json:  jsonOK(`{"action":"favorite","chat":"` + mgmtGroup + `","ok":true}`),
		},
		{
			name:  "chats unfavorite",
			kind:  chatUnfavoriteKind,
			args:  []string{"chats", "unfavorite", "--chat", mgmtGroup},
			calls: []string{"favorite " + mgmtGroup + " false"},
			human: "unfavorite: " + mgmtGroup + "\n",
			json:  jsonOK(`{"action":"unfavorite","chat":"` + mgmtGroup + `","ok":true}`),
		},
		{
			name: "chats lists create",
			kind: chatListCreateKind,
			args: []string{"chats", "lists", "create", "Test list"},
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.Name != "Test list" {
					t.Fatalf("name = %q", req.Name)
				}
			},
			calls: []string{`list-create "Test list"`},
			human: "Created list Test list (id 7)\n",
			json:  jsonOK(`{"action":"list-create","list":{"id":"7","name":"Test list"},"ok":true}`),
		},
		{
			name:  "chats lists rename",
			kind:  chatListRenameKind,
			args:  []string{"chats", "lists", "rename", "7", "Renamed list"},
			calls: []string{`list-rename 7 "Renamed list"`},
			human: "Renamed list 7 to Renamed list\n",
			json:  jsonOK(`{"action":"list-rename","list":{"id":"7","name":"Renamed list"},"ok":true}`),
		},
		{
			name:  "chats lists delete",
			kind:  chatListDeleteKind,
			args:  []string{"chats", "lists", "delete", "Test list", "--confirm"},
			calls: []string{"list-delete Test list"},
			human: "Deleted list Test list (id 7)\n",
			json:  jsonOK(`{"action":"list-delete","list":{"id":"7","name":"Test list"},"ok":true}`),
		},
		{
			name: "chats lists add",
			kind: chatListAddKind,
			args: []string{"chats", "lists", "add", "--list", "Test list", "--chat", mgmtGroup},
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.List != "Test list" || req.To != mgmtGroup {
					t.Fatalf("list add request = %+v", req)
				}
			},
			calls: []string{"list-member Test list " + mgmtGroup + " true"},
			human: "Added " + mgmtGroup + " to list Test list (id 7)\n",
			json:  jsonOK(`{"action":"list-add","chat":"` + mgmtGroup + `","list":{"id":"7","name":"Test list"},"ok":true}`),
		},
		{
			name:  "chats lists remove",
			kind:  chatListRemoveKind,
			args:  []string{"chats", "lists", "remove", "--list", "7", "--chat", mgmtGroup},
			calls: []string{"list-member 7 " + mgmtGroup + " false"},
			human: "Removed " + mgmtGroup + " from list Test list (id 7)\n",
			json:  jsonOK(`{"action":"list-remove","chat":"` + mgmtGroup + `","list":{"id":"7","name":"Test list"},"ok":true}`),
		},
		{
			name:  "messages star",
			kind:  messageStarKind,
			args:  []string{"messages", "star", "--chat", mgmtPhone, "--id", "MSG01"},
			seed:  fromMe,
			calls: []string{"star " + mgmtPhone + " MSG01 fromMe=true sender= true"},
			human: "Starred message MSG01 in " + mgmtPhone + "\n",
			json:  jsonOK(`{"action":"star","chat":"` + mgmtPhone + `","ok":true,"target":"MSG01"}`),
		},
		{
			name:  "messages unstar",
			kind:  messageUnstarKind,
			args:  []string{"messages", "unstar", "--chat", mgmtPhone, "--id", "MSG01"},
			seed:  func(t *testing.T, f *fakeManagementApp) { seedMgmtMessage(t, f, false) },
			calls: []string{"star " + mgmtPhone + " MSG01 fromMe=false sender= false"},
			human: "Unstarred message MSG01 in " + mgmtPhone + "\n",
			json:  jsonOK(`{"action":"unstar","chat":"` + mgmtPhone + `","ok":true,"target":"MSG01"}`),
		},
		{
			name: "messages pin",
			kind: messagePinKind,
			args: []string{"messages", "pin", "--chat", mgmtPhone, "--id", "MSG01", "--duration", "24h", "--post-send-wait", "0"},
			seed: fromMe,
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.Chat != mgmtPhone || req.ID != "MSG01" || req.Duration != "24h" {
					t.Fatalf("pin request = %+v", req)
				}
			},
			calls: []string{"send " + mgmtPhone + " pin MSG01 type=PIN_FOR_ALL secs=86400 fromMe=true"},
			human: "Pinned message MSG01 in " + mgmtPhone + " for 24h (id SENT01)\n",
			json:  jsonOK(`{"action":"pin","duration":"24h","id":"SENT01","sent":true,"target":"MSG01","to":"` + mgmtPhone + `"}`),
			after: func(t *testing.T, f *fakeManagementApp) {
				pins, err := f.db.ListPinnedMessages(nil, time.Now().UTC())
				if err != nil || len(pins) != 1 || pins[0].MsgID != "MSG01" || pins[0].ExpiresAt == nil {
					t.Fatalf("stored pins = %+v, %v", pins, err)
				}
			},
		},
		{
			name: "messages unpin",
			kind: messageUnpinKind,
			args: []string{"messages", "unpin", "--chat", mgmtPhone, "--id", "MSG01", "--post-send-wait", "0"},
			seed: func(t *testing.T, f *fakeManagementApp) {
				seedMgmtMessage(t, f, true)
				expires := time.Now().Add(time.Hour)
				if err := f.db.SetMessagePin(store.MessagePin{ChatJID: mgmtPhone, MsgID: "MSG01", Pinned: true, ChangedAt: time.Now().Add(-time.Minute), ExpiresAt: &expires}); err != nil {
					t.Fatal(err)
				}
			},
			calls: []string{"send " + mgmtPhone + " pin MSG01 type=UNPIN_FOR_ALL secs=0 fromMe=true"},
			human: "Unpinned message MSG01 in " + mgmtPhone + " (id SENT01)\n",
			json:  jsonOK(`{"action":"unpin","id":"SENT01","sent":true,"target":"MSG01","to":"` + mgmtPhone + `"}`),
			after: func(t *testing.T, f *fakeManagementApp) {
				pins, err := f.db.ListPinnedMessages(nil, time.Now().UTC())
				if err != nil || len(pins) != 0 {
					t.Fatalf("stored pins after unpin = %+v, %v", pins, err)
				}
			},
		},
		{
			name:  "messages keep",
			kind:  messageKeepKind,
			args:  []string{"messages", "keep", "--chat", mgmtPhone, "--id", "MSG01", "--post-send-wait", "0"},
			seed:  fromMe,
			calls: []string{"send " + mgmtPhone + " keep MSG01 type=KEEP_FOR_ALL"},
			human: "Kept message MSG01 in " + mgmtPhone + " (id SENT01)\n",
			json:  jsonOK(`{"action":"keep","id":"SENT01","sent":true,"target":"MSG01","to":"` + mgmtPhone + `"}`),
		},
		{
			name:  "messages unkeep",
			kind:  messageUnkeepKind,
			args:  []string{"messages", "unkeep", "--chat", mgmtPhone, "--id", "MSG01", "--post-send-wait", "0"},
			seed:  fromMe,
			calls: []string{"send " + mgmtPhone + " keep MSG01 type=UNDO_KEEP_FOR_ALL"},
			human: "Stopped keeping message MSG01 in " + mgmtPhone + " (id SENT01)\n",
			json:  jsonOK(`{"action":"unkeep","id":"SENT01","sent":true,"target":"MSG01","to":"` + mgmtPhone + `"}`),
		},
		{
			name: "send contact",
			kind: sendContactKind,
			args: []string{"send", "contact", "--to", mgmtGroup, "--contact", mgmtOtherPhone, "--name", "Test Person", "--post-send-wait", "0"},
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.To != mgmtGroup || !slices.Equal(req.Contacts, []string{mgmtOtherPhone}) || req.Name != "Test Person" {
					t.Fatalf("contact request = %+v", req)
				}
			},
			calls: []string{"send " + mgmtGroup + ` contacts "Contact: Test Person (+15550000002)"`},
			human: "Sent contact card to " + mgmtGroup + " (id SENT01)\n",
			json:  jsonOK(`{"contacts":1,"id":"SENT01","sent":true,"to":"` + mgmtGroup + `"}`),
			after: func(t *testing.T, f *fakeManagementApp) {
				m, err := f.db.GetMessage(mgmtGroup, "SENT01")
				if err != nil || !m.FromMe || m.Text != "Contact: Test Person (+15550000002)" {
					t.Fatalf("stored contact message = %+v, %v", m, err)
				}
			},
		},
		{
			name:  "send contacts array",
			kind:  sendContactKind,
			args:  []string{"send", "contact", "--to", mgmtGroup, "--contact", mgmtPhone, "--contact", "+1 555 000 0002", "--post-send-wait", "0"},
			seed:  seedMgmtContacts,
			calls: []string{"send " + mgmtGroup + ` contacts "Contacts:\nContact: Sam (+15550000001)\nContact: Samantha (+15550000002)"`},
			human: "Sent 2 contact cards to " + mgmtGroup + " (id SENT01)\n",
			json:  jsonOK(`{"contacts":2,"id":"SENT01","sent":true,"to":"` + mgmtGroup + `"}`),
		},
		{
			name: "send event",
			kind: sendEventKind,
			args: []string{"send", "event", "--to", mgmtGroup, "--name", "Test event", "--description", "Fictional plans",
				"--start", "2026-10-01T18:00:00Z", "--end", "2026-10-01T20:00:00Z", "--location", "Test place", "--post-send-wait", "0"},
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.Name != "Test event" || req.Description != "Fictional plans" || req.Location != "Test place" ||
					req.StartUnix != 1790877600 || req.EndUnix != 1790884800 {
					t.Fatalf("event request = %+v", req)
				}
			},
			calls: []string{"send-event " + mgmtGroup + ` "Test event" start=1790877600 end=1790884800 location="Test place" secret=32`},
			human: "Sent event Test event to " + mgmtGroup + " (id EVT01)\n",
			json:  jsonOK(`{"id":"EVT01","name":"Test event","sent":true,"to":"` + mgmtGroup + `"}`),
			after: func(t *testing.T, f *fakeManagementApp) {
				m, err := f.db.GetMessage(mgmtGroup, "EVT01")
				if err != nil || !strings.HasPrefix(m.Text, "Event: Test event\n") {
					t.Fatalf("stored event = %+v, %v", m, err)
				}
			},
		},
	}
}

// With the store locked by sync --follow, each command runs in that process
// and prints what the direct command prints, with and without --json.
func TestChatsMessagesCommandsDelegateToFollowProcess(t *testing.T) {
	for _, tc := range chatsMessagesCommandCases(t) {
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

// The direct path runs the same core in-process: same calls, same output.
func TestChatsMessagesCoreMatchesDelegatedResult(t *testing.T) {
	for _, tc := range chatsMessagesCommandCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			d := startManagementDaemon(t, tc.seed)
			if _, _, err := d.run(t, true, tc.args); err != nil {
				t.Fatalf("delegated run: %v", err)
			}
			req := d.received()[0]
			delegatedCalls := d.fake.list()

			direct := newTestManagementApp(t)
			if tc.seed != nil {
				tc.seed(t, direct)
			}
			resp, err := runChatsMessagesAction(context.Background(), direct, req, recipientOptions{pick: req.Pick, asJSON: true})
			if err != nil {
				t.Fatalf("direct core: %v", err)
			}
			if got := direct.list(); !slices.Equal(got, delegatedCalls) {
				t.Fatalf("direct calls = %q, delegated calls = %q", got, delegatedCalls)
			}
			if got, want := directJSON(t, chatsMessagesJSON(req.Kind, resp)), tc.json; got != want {
				t.Fatalf("direct JSON = %q\nwant       %q", got, want)
			}
			if got, want := chatsMessagesHuman(req.Kind, resp)+"\n", tc.human; got != want {
				t.Fatalf("direct human = %q\nwant        %q", got, want)
			}
		})
	}
}

// A sync process started before a kind existed rejects it without running it.
func TestChatsMessagesCommandsExplainOlderFollowProcessRejection(t *testing.T) {
	cases := chatsMessagesCommandCases(t)
	cases = append(cases,
		managementCommandCase{name: "send file view once", kind: fileViewOnceKind, args: []string{"send", "file", "--to", mgmtPhone, "--file", "photo.jpg", "--view-once"}},
		managementCommandCase{name: "send voice view once", kind: voiceViewOnceKind, args: []string{"send", "voice", "--to", mgmtPhone, "--file", "note.ogg", "--view-once"}},
	)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skipPresenceDelegateSocketTestOnUnsupportedOS(t)
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

// Read-only mode stops every command before it reaches a sync process.
func TestChatsMessagesCommandsHonorReadOnlyBeforeDelegating(t *testing.T) {
	for _, viaEnv := range []bool{false, true} {
		for _, tc := range chatsMessagesCommandCases(t) {
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

// Destructive chat and list commands need --confirm when nobody can answer a prompt.
func TestDestructiveChatCommandsRequireConfirm(t *testing.T) {
	for _, args := range [][]string{
		{"chats", "delete", "--chat", mgmtPhone},
		{"chats", "clear", "--chat", mgmtPhone},
		{"chats", "lists", "delete", "7"},
	} {
		t.Run(strings.Join(args[:2], " "), func(t *testing.T) {
			d := startManagementDaemon(t, nil)
			stdout, stderr, err := d.run(t, false, args)
			if err == nil || !strings.Contains(stderr, "pass --confirm") || stdout != "" {
				t.Fatalf("err=%v stdout=%q stderr=%q, want a --confirm refusal", err, stdout, stderr)
			}
			if reqs := d.received(); len(reqs) != 0 {
				t.Fatalf("unconfirmed command reached the sync process: %+v", reqs)
			}
		})
	}
}

// The production dispatcher sends every new kind to its executor.
func TestExecuteDelegatedSendRoutesChatsMessagesKinds(t *testing.T) {
	a, err := app.New(app.Options{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	t.Cleanup(a.Close)
	kinds := []string{fileViewOnceKind, voiceViewOnceKind}
	for kind := range chatsMessagesKinds {
		kinds = append(kinds, kind)
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			defer func() { _ = recover() }()
			_, err := executeDelegatedSend(context.Background(), a, sendDelegateRequest{Version: sendDelegateVersion, Kind: kind, TimeoutMS: 200})
			if err != nil && strings.Contains(err.Error(), "unsupported send kind") {
				t.Fatalf("kind %q rejected: %v", kind, err)
			}
		})
	}
	// Unknown neighbours of the new kinds are still refused before anything runs.
	fake := newTestManagementApp(t)
	for _, kind := range []string{"chat_delete_all", "message_pin_all", "contacts"} {
		_, err := executeDelegatedManagement(context.Background(), fake, sendDelegateRequest{Kind: kind, To: mgmtPhone})
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("unsupported send kind %q", kind)) {
			t.Fatalf("kind %q: error = %v", kind, err)
		}
	}
	if calls := fake.list(); len(calls) != 0 {
		t.Fatalf("unknown kinds ran %q", calls)
	}
}

func TestExecuteDelegatedChatsMessagesValidatesBeforeWhatsApp(t *testing.T) {
	fake := newTestManagementApp(t)
	seedMgmtContacts(t, fake)
	seedMgmtMessage(t, fake, false)
	if err := fake.db.MarkMessageDeletedForMe(mgmtPhone, "MSG01", mgmtPhone, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		req  sendDelegateRequest
		want string
	}{
		{"disappearing without duration", sendDelegateRequest{Kind: chatDisappearingKind, To: mgmtPhone}, "--duration is required"},
		{"disappearing with odd duration", sendDelegateRequest{Kind: chatDisappearingKind, To: mgmtPhone, Duration: "3d"}, "invalid --duration"},
		{"disappearing for a channel", sendDelegateRequest{Kind: chatDisappearingKind, To: "120363000000000009@newsletter", Duration: "7d"}, "one-to-one chats and groups"},
		{"pin with odd duration", sendDelegateRequest{Kind: messagePinKind, Chat: mgmtPhone, ID: "MSG01", Duration: "2d"}, "invalid --duration"},
		{"star a deleted message", sendDelegateRequest{Kind: messageStarKind, Chat: mgmtPhone, ID: "MSG01"}, "is deleted"},
		{"pin an unknown message", sendDelegateRequest{Kind: messagePinKind, Chat: mgmtPhone, ID: "MSG404", Duration: "7d"}, "no rows"},
		{"ambiguous chat without --pick", sendDelegateRequest{Kind: chatLockKind, To: "Sam"}, "use --pick N"},
		{"favorites through lists add", sendDelegateRequest{Kind: chatListAddKind, List: "Favorites", To: mgmtPhone}, "chats favorite"},
		{"contact with a group", sendDelegateRequest{Kind: sendContactKind, To: mgmtPhone, Contacts: []string{mgmtGroup}}, "not a person"},
		{"contact name with two contacts", sendDelegateRequest{Kind: sendContactKind, To: mgmtPhone, Contacts: []string{mgmtPhone, mgmtOtherPhone}, Name: "X"}, "single --contact"},
		{"event without name", sendDelegateRequest{Kind: sendEventKind, To: mgmtGroup, StartUnix: 1790877600}, "--name is required"},
		{"event ending before start", sendDelegateRequest{Kind: sendEventKind, To: mgmtGroup, Name: "E", StartUnix: 1790877600, EndUnix: 1790877000}, "--end must be after --start"},
		{"event with foreign join link", sendDelegateRequest{Kind: sendEventKind, To: mgmtGroup, Name: "E", StartUnix: 1790877600, JoinLink: "https://example.com/x"}, "WhatsApp call link"},
		{"event to a channel", sendDelegateRequest{Kind: sendEventKind, To: "120363000000000009@newsletter", Name: "E", StartUnix: 1790877600}, "groups and one-to-one"},
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

func TestSendDelegateRequestPreservesChatsMessagesFieldsInJSON(t *testing.T) {
	req := sendDelegateRequest{
		Version: sendDelegateVersion, Kind: sendEventKind, To: mgmtGroup, Chat: mgmtPhone, ID: "MSG01",
		DeleteStarred: true, DeleteMedia: true, Duration: "7d", List: "Test list",
		Contacts: []string{mgmtPhone, "+1 202 555 0142"}, Name: "Test event", Description: "Fictional plans",
		StartUnix: 1790877600, EndUnix: 1790884800, Location: "Test place", JoinLink: "https://call.whatsapp.com/video/FakeToken01",
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"delete_starred":true`, `"duration":"7d"`, `"list":"Test list"`, `"contacts":["` + mgmtPhone + `","+1 202 555 0142"]`,
		`"description":"Fictional plans"`, `"start_unix":1790877600`, `"end_unix":1790884800`, `"location":"Test place"`, `"join_link":"https://call.whatsapp.com/video/FakeToken01"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("encoded request %s missing %s", raw, key)
		}
	}
	var got sendDelegateRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, req) {
		t.Fatalf("round trip = %+v\nwant %+v", got, req)
	}

	resp := sendDelegateResponse{OK: true, Action: "list-create", ListID: "7", Name: "Test list", Duration: "24h", Count: 3}
	raw, err = json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var gotResp sendDelegateResponse
	if err := json.Unmarshal(raw, &gotResp); err != nil || !reflect.DeepEqual(gotResp, resp) {
		t.Fatalf("response round trip = %+v, %v", gotResp, err)
	}
	// Existing kinds do not grow keys.
	raw, _ = json.Marshal(sendDelegateResponse{OK: true, Sent: true, To: mgmtPhone, ID: "X"})
	if string(raw) != `{"ok":true,"sent":true,"to":"`+mgmtPhone+`","id":"X"}` {
		t.Fatalf("plain response = %s", raw)
	}
}

func TestSendFileViewOnceDelegatesItsOwnKind(t *testing.T) {
	for _, tc := range []struct {
		args []string
		kind string
	}{
		{[]string{"send", "file", "--to", mgmtPhone, "--file", "photo.jpg", "--view-once"}, fileViewOnceKind},
		{[]string{"send", "file", "--to", mgmtPhone, "--file", "photo.jpg"}, "file"},
		{[]string{"send", "voice", "--to", mgmtPhone, "--file", "note.ogg", "--view-once"}, voiceViewOnceKind},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			skipPresenceDelegateSocketTestOnUnsupportedOS(t)
			storeDir := shortPresenceDelegateStoreDir(t)
			lk, err := lock.Acquire(storeDir)
			if err != nil {
				t.Fatalf("lock store: %v", err)
			}
			defer lk.Release()
			server := startPresenceDelegateTestSocket(t, storeDir, func(req sendDelegateRequest) sendDelegateResponse {
				return sendDelegateResponse{OK: true, Sent: true, To: mgmtPhone, ID: "SENT01", File: map[string]string{"name": "photo.jpg", "view_once": "true"}}
			})
			defer server.stop()
			stdout, stderr, err := runPresenceDelegateHelper(t, append([]string{"--store", storeDir, "--json", "--timeout", "2s"}, tc.args...))
			if err != nil {
				t.Fatalf("err=%v stdout=%q stderr=%q", err, stdout, stderr)
			}
			if req := server.nextRequest(t); req.Kind != tc.kind {
				t.Fatalf("kind = %q, want %q", req.Kind, tc.kind)
			}
		})
	}
}

func TestValidateViewOnceTarget(t *testing.T) {
	user := types.NewJID("15550000001", types.DefaultUserServer)
	for _, tc := range []struct {
		to        types.JID
		mediaType string
		ptt       bool
		ok        bool
	}{
		{user, "image", false, true},
		{user, "video", false, true},
		{user, "audio", true, true},
		{user, "audio", false, false},
		{user, "document", false, false},
		{types.StatusBroadcastJID, "image", false, false},
		{types.NewJID("120363000000000009", types.NewsletterServer), "image", false, false},
	} {
		err := validateViewOnceTarget(true, tc.to, tc.mediaType, tc.ptt)
		if (err == nil) != tc.ok {
			t.Fatalf("%s %s ptt=%t: err=%v, want ok=%t", tc.to, tc.mediaType, tc.ptt, err, tc.ok)
		}
	}
	if err := validateViewOnceTarget(false, user, "document", false); err != nil {
		t.Fatalf("plain send refused: %v", err)
	}
}

func TestParseChatAndPinDurations(t *testing.T) {
	for in, want := range map[string]string{"off": "off", "24h": "24h", "1d": "24h", "7d": "7d", "week": "7d", "90d": "90d"} {
		if _, got, err := parseDisappearingDuration(in); err != nil || got != want {
			t.Fatalf("disappearing %q = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, want := range map[string]time.Duration{"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour} {
		if got, _, err := parsePinDuration(in); err != nil || got != want {
			t.Fatalf("pin %q = %s, %v; want %s", in, got, err, want)
		}
	}
	if _, _, err := parsePinDuration("90d"); err == nil {
		t.Fatal("pin accepted 90d")
	}
}

func TestDescribeChatsMessagesProtoIgnoresOtherMessages(t *testing.T) {
	if got := describeChatsMessagesProto(&waProto.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{}}); got != "" {
		t.Fatalf("describe text = %q", got)
	}
}
