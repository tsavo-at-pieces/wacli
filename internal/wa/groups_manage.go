package wa

import (
	"context"
	"fmt"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// liveGroupsClient returns the live whatsmeow client for the group calls in
// this file, or an error when there is none.
func (c *Client) liveGroupsClient() (*whatsmeow.Client, error) {
	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()
	if cli == nil || !cli.IsConnected() {
		return nil, fmt.Errorf("not connected")
	}
	return cli, nil
}

// SetGroupPhoto sets the group picture from JPEG bytes, or removes it when
// avatar is nil. It returns the new picture ID ("remove" after a removal).
func (c *Client) SetGroupPhoto(ctx context.Context, group types.JID, avatar []byte) (string, error) {
	cli, err := c.liveGroupsClient()
	if err != nil {
		return "", err
	}
	return cli.SetGroupPhoto(ctx, group, avatar)
}

// SetGroupJoinApprovalMode turns admin approval of join requests on or off.
func (c *Client) SetGroupJoinApprovalMode(ctx context.Context, group types.JID, on bool) error {
	cli, err := c.liveGroupsClient()
	if err != nil {
		return err
	}
	return cli.SetGroupJoinApprovalMode(ctx, group, on)
}

// SetGroupMemberAddMode sets who may add participants: admins only or every
// member.
func (c *Client) SetGroupMemberAddMode(ctx context.Context, group types.JID, mode types.GroupMemberAddMode) error {
	cli, err := c.liveGroupsClient()
	if err != nil {
		return err
	}
	return cli.SetGroupMemberAddMode(ctx, group, mode)
}

// GetGroupInfoFromLink previews the group behind an invite link or code
// without joining it.
func (c *Client) GetGroupInfoFromLink(ctx context.Context, code string) (*types.GroupInfo, error) {
	cli, err := c.liveGroupsClient()
	if err != nil {
		return nil, err
	}
	return cli.GetGroupInfoFromLink(ctx, code)
}

// GetSubGroups lists the groups linked to a community.
func (c *Client) GetSubGroups(ctx context.Context, community types.JID) ([]*types.GroupLinkTarget, error) {
	cli, err := c.liveGroupsClient()
	if err != nil {
		return nil, err
	}
	return cli.GetSubGroups(ctx, community)
}

// LinkGroup links an existing group into a community.
func (c *Client) LinkGroup(ctx context.Context, parent, child types.JID) error {
	cli, err := c.liveGroupsClient()
	if err != nil {
		return err
	}
	return cli.LinkGroup(ctx, parent, child)
}

// UnlinkGroup removes a group from a community. The group itself remains.
func (c *Client) UnlinkGroup(ctx context.Context, parent, child types.JID) error {
	cli, err := c.liveGroupsClient()
	if err != nil {
		return err
	}
	return cli.UnlinkGroup(ctx, parent, child)
}

// GetLinkedGroupsParticipants lists the participants across a community's
// linked groups.
func (c *Client) GetLinkedGroupsParticipants(ctx context.Context, community types.JID) ([]types.JID, error) {
	cli, err := c.liveGroupsClient()
	if err != nil {
		return nil, err
	}
	return cli.GetLinkedGroupsParticipants(ctx, community)
}
