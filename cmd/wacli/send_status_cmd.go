package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/spf13/cobra"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type statusTextOptions struct {
	BackgroundColor string
	Font            *int32
}

type statusMessageSender interface {
	SendProtoMessage(ctx context.Context, to types.JID, msg *waProto.Message) (types.MessageID, error)
}

func newSendStatusCmd(flags *rootFlags) *cobra.Command {
	var message string
	var backgroundColor string
	var font int32
	var filePath string
	var mimeOverride string
	var postSendWait = postSendRetryReceiptWait

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Send a status broadcast",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := statusSendOptions{
				message:         message,
				file:            strings.TrimSpace(filePath),
				mimeOverride:    mimeOverride,
				backgroundColor: backgroundColor,
			}
			if cmd.Flags().Changed("font") {
				opts.font = &font
			}
			if strings.TrimSpace(opts.message) == "" && opts.file == "" {
				return fmt.Errorf("--message or --file is required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			if opts.file != "" {
				if err := checkOutboundMediaPath(opts.file); err != nil {
					return err
				}
			} else if _, err := buildStatusTextMessage(opts.message, statusTextOptions{BackgroundColor: opts.backgroundColor, Font: opts.font}); err != nil {
				return err
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				delegated := opts.delegateRequest()
				delegated.PostSendWaitMS = durationMillis(postSendWait)
				return delegateAfterOpenFailure(ctx, flags, err, delegated, func(resp sendDelegateResponse) error {
					warnSendStoreFailureMsg(os.Stderr, resp.ID, resp.StoreWarning)
					return writeStatusSent(flags, statusSendResultFromDelegate(resp))
				})
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}
			res, err := sendStatusUpdate(ctx, a, opts)
			if err != nil {
				return err
			}
			warnSendStoreFailure(os.Stderr, res.id, res.storeWarning)
			waitForPostSendRetryReceipts(ctx, postSendWait)
			return writeStatusSent(flags, res)
		},
	}
	cmd.Flags().StringVar(&message, "message", "", "status text or media caption")
	cmd.Flags().StringVar(&filePath, "file", "", "media file to send as a status update")
	cmd.Flags().StringVar(&mimeOverride, "mime", "", "override detected MIME type for --file")
	cmd.Flags().StringVar(&backgroundColor, "background-color", "", "text status background color (#RRGGBB or #AARRGGBB)")
	cmd.Flags().Int32Var(&font, "font", 0, "WhatsApp text status font number")
	cmd.Flags().DurationVar(&postSendWait, "post-send-wait", postSendRetryReceiptWait, "keep the connection alive after send so retry receipts can be handled (0 disables)")
	return cmd
}

// statusSendOptions is one status update: text with optional styling, or a
// media file with --message as its caption.
type statusSendOptions struct {
	message         string
	file            string
	mimeOverride    string
	backgroundColor string
	font            *int32
}

// delegateRequest carries the options to a running sync process. A relative
// file path is made absolute, because that process has its own directory.
func (o statusSendOptions) delegateRequest() sendDelegateRequest {
	file := o.file
	if file != "" {
		if abs, err := filepath.Abs(file); err == nil {
			file = abs
		}
	}
	return sendDelegateRequest{
		Kind:            statusSendKind,
		Message:         o.message,
		File:            file,
		MIME:            o.mimeOverride,
		BackgroundColor: o.backgroundColor,
		Font:            o.font,
	}
}

type statusSendResult struct {
	id           string
	media        string
	mimeType     string
	storeWarning error
}

func statusSendResultFromDelegate(resp sendDelegateResponse) statusSendResult {
	res := statusSendResult{id: resp.ID, media: resp.File["media"], mimeType: resp.File["mime_type"]}
	if resp.StoreWarning != "" {
		res.storeWarning = errors.New(resp.StoreWarning)
	}
	return res
}

// statusSendApp is a connected WhatsApp client with its store: *app.App, or a
// test fake.
type statusSendApp interface {
	waStoreApp
	StoreDir() string
	Connect(context.Context, bool, func(string)) error
}

// sendStatusUpdate posts one status update and records it in status_messages.
// A failure to record it after delivery is returned as storeWarning.
func sendStatusUpdate(ctx context.Context, a statusSendApp, opts statusSendOptions) (statusSendResult, error) {
	if strings.TrimSpace(opts.message) == "" && opts.file == "" {
		return statusSendResult{}, fmt.Errorf("--message or --file is required")
	}
	var msg *waProto.Message
	if opts.file == "" {
		built, err := buildStatusTextMessage(opts.message, statusTextOptions{BackgroundColor: opts.backgroundColor, Font: opts.font})
		if err != nil {
			return statusSendResult{}, err
		}
		msg = built
	}
	if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
		return statusSendResult{}, err
	}
	if opts.file != "" {
		res, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (sendFileOutcome, error) {
			return sendFile(ctx, a, types.StatusBroadcastJID, opts.file, sendFileOptions{
				caption:      opts.message,
				mimeOverride: opts.mimeOverride,
			})
		})
		if err != nil {
			return statusSendResult{}, err
		}
		return statusSendResult{id: res.id, media: res.meta["media"], mimeType: res.meta["mime_type"], storeWarning: res.storeWarning}, nil
	}

	msgID, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (types.MessageID, error) {
		return sendStatusTextMessage(ctx, a.WA(), msg)
	})
	if err != nil {
		return statusSendResult{}, err
	}
	var storedFont int32
	if opts.font != nil {
		storedFont = *opts.font
	}
	storeErr := a.DB().UpsertStatusMessage(store.UpsertStatusMessageParams{
		MsgID:           string(msgID),
		Timestamp:       time.Now().UTC(),
		FromMe:          true,
		Text:            opts.message,
		BackgroundColor: opts.backgroundColor,
		Font:            storedFont,
	})
	return statusSendResult{id: string(msgID), storeWarning: storeErr}, nil
}

// writeStatusSent prints a sent status, direct or delegated.
func writeStatusSent(flags *rootFlags, res statusSendResult) error {
	if flags.asJSON {
		body := map[string]any{
			"sent": true,
			"to":   types.StatusBroadcastJID.String(),
			"id":   res.id,
		}
		if res.media != "" {
			body["media"] = res.media
			body["mime_type"] = res.mimeType
		}
		return out.WriteJSON(os.Stdout, addStoreWarning(body, res.storeWarning))
	}
	fmt.Fprintf(os.Stdout, "Sent status (id %s)\n", res.id)
	return nil
}

func buildStatusTextMessage(text string, opts statusTextOptions) (*waProto.Message, error) {
	ext := &waProto.ExtendedTextMessage{Text: proto.String(text)}
	if strings.TrimSpace(opts.BackgroundColor) != "" {
		color, err := parseStatusBackgroundColor(opts.BackgroundColor)
		if err != nil {
			return nil, err
		}
		ext.BackgroundArgb = proto.Uint32(color)
	}
	if opts.Font != nil {
		ext.Font = waProto.ExtendedTextMessage_FontType(*opts.Font).Enum()
	}
	return &waProto.Message{ExtendedTextMessage: ext}, nil
}

func parseStatusBackgroundColor(s string) (uint32, error) {
	s = strings.TrimSpace(strings.TrimPrefix(s, "#"))
	if len(s) == 6 {
		s = "ff" + s
	}
	if len(s) != 8 {
		return 0, fmt.Errorf("--background-color must be #RRGGBB or #AARRGGBB")
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid --background-color: %w", err)
	}
	return uint32(v), nil
}

func sendStatusTextMessage(ctx context.Context, sender statusMessageSender, msg *waProto.Message) (types.MessageID, error) {
	return sender.SendProtoMessage(ctx, types.StatusBroadcastJID, msg)
}
