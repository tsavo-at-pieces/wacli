package wa

import (
	"context"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func (c *Client) liveWhatsmeowClient() (*whatsmeow.Client, error) {
	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()
	if cli == nil || !cli.IsConnected() {
		return nil, fmt.Errorf("not connected")
	}
	return cli, nil
}

// NewsletterToggleMute mutes or unmutes a followed channel for this account.
func (c *Client) NewsletterToggleMute(ctx context.Context, jid types.JID, mute bool) error {
	cli, err := c.liveWhatsmeowClient()
	if err != nil {
		return err
	}
	return cli.NewsletterToggleMute(ctx, jid, mute)
}

// NewsletterSendReaction reacts to a channel post by server ID and returns the
// reaction's own message ID. An empty reaction removes an earlier one.
func (c *Client) NewsletterSendReaction(ctx context.Context, jid types.JID, serverID types.MessageServerID, reaction string) (types.MessageID, error) {
	cli, err := c.liveWhatsmeowClient()
	if err != nil {
		return "", err
	}
	id := cli.GenerateMessageID()
	if err := cli.NewsletterSendReaction(ctx, jid, serverID, reaction, id); err != nil {
		return "", err
	}
	return id, nil
}

// GetNewsletterMessages fetches up to count channel posts, older than before
// when it is set.
func (c *Client) GetNewsletterMessages(ctx context.Context, jid types.JID, count int, before types.MessageServerID) ([]*types.NewsletterMessage, error) {
	cli, err := c.liveWhatsmeowClient()
	if err != nil {
		return nil, err
	}
	return cli.GetNewsletterMessages(ctx, jid, &whatsmeow.GetNewsletterMessagesParams{Count: count, Before: before})
}

// NewsletterMarkViewed counts channel posts as viewed. whatsmeow waits for the
// server's reply without a context, so the wait is bounded here: a missing
// reply must not hold a delegated operation slot forever.
func (c *Client) NewsletterMarkViewed(ctx context.Context, jid types.JID, serverIDs []types.MessageServerID) error {
	cli, err := c.liveWhatsmeowClient()
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cli.NewsletterMarkViewed(ctx, jid, serverIDs) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("mark channel posts viewed: no reply from WhatsApp: %w", ctx.Err())
	}
}

// CreateNewsletter creates a channel owned by this account.
func (c *Client) CreateNewsletter(ctx context.Context, name, description string) (*types.NewsletterMetadata, error) {
	cli, err := c.liveWhatsmeowClient()
	if err != nil {
		return nil, err
	}
	return cli.CreateNewsletter(ctx, whatsmeow.CreateNewsletterParams{Name: name, Description: description})
}

// ParseNewsletterMessage extracts text and media metadata from a fetched
// channel post with the same parser used for synced messages.
func ParseNewsletterMessage(channel types.JID, msg *types.NewsletterMessage) ParsedMessage {
	if msg == nil {
		return ParsedMessage{Chat: channel}
	}
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: channel, Sender: channel},
			ID:            msg.MessageID,
			ServerID:      msg.MessageServerID,
			Timestamp:     msg.Timestamp,
			Type:          strings.TrimSpace(msg.Type),
		},
		Message: msg.Message,
	}
	if evt.Message == nil {
		return ParsedMessage{Chat: channel, ID: msg.MessageID, Timestamp: msg.Timestamp.UTC()}
	}
	return ParseLiveMessage(evt)
}
