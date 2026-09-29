package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
)

const defaultPinDuration = "7d"

func newMessagesStarCmd(flags *rootFlags, star bool) *cobra.Command {
	use, short, kind := "star", "Star a message on all your devices", messageStarKind
	if !star {
		use, short, kind = "unstar", "Unstar a message on all your devices", messageUnstarKind
	}
	var chat, id string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireMessageFlags(chat, id); err != nil {
				return err
			}
			return runChatsMessagesCommand(flags, sendDelegateRequest{Kind: kind, Chat: chat, ID: id})
		},
	}
	cmd.Flags().StringVar(&chat, "chat", "", "chat JID or phone number")
	cmd.Flags().StringVar(&id, "id", "", "message ID")
	return cmd
}

func newMessagesPinCmd(flags *rootFlags, pin bool) *cobra.Command {
	use, short, kind := "pin", "Pin a message for everyone in the chat", messagePinKind
	if !pin {
		use, short, kind = "unpin", "Unpin a message for everyone in the chat", messageUnpinKind
	}
	var chat, id string
	duration := defaultPinDuration
	postSendWait := postSendRetryReceiptWait
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireMessageFlags(chat, id); err != nil {
				return err
			}
			req := sendDelegateRequest{Kind: kind, Chat: chat, ID: id, PostSendWaitMS: durationMillis(postSendWait)}
			if pin {
				if _, _, err := parsePinDuration(duration); err != nil {
					return err
				}
				req.Duration = duration
			}
			return runChatsMessagesCommand(flags, req)
		},
	}
	cmd.Flags().StringVar(&chat, "chat", "", "chat JID or phone number")
	cmd.Flags().StringVar(&id, "id", "", "message ID")
	if pin {
		cmd.Flags().StringVar(&duration, "duration", defaultPinDuration, "how long the pin lasts: 24h, 7d or 30d")
	}
	cmd.Flags().DurationVar(&postSendWait, "post-send-wait", postSendRetryReceiptWait, "keep the connection alive after send so retry receipts can be handled (0 disables)")
	return cmd
}

func newMessagesKeepCmd(flags *rootFlags, keep bool) *cobra.Command {
	use, short, kind := "keep", "Keep a message in a chat with disappearing messages", messageKeepKind
	if !keep {
		use, short, kind = "unkeep", "Stop keeping a message in a chat with disappearing messages", messageUnkeepKind
	}
	var chat, id string
	postSendWait := postSendRetryReceiptWait
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireMessageFlags(chat, id); err != nil {
				return err
			}
			return runChatsMessagesCommand(flags, sendDelegateRequest{Kind: kind, Chat: chat, ID: id, PostSendWaitMS: durationMillis(postSendWait)})
		},
	}
	cmd.Flags().StringVar(&chat, "chat", "", "chat JID or phone number")
	cmd.Flags().StringVar(&id, "id", "", "message ID")
	cmd.Flags().DurationVar(&postSendWait, "post-send-wait", postSendRetryReceiptWait, "keep the connection alive after send so retry receipts can be handled (0 disables)")
	return cmd
}

func newMessagesPinnedCmd(flags *rootFlags) *cobra.Command {
	var chat string
	cmd := &cobra.Command{
		Use:   "pinned",
		Short: "List messages pinned in chats",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, false, false)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			chatJIDs, err := messageChatJIDFilter(ctx, a, chat)
			if err != nil {
				return err
			}
			pins, err := a.DB().ListPinnedMessages(chatJIDs, time.Now().UTC())
			if err != nil {
				return err
			}
			rows := make([]pinnedMessageView, 0, len(pins))
			for _, p := range pins {
				row := pinnedMessageView{MessagePin: p}
				if m, err := a.DB().GetMessage(p.ChatJID, p.MsgID); err == nil {
					row.Message = &m
				}
				rows = append(rows, row)
			}
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, rows)
			}
			fullOutput := fullTableOutput(flags.fullOutput)
			w := newTableWriter(os.Stdout)
			fmt.Fprintln(w, "PINNED\tEXPIRES\tCHAT\tID\tTEXT")
			for _, r := range rows {
				expires := "-"
				if r.ExpiresAt != nil {
					expires = r.ExpiresAt.Local().Format("2006-01-02 15:04")
				}
				text := ""
				if r.Message != nil {
					text = messageText(*r.Message)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.ChangedAt.Local().Format("2006-01-02 15:04"), expires,
					tableCell(r.ChatJID, 28, fullOutput), tableCell(r.MsgID, 14, fullOutput), tableCell(text, 60, fullOutput))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&chat, "chat", "", "filter by chat JID")
	return cmd
}

type pinnedMessageView struct {
	store.MessagePin
	Message *store.Message `json:"message,omitempty"`
}

func requireMessageFlags(chat, id string) error {
	if strings.TrimSpace(chat) == "" || strings.TrimSpace(id) == "" {
		return fmt.Errorf("--chat and --id are required")
	}
	return nil
}

func parsePinDuration(value string) (time.Duration, string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "24h", "1d", "day":
		return 24 * time.Hour, "24h", nil
	case "7d", "168h", "1w", "week":
		return 7 * 24 * time.Hour, "7d", nil
	case "30d", "720h":
		return 30 * 24 * time.Hour, "30d", nil
	default:
		return 0, "", fmt.Errorf("invalid --duration %q (want 24h, 7d or 30d)", value)
	}
}

// loadMessageActionTarget finds a stored message that star, pin and keep can
// address, with the sender its message key needs.
func loadMessageActionTarget(ctx context.Context, a chatsMessagesApp, chat, id string) (store.Message, types.JID, types.JID, error) {
	msg, chatJID, err := loadMessageMutationTarget(ctx, a, chat, id)
	if err != nil {
		return store.Message{}, types.JID{}, types.JID{}, err
	}
	if msg.DeletedAt != nil || msg.Revoked || msg.DeletedForMe {
		return store.Message{}, types.JID{}, types.JID{}, fmt.Errorf("message %s is deleted", msg.MsgID)
	}
	sender, err := messageKeySender(ctx, a, chatJID, msg)
	if err != nil {
		return store.Message{}, types.JID{}, types.JID{}, err
	}
	return msg, chatJID, sender, nil
}

// messageKeySender is the participant a group message's key names. wacli
// stores phone JIDs, so in a LID-addressed group it switches to the LID.
func messageKeySender(ctx context.Context, a chatsMessagesApp, chat types.JID, msg store.Message) (types.JID, error) {
	if msg.FromMe || chat.Server != types.GroupServer {
		return types.EmptyJID, nil
	}
	sender, err := types.ParseJID(strings.TrimSpace(msg.SenderJID))
	if err != nil || sender.IsEmpty() {
		return types.EmptyJID, fmt.Errorf("stored sender JID is required to address a group message someone else sent")
	}
	if sender.Server == types.DefaultUserServer {
		if info, err := a.WA().GetGroupInfo(ctx, chat); err == nil && info != nil && info.AddressingMode == types.AddressingModeLID {
			if lid := a.WA().ResolvePNToLID(ctx, sender); lid.Server == types.HiddenUserServer {
				sender = lid
			}
		}
	}
	return sender, nil
}

func runMessageStar(ctx context.Context, a chatsMessagesApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	msg, chatJID, sender, err := loadMessageActionTarget(ctx, a, req.Chat, req.ID)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	ref := app.MessageRef{Chat: chatJID, ID: msg.MsgID, FromMe: msg.FromMe, Sender: sender}
	if err := a.StarMessage(ctx, ref, req.Kind == messageStarKind); err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Chat: chatJID.String(), Target: msg.MsgID, Action: actionName(req.Kind)}, nil
}

func runMessagePinOrKeep(ctx context.Context, a chatsMessagesApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	var duration time.Duration
	var durationLabel string
	if req.Kind == messagePinKind {
		var err error
		if duration, durationLabel, err = parsePinDuration(req.Duration); err != nil {
			return sendDelegateResponse{}, err
		}
	}
	msg, chatJID, sender, err := loadMessageActionTarget(ctx, a, req.Chat, req.ID)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	key := wa.MessageKey(chatJID, msg.MsgID, msg.FromMe, sender)
	now := time.Now().UTC()
	var message *waProto.Message
	switch req.Kind {
	case messagePinKind, messageUnpinKind:
		message = wa.BuildPinInChatMessage(key, req.Kind == messagePinKind, duration, now)
	default:
		message = wa.BuildKeepInChatMessage(key, req.Kind == messageKeepKind, now)
	}
	if err := warnRapidSendIfNeeded(a.StoreDir(), now, os.Stderr); err != nil {
		return sendDelegateResponse{}, err
	}
	sentID, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (types.MessageID, error) {
		return a.WA().SendProtoMessage(ctx, chatJID, message)
	})
	if err != nil {
		return sendDelegateResponse{}, err
	}
	resp := sendDelegateResponse{OK: true, Sent: true, To: chatJID.String(), ID: string(sentID), Target: msg.MsgID, Action: actionName(req.Kind), Duration: durationLabel}
	if req.Kind == messagePinKind || req.Kind == messageUnpinKind {
		pin := store.MessagePin{ChatJID: msg.ChatJID, MsgID: msg.MsgID, Pinned: req.Kind == messagePinKind, ChangedAt: now}
		if pin.Pinned {
			expires := now.Add(duration)
			pin.ExpiresAt = &expires
		}
		if err := a.DB().SetMessagePin(pin); err != nil {
			resp.StoreWarning = fmt.Sprintf("pin state: %v", err)
		}
	}
	waitForPostSendRetryReceipts(ctx, millisDuration(req.PostSendWaitMS, 0))
	return resp, nil
}
