package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

const eventCallLinkHost = "call.whatsapp.com"

func newSendEventCmd(flags *rootFlags) *cobra.Command {
	var to string
	var pick int
	var name, description, start, end, location, joinLink string
	postSendWait := postSendRetryReceiptWait

	cmd := &cobra.Command{
		Use:   "event",
		Short: "Send an event invitation (usually to a group)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(to) == "" {
				return fmt.Errorf("--to is required")
			}
			req := sendDelegateRequest{
				Kind:           sendEventKind,
				To:             to,
				Pick:           pick,
				Name:           name,
				Description:    description,
				Location:       location,
				JoinLink:       joinLink,
				PostSendWaitMS: durationMillis(postSendWait),
			}
			startAt, err := parseEventTime("--start", start)
			if err != nil {
				return err
			}
			req.StartUnix = startAt.Unix()
			if strings.TrimSpace(end) != "" {
				endAt, err := parseEventTime("--end", end)
				if err != nil {
					return err
				}
				req.EndUnix = endAt.Unix()
			}
			if _, err := eventDetailsFromRequest(req); err != nil {
				return err
			}
			return runChatsMessagesCommand(flags, req)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "group (or chat) JID, phone number, or name")
	cmd.Flags().IntVar(&pick, "pick", 0, "when --to is ambiguous, pick the Nth match (1-indexed)")
	cmd.Flags().StringVar(&name, "name", "", "event name")
	cmd.Flags().StringVar(&description, "description", "", "event description")
	cmd.Flags().StringVar(&start, "start", "", "start time (RFC3339, for example 2026-10-01T18:00:00+02:00)")
	cmd.Flags().StringVar(&end, "end", "", "optional end time (RFC3339)")
	cmd.Flags().StringVar(&location, "location", "", "optional location text")
	cmd.Flags().StringVar(&joinLink, "join-link", "", "optional WhatsApp call link (https://call.whatsapp.com/...) created on the phone")
	cmd.Flags().DurationVar(&postSendWait, "post-send-wait", postSendRetryReceiptWait, "keep the connection alive after send so retry receipts can be handled (0 disables)")
	return cmd
}

func parseEventTime(flag, value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fmt.Errorf("%s is required", flag)
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s %q: use RFC3339, for example 2026-10-01T18:00:00Z", flag, value)
	}
	return t, nil
}

// eventDetailsFromRequest validates an event; the sync process checks again.
func eventDetailsFromRequest(req sendDelegateRequest) (wa.EventDetails, error) {
	d := wa.EventDetails{
		Name:        strings.TrimSpace(req.Name),
		Description: req.Description,
		Location:    req.Location,
		JoinLink:    strings.TrimSpace(req.JoinLink),
	}
	if d.Name == "" {
		return wa.EventDetails{}, fmt.Errorf("--name is required")
	}
	if req.StartUnix <= 0 {
		return wa.EventDetails{}, fmt.Errorf("--start is required")
	}
	d.Start = time.Unix(req.StartUnix, 0).UTC()
	if req.EndUnix != 0 {
		d.End = time.Unix(req.EndUnix, 0).UTC()
		if !d.End.After(d.Start) {
			return wa.EventDetails{}, fmt.Errorf("--end must be after --start")
		}
	}
	if d.JoinLink != "" {
		u, err := url.Parse(d.JoinLink)
		if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, eventCallLinkHost) {
			return wa.EventDetails{}, fmt.Errorf("--join-link must be a WhatsApp call link (https://%s/...)", eventCallLinkHost)
		}
	}
	return d, nil
}

func runSendEvent(ctx context.Context, a chatsMessagesApp, req sendDelegateRequest, ropts recipientOptions) (sendDelegateResponse, error) {
	details, err := eventDetailsFromRequest(req)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	toJID, err := resolveRecipient(a, req.To, ropts)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	switch toJID.Server {
	case types.GroupServer, types.DefaultUserServer, types.HiddenUserServer:
	default:
		return sendDelegateResponse{}, fmt.Errorf("events can only be sent to groups and one-to-one chats, not %s", toJID)
	}
	msg := wa.BuildEventMessage(details)
	toJID = warmupRecipient(ctx, a.WA(), toJID, os.Stderr)
	if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
		return sendDelegateResponse{}, err
	}
	msgID, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (types.MessageID, error) {
		return a.WA().SendEventMessage(ctx, toJID, msg)
	})
	if err != nil {
		return sendDelegateResponse{}, err
	}
	storeErr := persistOutboundTextWith(ctx, a.DB(), a.WA(), toJID, string(msgID), wa.EventMessageText(msg.GetEventMessage()), time.Now().UTC())
	waitForPostSendRetryReceipts(ctx, millisDuration(req.PostSendWaitMS, 0))
	resp := sendDelegateResponse{OK: true, Sent: true, To: toJID.String(), ID: string(msgID), Name: details.Name}
	if storeErr != nil {
		resp.StoreWarning = storeErr.Error()
	}
	return resp, nil
}
