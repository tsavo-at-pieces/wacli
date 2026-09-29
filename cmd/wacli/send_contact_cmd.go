package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

func newSendContactCmd(flags *rootFlags) *cobra.Command {
	var to string
	var pick int
	var contacts []string
	var name string
	postSendWait := postSendRetryReceiptWait

	cmd := &cobra.Command{
		Use:   "contact",
		Short: "Share one or more contact cards",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(to) == "" || len(contacts) == 0 {
				return fmt.Errorf("--to and --contact are required")
			}
			if err := validateContactOptions(contacts, name); err != nil {
				return err
			}
			return runChatsMessagesCommand(flags, sendDelegateRequest{
				Kind:           sendContactKind,
				To:             to,
				Pick:           pick,
				Contacts:       contacts,
				Name:           name,
				PostSendWaitMS: durationMillis(postSendWait),
			})
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "recipient JID, phone number, or contact/group/chat name")
	cmd.Flags().IntVar(&pick, "pick", 0, "when --to is ambiguous, pick the Nth match (1-indexed)")
	cmd.Flags().StringArrayVar(&contacts, "contact", nil, "contact to share: JID, phone number, or contact name (repeatable)")
	cmd.Flags().StringVar(&name, "name", "", "name on the card (only with a single --contact; defaults to the stored contact name)")
	cmd.Flags().DurationVar(&postSendWait, "post-send-wait", postSendRetryReceiptWait, "keep the connection alive after send so retry receipts can be handled (0 disables)")
	return cmd
}

func validateContactOptions(contacts []string, name string) error {
	if len(contacts) == 0 {
		return fmt.Errorf("at least one --contact is required")
	}
	for _, c := range contacts {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("--contact must not be empty")
		}
	}
	if strings.TrimSpace(name) != "" && len(contacts) > 1 {
		return fmt.Errorf("--name only applies to a single --contact")
	}
	return nil
}

// contactCards resolves each --contact to a phone number and a name.
func contactCards(ctx context.Context, a chatsMessagesApp, inputs []string, name string) ([]wa.ContactCard, error) {
	cards := make([]wa.ContactCard, 0, len(inputs))
	for _, input := range inputs {
		jid, err := resolveRecipient(a, input, recipientOptions{asJSON: true})
		if err != nil {
			return nil, fmt.Errorf("--contact %q: %w", input, err)
		}
		if jid.Server == types.HiddenUserServer {
			jid = a.WA().ResolveLIDToPN(ctx, jid)
		}
		if jid.Server != types.DefaultUserServer {
			return nil, fmt.Errorf("--contact %q is not a person with a known phone number (%s)", input, jid)
		}
		jid = jid.ToNonAD()
		card := wa.ContactCard{Name: strings.TrimSpace(name), Phone: jid.User}
		if card.Name == "" {
			card.Name = storedContactName(a, jid)
		}
		cards = append(cards, card)
	}
	return cards, nil
}

func storedContactName(a chatsMessagesApp, jid types.JID) string {
	if c, err := a.DB().GetContact(jid.String()); err == nil {
		for _, name := range []string{c.Alias, c.Name, c.SystemName} {
			if strings.TrimSpace(name) != "" {
				return strings.TrimSpace(name)
			}
		}
	}
	return "+" + jid.User
}

func runSendContact(ctx context.Context, a chatsMessagesApp, req sendDelegateRequest, ropts recipientOptions) (sendDelegateResponse, error) {
	if err := validateContactOptions(req.Contacts, req.Name); err != nil {
		return sendDelegateResponse{}, err
	}
	toJID, err := resolveRecipient(a, req.To, ropts)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	cards, err := contactCards(ctx, a, req.Contacts, req.Name)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	msg, err := wa.BuildContactsMessage(cards)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	toJID = warmupRecipient(ctx, a.WA(), toJID, os.Stderr)
	if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
		return sendDelegateResponse{}, err
	}
	msgID, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (types.MessageID, error) {
		return a.WA().SendProtoMessage(ctx, toJID, msg)
	})
	if err != nil {
		return sendDelegateResponse{}, err
	}
	storeErr := persistOutboundTextWith(ctx, a.DB(), a.WA(), toJID, string(msgID), wa.ContactMessageText(msg), time.Now().UTC())
	waitForPostSendRetryReceipts(ctx, millisDuration(req.PostSendWaitMS, 0))
	resp := sendDelegateResponse{OK: true, Sent: true, To: toJID.String(), ID: string(msgID), Count: len(cards)}
	if storeErr != nil {
		resp.StoreWarning = storeErr.Error()
	}
	return resp, nil
}
