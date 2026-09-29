package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func newContactsSaveCmd(flags *rootFlags) *cobra.Command {
	var jidRaw, phoneRaw, firstName, fullName string
	var saveToPhone bool
	cmd := &cobra.Command{
		Use:   "save (--jid JID | --phone PHONE) [--first-name NAME] [--full-name NAME]",
		Short: "Save or rename a WhatsApp contact",
		Long: "Save a phone number to the account's WhatsApp contacts, or rename a saved\n" +
			"contact. The entry syncs to the phone and every linked device; --save-to-phone\n" +
			"also asks the phone to add it to its own address book. --full-name defaults to\n" +
			"--first-name.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := contactBookTargetFlag(jidRaw, phoneRaw)
			if err != nil {
				return err
			}
			if strings.TrimSpace(firstName) == "" && strings.TrimSpace(fullName) == "" {
				return fmt.Errorf("--full-name or --first-name is required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			return delegatedCommand[contactSaveResult]{
				req:  sendDelegateRequest{Kind: contactSaveKind, To: target, FirstName: firstName, FullName: fullName, SaveToPhone: saveToPhone},
				live: true,
				op:   saveWhatsAppContact,
				write: func(res contactSaveResult) error {
					warnContactStoreFailure(res.StoreWarning)
					if flags.asJSON {
						return out.WriteJSON(os.Stdout, res)
					}
					fmt.Fprintf(os.Stdout, "Saved %s as %s\n", res.JID, sanitize(res.FullName))
					return nil
				},
			}.run(flags)
		},
	}
	cmd.Flags().StringVar(&jidRaw, "jid", "", "contact JID or phone number")
	cmd.Flags().StringVar(&phoneRaw, "phone", "", "contact phone number")
	cmd.Flags().StringVar(&firstName, "first-name", "", "first name")
	cmd.Flags().StringVar(&fullName, "full-name", "", "full name (defaults to --first-name)")
	cmd.Flags().BoolVar(&saveToPhone, "save-to-phone", false, "also save to the phone's own address book")
	return cmd
}

func newContactsDeleteCmd(flags *rootFlags) *cobra.Command {
	var jidRaw string
	cmd := &cobra.Command{
		Use:   "delete --jid JID",
		Short: "Delete a saved WhatsApp contact",
		Long: "Remove a contact from the account's WhatsApp contacts on every linked device.\n" +
			"Chats, messages and local aliases or tags are kept. Fails without changing\n" +
			"anything when the session has no saved entry for the contact.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := parseContactBookTarget(jidRaw); err != nil {
				return err
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			return delegatedCommand[contactDeleteResult]{
				req:  sendDelegateRequest{Kind: contactDeleteKind, To: jidRaw},
				live: true,
				op:   deleteWhatsAppContact,
				write: func(res contactDeleteResult) error {
					warnContactStoreFailure(res.StoreWarning)
					if flags.asJSON {
						return out.WriteJSON(os.Stdout, res)
					}
					fmt.Fprintf(os.Stdout, "Deleted contact %s\n", res.JID)
					return nil
				},
			}.run(flags)
		},
	}
	cmd.Flags().StringVar(&jidRaw, "jid", "", "contact JID or phone number")
	return cmd
}

func newContactsBlockCmd(flags *rootFlags, block bool) *cobra.Command {
	var jidRaw string
	use, short, kind := "block", "Block a WhatsApp user", contactBlockKind
	if !block {
		use, short, kind = "unblock", "Unblock a WhatsApp user", contactUnblockKind
	}
	cmd := &cobra.Command{
		Use:   use + " --jid JID",
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := parseContactBookTarget(jidRaw); err != nil {
				return err
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			return delegatedCommand[contactBlockResult]{
				req:  sendDelegateRequest{Kind: kind, To: jidRaw},
				live: true,
				op:   changeContactBlock,
				write: func(res contactBlockResult) error {
					warnContactStoreFailure(res.StoreWarning)
					if flags.asJSON {
						return out.WriteJSON(os.Stdout, res)
					}
					if res.Blocked {
						fmt.Fprintf(os.Stdout, "Blocked %s\n", res.JID)
					} else {
						fmt.Fprintf(os.Stdout, "Unblocked %s\n", res.JID)
					}
					return nil
				},
			}.run(flags)
		},
	}
	cmd.Flags().StringVar(&jidRaw, "jid", "", "user JID or phone number")
	return cmd
}

func newContactsBlocklistCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "blocklist",
		Short: "List blocked WhatsApp users",
		Long: "Fetch the account's block list from WhatsApp and refresh the local copy that\n" +
			"`contacts show` reports.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := flags.requireWritable(); err != nil {
				return err
			}
			return delegatedCommand[blocklistResult]{
				req:  sendDelegateRequest{Kind: contactsBlocklistKind},
				live: true,
				op:   fetchBlocklist,
				write: func(res blocklistResult) error {
					warnContactStoreFailure(res.StoreWarning)
					if flags.asJSON {
						return out.WriteJSON(os.Stdout, res)
					}
					if len(res.Blocked) == 0 {
						fmt.Fprintln(os.Stdout, "No blocked users.")
						return nil
					}
					w := newTableWriter(os.Stdout)
					fmt.Fprintln(w, "JID\tPHONE JID")
					for _, b := range res.Blocked {
						fmt.Fprintf(w, "%s\t%s\n", b.JID, b.PhoneJID)
					}
					return w.Flush()
				},
			}.run(flags)
		},
	}
}

func warnContactStoreFailure(warning string) {
	if warning != "" {
		fmt.Fprintf(os.Stderr, "warning: WhatsApp was updated, but the local contact store was not: %s\n", warning)
	}
}

func contactBookTargetFlag(jidRaw, phoneRaw string) (string, error) {
	jidRaw, phoneRaw = strings.TrimSpace(jidRaw), strings.TrimSpace(phoneRaw)
	switch {
	case jidRaw != "" && phoneRaw != "":
		return "", fmt.Errorf("pass --jid or --phone, not both")
	case jidRaw == "" && phoneRaw == "":
		return "", fmt.Errorf("--jid or --phone is required")
	case phoneRaw != "":
		if strings.Contains(phoneRaw, "@") {
			return "", fmt.Errorf("--phone takes a phone number; use --jid for a JID")
		}
		jidRaw = phoneRaw
	}
	if _, err := parseContactBookTarget(jidRaw); err != nil {
		return "", err
	}
	return jidRaw, nil
}

// parseContactBookTarget accepts a phone number or a user JID, phone-number
// or LID.
func parseContactBookTarget(raw string) (types.JID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return types.JID{}, fmt.Errorf("--jid is required")
	}
	jid, err := wa.ParseUserOrJID(raw)
	if err != nil {
		return types.JID{}, err
	}
	if jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer {
		return types.JID{}, fmt.Errorf("unsupported contact %q: pass a phone number or user JID", raw)
	}
	return jid.ToNonAD(), nil
}

type contactSaveResult struct {
	JID          string `json:"jid"`
	LID          string `json:"lid,omitempty"`
	FullName     string `json:"full_name"`
	FirstName    string `json:"first_name,omitempty"`
	SaveToPhone  bool   `json:"save_to_phone"`
	StoreWarning string `json:"store_warning,omitempty"`
}

// saveWhatsAppContact saves the contact on WhatsApp, then records the saved
// name locally so contacts show and search use it at once.
func saveWhatsAppContact(ctx context.Context, a waStoreApp, req sendDelegateRequest) (contactSaveResult, error) {
	target, err := parseContactBookTarget(req.To)
	if err != nil {
		return contactSaveResult{}, err
	}
	first, full := strings.TrimSpace(req.FirstName), strings.TrimSpace(req.FullName)
	if full == "" {
		full = first
	}
	if full == "" {
		return contactSaveResult{}, fmt.Errorf("--full-name or --first-name is required")
	}
	saved, err := a.WA().SaveContact(ctx, wa.ContactSaveRequest{JID: target, FirstName: first, FullName: full, SaveOnPrimaryAddressbook: req.SaveToPhone})
	if err != nil {
		return contactSaveResult{}, fmt.Errorf("save contact: %w", err)
	}
	result := contactSaveResult{JID: saved.JID.String(), FullName: full, FirstName: first, SaveToPhone: req.SaveToPhone}
	var others []string
	if !saved.LID.IsEmpty() {
		result.LID = saved.LID.String()
		others = append(others, result.LID)
	}
	if err := a.DB().SetContactBookName(result.JID, saved.JID.User, first, full, others...); err != nil {
		result.StoreWarning = err.Error()
	}
	return result, nil
}

type contactDeleteResult struct {
	JID          string   `json:"jid"`
	LID          string   `json:"lid,omitempty"`
	Deleted      bool     `json:"deleted"`
	Removed      []string `json:"removed"`
	StoreWarning string   `json:"store_warning,omitempty"`
}

func deleteWhatsAppContact(ctx context.Context, a waStoreApp, req sendDelegateRequest) (contactDeleteResult, error) {
	target, err := parseContactBookTarget(req.To)
	if err != nil {
		return contactDeleteResult{}, err
	}
	deleted, err := a.WA().DeleteContact(ctx, target)
	if err != nil {
		return contactDeleteResult{}, fmt.Errorf("delete contact: %w", err)
	}
	result := contactDeleteResult{JID: deleted.JID.String(), Deleted: true, Removed: []string{}}
	jids := []string{result.JID}
	if !deleted.LID.IsEmpty() && deleted.LID != deleted.JID {
		result.LID = deleted.LID.String()
		jids = append(jids, result.LID)
	}
	for _, removed := range deleted.Removed {
		result.Removed = append(result.Removed, removed.String())
	}
	if err := a.DB().ClearContactBookName(jids...); err != nil {
		result.StoreWarning = err.Error()
	}
	return result, nil
}

type contactBlockResult struct {
	JID          string `json:"jid"`
	Blocked      bool   `json:"blocked"`
	StoreWarning string `json:"store_warning,omitempty"`
}

func changeContactBlock(ctx context.Context, a waStoreApp, req sendDelegateRequest) (contactBlockResult, error) {
	var action events.BlocklistChangeAction
	switch req.Kind {
	case contactBlockKind:
		action = events.BlocklistChangeActionBlock
	case contactUnblockKind:
		action = events.BlocklistChangeActionUnblock
	default:
		return contactBlockResult{}, fmt.Errorf("unsupported send kind %q", req.Kind)
	}
	target, err := parseContactBookTarget(req.To)
	if err != nil {
		return contactBlockResult{}, err
	}
	if _, err := a.WA().UpdateBlocklist(ctx, target, action); err != nil {
		return contactBlockResult{}, fmt.Errorf("%s contact: %w", action, err)
	}
	result := contactBlockResult{JID: target.String(), Blocked: action == events.BlocklistChangeActionBlock}
	if err := a.DB().SetContactsBlocked(blockIdentityJIDs(ctx, a, target), result.Blocked, time.Now()); err != nil {
		result.StoreWarning = err.Error()
	}
	return result, nil
}

type blockedUser struct {
	JID      string `json:"jid"`
	PhoneJID string `json:"phone_jid,omitempty"`
}

type blocklistResult struct {
	Count        int           `json:"count"`
	Blocked      []blockedUser `json:"blocked"`
	StoreWarning string        `json:"store_warning,omitempty"`
}

// fetchBlocklist reads the block list from WhatsApp and replaces the local
// copy with it.
func fetchBlocklist(ctx context.Context, a waStoreApp, _ sendDelegateRequest) (blocklistResult, error) {
	list, err := a.WA().GetBlocklist(ctx)
	if err != nil {
		return blocklistResult{}, fmt.Errorf("get blocklist: %w", err)
	}
	result := blocklistResult{Blocked: []blockedUser{}}
	var jids []string
	if list != nil {
		for _, jid := range list.JIDs {
			jid = jid.ToNonAD()
			entry := blockedUser{JID: jid.String()}
			if pn := a.WA().ResolveLIDToPN(ctx, jid).ToNonAD(); pn != jid && pn.Server == types.DefaultUserServer {
				entry.PhoneJID = pn.String()
			}
			result.Blocked = append(result.Blocked, entry)
			jids = append(jids, entry.JID)
			if entry.PhoneJID != "" {
				jids = append(jids, entry.PhoneJID)
			}
		}
	}
	result.Count = len(result.Blocked)
	if err := a.DB().ReplaceContactBlocks(jids, time.Now()); err != nil {
		result.StoreWarning = err.Error()
	}
	return result, nil
}

// blockIdentityJIDs is the user and, when the session knows it, the other
// identity of the same person.
func blockIdentityJIDs(ctx context.Context, a waStoreApp, target types.JID) []string {
	jids := []string{target.String()}
	var pair types.JID
	switch target.Server {
	case types.DefaultUserServer:
		pair = a.WA().ResolvePNToLID(ctx, target).ToNonAD()
	case types.HiddenUserServer:
		pair = a.WA().ResolveLIDToPN(ctx, target).ToNonAD()
	}
	if !pair.IsEmpty() && pair != target {
		jids = append(jids, pair.String())
	}
	return jids
}
