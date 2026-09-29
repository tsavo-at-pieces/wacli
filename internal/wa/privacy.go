package wa

import (
	"context"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func (c *Client) connectedSession() (*whatsmeow.Client, error) {
	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()
	if cli == nil || !cli.IsConnected() {
		return nil, fmt.Errorf("not connected")
	}
	return cli, nil
}

// GetPrivacySettings fetches the account's privacy settings from WhatsApp,
// bypassing whatsmeow's cache so a change made on the phone shows up.
func (c *Client) GetPrivacySettings(ctx context.Context) (types.PrivacySettings, error) {
	cli, err := c.connectedSession()
	if err != nil {
		return types.PrivacySettings{}, err
	}
	settings, err := cli.TryFetchPrivacySettings(ctx, true)
	if err != nil {
		return types.PrivacySettings{}, err
	}
	return *settings, nil
}

// SetPrivacySetting changes one privacy setting and returns the settings as
// they are after the change.
func (c *Client) SetPrivacySetting(ctx context.Context, name types.PrivacySettingType, value types.PrivacySetting) (types.PrivacySettings, error) {
	cli, err := c.connectedSession()
	if err != nil {
		return types.PrivacySettings{}, err
	}
	settings, err := cli.SetPrivacySetting(ctx, name, value)
	if err != nil {
		return types.PrivacySettings{}, err
	}
	// whatsmeow only folds the older categories back into its result.
	switch name {
	case types.PrivacySettingTypeMessages:
		settings.Messages = value
	case types.PrivacySettingTypeDefense:
		settings.Defense = value
	case types.PrivacySettingTypeStickers:
		settings.Stickers = value
	}
	return settings, nil
}

// SetDefaultDisappearingTimer sets the timer new chats start with; zero turns
// it off.
func (c *Client) SetDefaultDisappearingTimer(ctx context.Context, timer time.Duration) error {
	cli, err := c.connectedSession()
	if err != nil {
		return err
	}
	return cli.SetDefaultDisappearingTimer(ctx, timer)
}

// GetStatusPrivacy fetches who status updates are shared with. The default
// list comes first.
func (c *Client) GetStatusPrivacy(ctx context.Context) ([]types.StatusPrivacy, error) {
	cli, err := c.connectedSession()
	if err != nil {
		return nil, err
	}
	return cli.GetStatusPrivacy(ctx)
}

func (c *Client) GetBlocklist(ctx context.Context) (*types.Blocklist, error) {
	cli, err := c.connectedSession()
	if err != nil {
		return nil, err
	}
	return cli.GetBlocklist(ctx)
}

func (c *Client) UpdateBlocklist(ctx context.Context, jid types.JID, action events.BlocklistChangeAction) (*types.Blocklist, error) {
	cli, err := c.connectedSession()
	if err != nil {
		return nil, err
	}
	return cli.UpdateBlocklist(ctx, jid, action)
}
