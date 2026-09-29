package wa

import (
	"context"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

// Every group management wrapper refuses to run without a live connection
// instead of dereferencing a missing client.
func TestGroupManagementRequiresConnection(t *testing.T) {
	var c Client
	ctx := context.Background()
	group := types.NewJID("120363000000000001", types.GroupServer)
	parent := types.NewJID("120363000000000009", types.GroupServer)

	calls := map[string]func() error{
		"SetGroupTopic": func() error { return c.SetGroupTopic(ctx, group, "TOPIC01", "Fictional topic") },
		"SetGroupPhoto": func() error { _, err := c.SetGroupPhoto(ctx, group, []byte{0xff, 0xd8}); return err },
		"SetGroupJoinApprovalMode": func() error {
			return c.SetGroupJoinApprovalMode(ctx, group, true)
		},
		"SetGroupMemberAddMode": func() error {
			return c.SetGroupMemberAddMode(ctx, group, types.GroupMemberAddModeAdmin)
		},
		"GetGroupInfoFromLink": func() error { _, err := c.GetGroupInfoFromLink(ctx, "FakeInviteCode01"); return err },
		"GetSubGroups":         func() error { _, err := c.GetSubGroups(ctx, parent); return err },
		"LinkGroup":            func() error { return c.LinkGroup(ctx, parent, group) },
		"UnlinkGroup":          func() error { return c.UnlinkGroup(ctx, parent, group) },
		"GetLinkedGroupsParticipants": func() error {
			_, err := c.GetLinkedGroupsParticipants(ctx, parent)
			return err
		},
	}
	for name, call := range calls {
		if err := call(); err == nil || !strings.Contains(err.Error(), "not connected") {
			t.Fatalf("%s without a client = %v, want not connected", name, err)
		}
	}
}
