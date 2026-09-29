package wa

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// PinChange is a pin-in-chat or unpin message: someone pinned or unpinned
// TargetID for everyone in the chat.
type PinChange struct {
	TargetID     string
	TargetSender string
	TargetFromMe bool
	Pinned       bool
	// Duration is how long the pin lasts, when the sender said; zero otherwise.
	Duration  time.Duration
	ChangedAt time.Time
}

// KeepChange is a keep-in-chat or undo-keep message in a chat with
// disappearing messages.
type KeepChange struct {
	TargetID string
	Kept     bool
}

// MessageKey addresses a stored message for pin, keep and similar protocol
// messages. Group messages sent by someone else name their sender.
func MessageKey(chat types.JID, id string, fromMe bool, sender types.JID) *waCommon.MessageKey {
	key := &waCommon.MessageKey{
		RemoteJID: proto.String(chat.String()),
		FromMe:    proto.Bool(fromMe),
		ID:        proto.String(id),
	}
	if !fromMe && chat.Server == types.GroupServer && !sender.IsEmpty() {
		key.Participant = proto.String(sender.String())
	}
	return key
}

// BuildPinInChatMessage pins or unpins a message for everyone in the chat.
// A pin lasts duration (WhatsApp offers 24 hours, 7 days and 30 days).
func BuildPinInChatMessage(key *waCommon.MessageKey, pin bool, duration time.Duration, now time.Time) *waE2E.Message {
	pinType := waE2E.PinInChatMessage_UNPIN_FOR_ALL
	seconds := uint32(0)
	if pin {
		pinType = waE2E.PinInChatMessage_PIN_FOR_ALL
		seconds = uint32(duration / time.Second)
	}
	return &waE2E.Message{
		PinInChatMessage: &waE2E.PinInChatMessage{
			Key:               key,
			Type:              pinType.Enum(),
			SenderTimestampMS: proto.Int64(now.UnixMilli()),
		},
		MessageContextInfo: &waE2E.MessageContextInfo{
			MessageAddOnDurationInSecs: proto.Uint32(seconds),
		},
	}
}

// BuildKeepInChatMessage keeps a message from disappearing, or undoes that.
func BuildKeepInChatMessage(key *waCommon.MessageKey, keep bool, now time.Time) *waE2E.Message {
	keepType := waE2E.KeepType_UNDO_KEEP_FOR_ALL
	if keep {
		keepType = waE2E.KeepType_KEEP_FOR_ALL
	}
	return &waE2E.Message{
		KeepInChatMessage: &waE2E.KeepInChatMessage{
			Key:         key,
			KeepType:    keepType.Enum(),
			TimestampMS: proto.Int64(now.UnixMilli()),
		},
	}
}

// ContactCard is one shared contact.
type ContactCard struct {
	Name string
	// Phone is the number in international form without "+" or spaces.
	Phone string
}

// BuildVCard renders a contact the way WhatsApp's apps do, with the WhatsApp
// ID on the phone line so recipients get a "Message" button.
func BuildVCard(card ContactCard) string {
	name := escapeVCardValue(card.Name)
	var b strings.Builder
	b.WriteString("BEGIN:VCARD\n")
	b.WriteString("VERSION:3.0\n")
	b.WriteString("N:;" + name + ";;;\n")
	b.WriteString("FN:" + name + "\n")
	if phone := strings.TrimSpace(card.Phone); phone != "" {
		fmt.Fprintf(&b, "TEL;type=CELL;type=VOICE;waid=%s:+%s\n", phone, phone)
	}
	b.WriteString("END:VCARD")
	return b.String()
}

// BuildContactsMessage shares one contact card, or several as a contacts array.
func BuildContactsMessage(cards []ContactCard) (*waE2E.Message, error) {
	switch len(cards) {
	case 0:
		return nil, fmt.Errorf("at least one contact is required")
	case 1:
		return &waE2E.Message{ContactMessage: &waE2E.ContactMessage{
			DisplayName: proto.String(cards[0].Name),
			Vcard:       proto.String(BuildVCard(cards[0])),
		}}, nil
	}
	contacts := make([]*waE2E.ContactMessage, 0, len(cards))
	for _, card := range cards {
		contacts = append(contacts, &waE2E.ContactMessage{
			DisplayName: proto.String(card.Name),
			Vcard:       proto.String(BuildVCard(card)),
		})
	}
	return &waE2E.Message{ContactsArrayMessage: &waE2E.ContactsArrayMessage{
		DisplayName: proto.String(fmt.Sprintf("%d contacts", len(cards))),
		Contacts:    contacts,
	}}, nil
}

// ContactMessageText is the searchable text wacli stores for a contact card,
// the same text sync stores when one arrives.
func ContactMessageText(msg *waProto.Message) string {
	var pm ParsedMessage
	extractContactText(msg, &pm)
	return pm.Text
}

// EventDetails describes a WhatsApp event invitation.
type EventDetails struct {
	Name        string
	Description string
	Start       time.Time
	End         time.Time // zero: no end time
	Location    string
	JoinLink    string
}

// BuildEventMessage creates an event. The random message secret is what RSVP
// responses are encrypted with; whatsmeow keeps it when the event is sent.
func BuildEventMessage(d EventDetails) *waE2E.Message {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	ev := &waE2E.EventMessage{
		Name:           proto.String(d.Name),
		StartTime:      proto.Int64(d.Start.Unix()),
		IsCanceled:     proto.Bool(false),
		IsScheduleCall: proto.Bool(false),
	}
	if desc := strings.TrimSpace(d.Description); desc != "" {
		ev.Description = proto.String(desc)
	}
	if !d.End.IsZero() {
		ev.EndTime = proto.Int64(d.End.Unix())
	}
	if loc := strings.TrimSpace(d.Location); loc != "" {
		ev.Location = &waE2E.LocationMessage{Name: proto.String(loc)}
	}
	if link := strings.TrimSpace(d.JoinLink); link != "" {
		ev.JoinLink = proto.String(link)
	}
	return &waE2E.Message{
		EventMessage:       ev,
		MessageContextInfo: &waE2E.MessageContextInfo{MessageSecret: secret},
	}
}

// EventMessageText is the searchable text for an event.
func EventMessageText(ev *waE2E.EventMessage) string {
	if ev == nil {
		return ""
	}
	lines := []string{"Event: " + strings.TrimSpace(ev.GetName())}
	if ev.GetIsCanceled() {
		lines[0] += " (canceled)"
	}
	if start := ev.GetStartTime(); start > 0 {
		when := "Starts: " + time.Unix(start, 0).UTC().Format(time.RFC3339)
		if end := ev.GetEndTime(); end > 0 {
			when += ", ends: " + time.Unix(end, 0).UTC().Format(time.RFC3339)
		}
		lines = append(lines, when)
	}
	if loc := strings.TrimSpace(ev.GetLocation().GetName()); loc != "" {
		lines = append(lines, "Location: "+loc)
	}
	if desc := strings.TrimSpace(ev.GetDescription()); desc != "" {
		lines = append(lines, desc)
	}
	if link := strings.TrimSpace(ev.GetJoinLink()); link != "" {
		lines = append(lines, "Call link: "+link)
	}
	return strings.Join(lines, "\n")
}

// SendEventMessage sends an event with the creation meta node WhatsApp's own
// clients attach. The stanza must be typed "event"; the server rejects a
// "text" one with error 479 (go.mod replaces whatsmeow with a build that sets
// the type until upstream does).
func (c *Client) SendEventMessage(ctx context.Context, to types.JID, msg *waProto.Message) (types.MessageID, error) {
	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()
	if cli == nil || !cli.IsConnected() {
		return "", fmt.Errorf("not connected")
	}
	nodes := []waBinary.Node{{Tag: "meta", Attrs: waBinary.Attrs{"event_type": "creation"}}}
	resp, err := cli.SendMessage(ctx, to, msg, whatsmeow.SendRequestExtra{AdditionalNodes: &nodes})
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// WrapViewOnce turns an image, video or voice-note message into a view-once
// message the way WhatsApp's own clients send one. Other payloads are refused
// because WhatsApp only offers view once for photos, videos and voice notes.
func WrapViewOnce(msg *waProto.Message) (*waProto.Message, error) {
	switch {
	case msg.GetImageMessage() != nil:
		msg.ImageMessage.ViewOnce = proto.Bool(true)
		return &waProto.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: msg}}, nil
	case msg.GetVideoMessage() != nil:
		msg.VideoMessage.ViewOnce = proto.Bool(true)
		return &waProto.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: msg}}, nil
	case msg.GetAudioMessage() != nil && msg.GetAudioMessage().GetPTT():
		msg.AudioMessage.ViewOnce = proto.Bool(true)
		return &waProto.Message{ViewOnceMessageV2Extension: &waE2E.FutureProofMessage{Message: msg}}, nil
	default:
		return nil, fmt.Errorf("view once only works for images, videos and voice notes")
	}
}

// extractChatActions reads pin, keep and event payloads, which otherwise
// arrive as content-free placeholders.
func extractChatActions(m *waProto.Message, pm *ParsedMessage) {
	if pin := m.GetPinInChatMessage(); pin != nil && strings.TrimSpace(pin.GetKey().GetID()) != "" {
		change := &PinChange{
			TargetID:     strings.TrimSpace(pin.GetKey().GetID()),
			TargetSender: strings.TrimSpace(pin.GetKey().GetParticipant()),
			TargetFromMe: pin.GetKey().GetFromMe(),
			Pinned:       pin.GetType() == waE2E.PinInChatMessage_PIN_FOR_ALL,
		}
		if ms := pin.GetSenderTimestampMS(); ms > 0 {
			change.ChangedAt = time.UnixMilli(ms).UTC()
		}
		if secs := m.GetMessageContextInfo().GetMessageAddOnDurationInSecs(); change.Pinned && secs > 0 {
			change.Duration = time.Duration(secs) * time.Second
		}
		pm.Pin = change
	}
	if keep := m.GetKeepInChatMessage(); keep != nil && strings.TrimSpace(keep.GetKey().GetID()) != "" {
		pm.Keep = &KeepChange{
			TargetID: strings.TrimSpace(keep.GetKey().GetID()),
			Kept:     keep.GetKeepType() == waE2E.KeepType_KEEP_FOR_ALL,
		}
	}
	if ev := m.GetEventMessage(); ev != nil && strings.TrimSpace(ev.GetName()) != "" {
		pm.Text = EventMessageText(ev)
	}
}

func escapeVCardValue(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `,`, `\,`, `;`, `\;`, "\r\n", `\n`, "\n", `\n`)
	return replacer.Replace(strings.TrimSpace(value))
}
