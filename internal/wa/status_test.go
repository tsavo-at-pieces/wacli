package wa

import (
	"context"
	"slices"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
)

func TestBuildUserStatusMutePatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target types.JID
		mute   bool
		index  string
	}{
		{"mute phone", types.NewJID("15550000001", types.DefaultUserServer), true, "15550000001@s.whatsapp.net"},
		{"unmute lid device", types.JID{User: "100000000001", Device: 3, Server: types.HiddenUserServer}, false, "100000000001@lid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patch := buildUserStatusMutePatch(tc.target, tc.mute)
			if patch.Type != appstate.WAPatchRegularHigh {
				t.Fatalf("patch type = %q, want regular_high", patch.Type)
			}
			if len(patch.Mutations) != 1 {
				t.Fatalf("mutations = %d, want 1", len(patch.Mutations))
			}
			mut := patch.Mutations[0]
			if want := []string{appstate.IndexUserStatusMute, tc.index}; !slices.Equal(mut.Index, want) {
				t.Fatalf("index = %q, want %q", mut.Index, want)
			}
			if mut.Index[0] != "userStatusMute" {
				t.Fatalf("index name = %q", mut.Index[0])
			}
			if mut.Version != 7 {
				t.Fatalf("version = %d, want 7 (WAWebUserStatusMuteSync)", mut.Version)
			}
			action := mut.Value.GetUserStatusMuteAction()
			if action == nil || action.Muted == nil || action.GetMuted() != tc.mute {
				t.Fatalf("action = %+v, want muted=%t", action, tc.mute)
			}
		})
	}
}

func TestMuteUserStatusRejectsNonUserTargetsBeforeSending(t *testing.T) {
	c := &Client{}
	for _, jid := range []types.JID{
		types.NewJID("120363000000000001", types.GroupServer),
		types.NewJID("120363000000000001", types.NewsletterServer),
		types.StatusBroadcastJID,
	} {
		called := false
		_, err := c.MuteUserStatus(context.Background(), jid, true, func() { called = true })
		if err == nil || !strings.Contains(err.Error(), "user JID") {
			t.Fatalf("MuteUserStatus(%s) error = %v", jid, err)
		}
		if called {
			t.Fatalf("MuteUserStatus(%s) reserved an apply boundary", jid)
		}
	}
}

func TestGetStatusPrivacyRequiresConnection(t *testing.T) {
	if _, err := (&Client{}).GetStatusPrivacy(context.Background()); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("GetStatusPrivacy error = %v, want not connected", err)
	}
}
