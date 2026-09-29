package wa

import (
	"context"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow/types"
)

// RejectCall declines a ringing call. from is the caller as the call offer
// named it, and callID is the offer's call ID.
func (c *Client) RejectCall(ctx context.Context, from types.JID, callID string) error {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return fmt.Errorf("call ID is required")
	}
	if from.Server != types.DefaultUserServer && from.Server != types.HiddenUserServer {
		return fmt.Errorf("the caller must be a user JID, got %s", from)
	}
	cli, err := c.liveWhatsmeowClient()
	if err != nil {
		return err
	}
	return cli.RejectCall(ctx, from, callID)
}
