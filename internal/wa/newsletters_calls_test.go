package wa

import (
	"context"
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestChannelAndCallWrappersRequireConnection(t *testing.T) {
	c := &Client{}
	ctx := context.Background()
	channel := types.NewJID("120363000000000001", types.NewsletterServer)
	caller := types.NewJID("15550000001", types.DefaultUserServer)
	checks := map[string]error{}
	checks["channel mute"] = c.NewsletterToggleMute(ctx, channel, true)
	_, checks["channel react"] = c.NewsletterSendReaction(ctx, channel, 7, "x")
	_, checks["channel messages"] = c.GetNewsletterMessages(ctx, channel, 5, 0)
	checks["channel viewed"] = c.NewsletterMarkViewed(ctx, channel, []types.MessageServerID{7})
	_, checks["channel create"] = c.CreateNewsletter(ctx, "Test channel", "")
	checks["call reject"] = c.RejectCall(ctx, caller, "CALL01")
	for name, err := range checks {
		if err == nil || !strings.Contains(err.Error(), "not connected") {
			t.Fatalf("%s error = %v, want not connected", name, err)
		}
	}
}

func TestRejectCallValidatesBeforeConnecting(t *testing.T) {
	c := &Client{}
	ctx := context.Background()
	if err := c.RejectCall(ctx, types.NewJID("15550000001", types.DefaultUserServer), "  "); err == nil || !strings.Contains(err.Error(), "call ID") {
		t.Fatalf("empty call ID error = %v", err)
	}
	if err := c.RejectCall(ctx, types.NewJID("120363000000000001", types.GroupServer), "CALL01"); err == nil || !strings.Contains(err.Error(), "user JID") {
		t.Fatalf("group caller error = %v", err)
	}
}

func TestParseNewsletterMessage(t *testing.T) {
	channel := types.NewJID("120363000000000001", types.NewsletterServer)
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	text := ParseNewsletterMessage(channel, &types.NewsletterMessage{
		MessageServerID: 101,
		MessageID:       "POST01",
		Type:            "text",
		Timestamp:       ts,
		Message:         &waProto.Message{Conversation: proto.String("Fictional update")},
	})
	if text.Chat != channel || text.ID != "POST01" || text.Text != "Fictional update" || !text.Timestamp.Equal(ts) || text.Media != nil {
		t.Fatalf("text post = %+v", text)
	}

	image := ParseNewsletterMessage(channel, &types.NewsletterMessage{
		MessageServerID: 102,
		MessageID:       "POST02",
		Type:            "media",
		Timestamp:       ts,
		Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{
			Caption:  proto.String("Fictional caption"),
			Mimetype: proto.String("image/jpeg"),
		}},
	})
	if image.Media == nil || image.Media.Type != "image" || image.Media.Caption != "Fictional caption" || image.Media.MimeType != "image/jpeg" {
		t.Fatalf("image post = %+v media=%+v", image, image.Media)
	}

	// Live update entries carry no message body.
	bare := ParseNewsletterMessage(channel, &types.NewsletterMessage{MessageServerID: 103, MessageID: "POST03", Timestamp: ts})
	if bare.ID != "POST03" || bare.Text != "" || bare.Media != nil {
		t.Fatalf("bare post = %+v", bare)
	}
	if empty := ParseNewsletterMessage(channel, nil); empty.Chat != channel || empty.ID != "" {
		t.Fatalf("nil post = %+v", empty)
	}
}
