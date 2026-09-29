package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/spf13/cobra"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func newMessagesForwardCmd(flags *rootFlags) *cobra.Command {
	var chat string
	var id string
	var to string
	var pick int
	postSendWait := postSendRetryReceiptWait

	cmd := &cobra.Command{
		Use:   "forward",
		Short: "Forward a stored message",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(chat) == "" || strings.TrimSpace(id) == "" || strings.TrimSpace(to) == "" {
				return fmt.Errorf("--chat, --id, and --to are required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}

			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				return delegateAfterOpenFailure(ctx, flags, err, sendDelegateRequest{
					Kind:           messageForwardKind,
					Chat:           chat,
					ID:             id,
					To:             to,
					Pick:           pick,
					PostSendWaitMS: durationMillis(postSendWait),
				}, func(resp sendDelegateResponse) error {
					warnSendStoreFailureMsg(os.Stderr, resp.ID, resp.StoreWarning)
					return writeMessageForwarded(flags, messageForwardResult{
						to:           resp.To,
						id:           resp.ID,
						source:       resp.Target,
						storeWarning: resp.StoreWarning,
					})
				})
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			plan, err := planMessageForward(ctx, a, chat, id, to, recipientOptions{pick: pick, asJSON: flags.asJSON})
			if err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}
			res, err := forwardStoredMessage(ctx, a, plan)
			if err != nil {
				return err
			}
			warnSendStoreFailureMsg(os.Stderr, res.id, res.storeWarning)

			waitForPostSendRetryReceipts(ctx, postSendWait)

			return writeMessageForwarded(flags, res)
		},
	}
	cmd.Flags().StringVar(&chat, "chat", "", "source chat JID or phone number")
	cmd.Flags().StringVar(&id, "id", "", "source message ID to forward")
	cmd.Flags().StringVar(&to, "to", "", "recipient JID, phone number, or contact/group/chat name")
	cmd.Flags().IntVar(&pick, "pick", 0, "when --to is ambiguous, pick the Nth match (1-indexed)")
	cmd.Flags().DurationVar(&postSendWait, "post-send-wait", postSendRetryReceiptWait, "keep the connection alive after forward so retry receipts can be handled (0 disables)")
	return cmd
}

// messageForwardPlan is a checked source message and its resolved recipient.
type messageForwardPlan struct {
	source    store.Message
	mediaInfo *store.MediaDownloadInfo
	to        types.JID
}

// messageForwardResult is what forward reports, directly or through a running
// sync process.
type messageForwardResult struct {
	to           string
	id           string
	source       string
	storeWarning string
}

func planMessageForward(ctx context.Context, a messageTargetApp, chat, id, to string, recipient recipientOptions) (messageForwardPlan, error) {
	source, _, err := loadMessageMutationTarget(ctx, a, chat, id)
	if err != nil {
		return messageForwardPlan{}, err
	}
	if err := validateMessageCanForward(source); err != nil {
		return messageForwardPlan{}, err
	}
	var mediaInfo *store.MediaDownloadInfo
	if strings.TrimSpace(source.MediaType) != "" {
		info, err := a.DB().GetMediaDownloadInfo(source.ChatJID, source.MsgID)
		if err != nil {
			return messageForwardPlan{}, err
		}
		mediaInfo = &info
	}
	toJID, err := resolveRecipient(a, to, recipient)
	if err != nil {
		return messageForwardPlan{}, err
	}
	if mediaInfo != nil && toJID.Server == types.NewsletterServer {
		return messageForwardPlan{}, fmt.Errorf("media forwarding to channels is not supported")
	}
	return messageForwardPlan{source: source, mediaInfo: mediaInfo, to: toJID}, nil
}

// forwardStoredMessage sends the forwarded copy and records it locally. A local
// write failure after delivery is reported as a store warning, not an error.
func forwardStoredMessage(ctx context.Context, a messageMutationApp, plan messageForwardPlan) (messageForwardResult, error) {
	toJID := warmupRecipient(ctx, a.WA(), plan.to, os.Stderr)
	if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
		return messageForwardResult{}, err
	}
	payload, err := buildForwardedMessage(plan.source, plan.mediaInfo)
	if err != nil {
		return messageForwardResult{}, err
	}
	sentID, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (types.MessageID, error) {
		return a.WA().SendProtoMessage(ctx, toJID, payload.Message)
	})
	if err != nil {
		return messageForwardResult{}, err
	}

	now := time.Now().UTC()
	chatName := a.WA().ResolveChatName(ctx, toJID, "")
	var storeErr error
	if err := a.DB().UpsertChat(toJID.String(), chatKindFromJID(toJID), chatName, now); err != nil {
		storeErr = fmt.Errorf("chat update: %w", err)
	}
	if err := a.DB().UpsertMessage(store.UpsertMessageParams{
		ChatJID:         toJID.String(),
		ChatName:        chatName,
		MsgID:           string(sentID),
		SenderName:      "me",
		Timestamp:       now,
		FromMe:          true,
		Text:            payload.Text,
		DisplayText:     payload.Text,
		MediaType:       payload.MediaType,
		MediaCaption:    payload.MediaCaption,
		Filename:        payload.Filename,
		MimeType:        payload.MimeType,
		DirectPath:      payload.DirectPath,
		MediaKey:        payload.MediaKey,
		FileSHA256:      payload.FileSHA256,
		FileEncSHA256:   payload.FileEncSHA256,
		FileLength:      payload.FileLength,
		IsForwarded:     true,
		ForwardingScore: payload.ForwardingScore,
	}); err != nil {
		storeErr = errors.Join(storeErr, fmt.Errorf("message update: %w", err))
	}
	res := messageForwardResult{to: toJID.String(), id: string(sentID), source: plan.source.MsgID}
	if storeErr != nil {
		res.storeWarning = storeErr.Error()
	}
	return res, nil
}

func writeMessageForwarded(flags *rootFlags, res messageForwardResult) error {
	if flags.asJSON {
		body := map[string]any{
			"forwarded": true,
			"to":        res.to,
			"id":        res.id,
			"source":    res.source,
		}
		if res.storeWarning != "" {
			body["store_warning"] = res.storeWarning
		}
		return out.WriteJSON(os.Stdout, body)
	}
	fmt.Fprintf(os.Stdout, "Forwarded message %s to %s (id %s)\n", res.source, res.to, res.id)
	return nil
}

func validateMessageCanForward(msg store.Message) error {
	if msg.Revoked {
		return fmt.Errorf("message %s is deleted", msg.MsgID)
	}
	if msg.DeletedForMe {
		return fmt.Errorf("message %s was deleted for me", msg.MsgID)
	}
	if strings.TrimSpace(msg.ReactionToID) != "" {
		return fmt.Errorf("reaction messages cannot be forwarded")
	}
	mediaType := strings.ToLower(strings.TrimSpace(msg.MediaType))
	if mediaType != "" && !isStoredMediaType(mediaType) {
		return fmt.Errorf("%s messages cannot be forwarded", mediaType)
	}
	if mediaType == "" && strings.TrimSpace(messageForwardText(msg)) == "" {
		return fmt.Errorf("only text messages can be forwarded")
	}
	return nil
}

func messageForwardText(msg store.Message) string {
	if strings.TrimSpace(msg.Text) != "" {
		return msg.Text
	}
	if strings.TrimSpace(msg.DisplayText) != "" {
		return msg.DisplayText
	}
	return ""
}

type forwardedMessagePayload struct {
	Message         *waProto.Message
	Text            string
	MediaType       string
	MediaCaption    string
	Filename        string
	MimeType        string
	DirectPath      string
	MediaKey        []byte
	FileSHA256      []byte
	FileEncSHA256   []byte
	FileLength      uint64
	ForwardingScore uint32
}

func buildForwardedMessage(msg store.Message, mediaInfo *store.MediaDownloadInfo) (forwardedMessagePayload, error) {
	forwardingScore := msg.ForwardingScore + 1
	mediaType := strings.ToLower(strings.TrimSpace(msg.MediaType))
	if mediaType == "" {
		text := messageForwardText(msg)
		return forwardedMessagePayload{
			Message:         buildForwardedTextMessage(text, forwardingScore),
			Text:            text,
			ForwardingScore: forwardingScore,
		}, nil
	}
	if mediaInfo == nil {
		return forwardedMessagePayload{}, fmt.Errorf("message has no media metadata")
	}
	if err := validateStoredMediaInfo(*mediaInfo); err != nil {
		return forwardedMessagePayload{}, err
	}

	payload := forwardedMessagePayload{
		Text:            msg.MediaCaption,
		MediaType:       mediaType,
		MediaCaption:    msg.MediaCaption,
		Filename:        mediaInfo.Filename,
		MimeType:        mediaInfo.MimeType,
		DirectPath:      mediaInfo.DirectPath,
		MediaKey:        append([]byte(nil), mediaInfo.MediaKey...),
		FileSHA256:      append([]byte(nil), mediaInfo.FileSHA256...),
		FileEncSHA256:   append([]byte(nil), mediaInfo.FileEncSHA256...),
		FileLength:      mediaInfo.FileLength,
		ForwardingScore: forwardingScore,
	}
	ctx := forwardedContextInfo(forwardingScore)
	message, err := buildStoredMediaMessage(mediaType, msg.MediaCaption, *mediaInfo, ctx)
	if err != nil {
		return forwardedMessagePayload{}, err
	}
	payload.Message = message
	if mediaType == "document" && strings.TrimSpace(payload.Filename) == "" {
		payload.Filename = "document"
	}
	return payload, nil
}

func buildForwardedTextMessage(text string, forwardingScore uint32) *waProto.Message {
	return &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        proto.String(text),
			ContextInfo: forwardedContextInfo(forwardingScore),
		},
	}
}

func forwardedContextInfo(forwardingScore uint32) *waProto.ContextInfo {
	if forwardingScore == 0 {
		forwardingScore = 1
	}
	return &waProto.ContextInfo{
		IsForwarded:     proto.Bool(true),
		ForwardingScore: proto.Uint32(forwardingScore),
	}
}
