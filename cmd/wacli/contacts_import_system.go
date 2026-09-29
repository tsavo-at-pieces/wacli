package main

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/syscontacts"
	"github.com/spf13/cobra"
)

type systemContactMatch struct {
	JID           string `json:"jid"`
	Phone         string `json:"phone"`
	CurrentName   string `json:"current_name"`
	SystemName    string `json:"system_name"`
	ExistingValue string `json:"existing_system_name,omitempty"`
}

func newContactsImportSystemCmd(flags *rootFlags) *cobra.Command {
	var dryRun bool
	var clear bool
	var input string
	cmd := &cobra.Command{
		Use:   "import-system",
		Short: "Import display names from macOS Contacts",
		Long: `Import display names from macOS Contacts and store them as local system names.

System names are local wacli metadata. They do not edit WhatsApp contacts or
macOS Contacts. Display precedence is: alias, system name, WhatsApp names.

On macOS, the default source is the Contacts framework. Use --input to import
from a JSON array or NDJSON file with fields first_name, last_name, full_name,
and phones.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !dryRun {
				if err := flags.requireWritable(); err != nil {
					return err
				}
			}

			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			if clear && !dryRun {
				return delegatedCommand[systemClearResult]{
					req:   sendDelegateRequest{Kind: contactsImportSystemClearKind},
					op:    clearSystemNames,
					write: func(res systemClearResult) error { return writeSystemClear(flags, res) },
				}.run(flags)
			}
			if clear {
				a, lk, err := newApp(ctx, flags, false, false)
				if err != nil {
					return err
				}
				defer closeApp(a, lk)
				return runContactsSystemClearPreview(a.DB(), flags.asJSON)
			}

			// The caller reads the system contacts, so a sync process that
			// holds the lock only needs the phone-to-name map.
			systemContacts, err := readSystemContacts(ctx, input)
			if err != nil {
				return err
			}
			phoneToName := syscontacts.PhoneToName(systemContacts)
			if dryRun {
				a, lk, err := newApp(ctx, flags, false, false)
				if err != nil {
					return err
				}
				defer closeApp(a, lk)
				result, err := matchAndApplySystemNames(a.DB(), phoneToName, true)
				if err != nil {
					return err
				}
				return writeSystemImport(flags, result)
			}
			return delegatedCommand[systemImportResult]{
				req:   sendDelegateRequest{Kind: contactsImportSystemKind, SystemNames: phoneToName},
				op:    importSystemNames,
				write: func(res systemImportResult) error { return writeSystemImport(flags, res) },
			}.run(flags)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be imported without writing")
	cmd.Flags().BoolVar(&clear, "clear", false, "clear all imported system names")
	cmd.Flags().StringVar(&input, "input", "", "read system contacts from JSON/NDJSON instead of macOS Contacts")
	return cmd
}

func readSystemContacts(ctx context.Context, input string) ([]syscontacts.Contact, error) {
	if input != "" {
		data, err := readRegularFileLimited(input, syscontacts.MaxContactsDecodeBytes)
		if err != nil {
			return nil, err
		}
		return syscontacts.Decode(bytes.NewReader(data))
	}
	return syscontacts.ReadSystem(ctx)
}

func runContactsSystemClearPreview(db *store.DB, asJSON bool) error {
	count, err := db.CountSystemNames()
	if err != nil {
		return err
	}
	if asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{"would_clear": count, "dry_run": true})
	}
	fmt.Fprintf(os.Stdout, "Would clear %d system contact name(s).\n", count)
	return nil
}

type systemClearResult struct {
	Cleared int64 `json:"cleared"`
}

func clearSystemNames(_ context.Context, a waStoreApp, _ sendDelegateRequest) (systemClearResult, error) {
	cleared, err := a.DB().ClearAllSystemNames()
	if err != nil {
		return systemClearResult{}, err
	}
	return systemClearResult{Cleared: cleared}, nil
}

func writeSystemClear(flags *rootFlags, res systemClearResult) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, res)
	}
	fmt.Fprintf(os.Stdout, "Cleared %d system contact name(s).\n", res.Cleared)
	return nil
}

// systemImportResult is what import-system prints. Fields are in the order
// its earlier map output sorted them, so --json output is unchanged.
type systemImportResult struct {
	Applied        *int                 `json:"applied,omitempty"`
	DryRun         bool                 `json:"dry_run"`
	Matched        int                  `json:"matched"`
	Matches        []systemContactMatch `json:"matches"`
	SkippedNoMatch int                  `json:"skipped_no_match"`
	SkippedNoPhone int                  `json:"skipped_no_phone"`
	SkippedSame    int                  `json:"skipped_same"`
}

func importSystemNames(_ context.Context, a waStoreApp, req sendDelegateRequest) (systemImportResult, error) {
	return matchAndApplySystemNames(a.DB(), req.SystemNames, false)
}

// matchAndApplySystemNames matches system names to synced contacts by phone
// number and, unless dryRun, stores them.
func matchAndApplySystemNames(db *store.DB, phoneToName map[string]string, dryRun bool) (systemImportResult, error) {
	localContacts, err := db.ListContacts(0)
	if err != nil {
		return systemImportResult{}, err
	}
	matches, skippedNoPhone, skippedNoMatch, skippedSame := matchSystemContacts(localContacts, phoneToName)
	result := systemImportResult{
		DryRun:         dryRun,
		Matched:        len(matches),
		Matches:        matches,
		SkippedNoMatch: skippedNoMatch,
		SkippedNoPhone: skippedNoPhone,
		SkippedSame:    skippedSame,
	}
	if dryRun {
		return result, nil
	}
	applied := 0
	for _, m := range matches {
		if err := db.SetSystemName(m.JID, m.SystemName); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to set system name for %s: %v\n", m.JID, err)
			continue
		}
		applied++
	}
	result.Applied = &applied
	return result, nil
}

func writeSystemImport(flags *rootFlags, res systemImportResult) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, res)
	}
	if res.DryRun {
		writeSystemImportPreview(res.Matches, res.SkippedNoPhone, res.SkippedNoMatch, res.SkippedSame)
		return nil
	}
	applied := 0
	if res.Applied != nil {
		applied = *res.Applied
	}
	fmt.Fprintf(os.Stdout, "Applied %d system contact name(s).\n", applied)
	return nil
}

func matchSystemContacts(local []store.Contact, phoneToName map[string]string) ([]systemContactMatch, int, int, int) {
	var matches []systemContactMatch
	var skippedNoPhone, skippedNoMatch, skippedSame int
	for _, c := range local {
		phone := syscontacts.NormalizePhone(c.Phone)
		if phone == "" {
			skippedNoPhone++
			continue
		}
		systemName, ok := phoneToName[phone]
		if !ok {
			skippedNoMatch++
			continue
		}
		if c.SystemName == systemName {
			skippedSame++
			continue
		}
		matches = append(matches, systemContactMatch{
			JID:           c.JID,
			Phone:         c.Phone,
			CurrentName:   c.Name,
			SystemName:    systemName,
			ExistingValue: c.SystemName,
		})
	}
	return matches, skippedNoPhone, skippedNoMatch, skippedSame
}

func writeSystemImportPreview(matches []systemContactMatch, skippedNoPhone, skippedNoMatch, skippedSame int) {
	fmt.Fprintf(os.Stdout, "Would import %d system contact name(s).\n", len(matches))
	fmt.Fprintf(os.Stdout, "Skipped: %d no phone, %d no match, %d already current.\n", skippedNoPhone, skippedNoMatch, skippedSame)
	if len(matches) == 0 {
		return
	}
	w := newTableWriter(os.Stdout)
	fmt.Fprintln(w, "PHONE\tCURRENT\tSYSTEM")
	for _, m := range matches {
		fmt.Fprintf(w, "%s\t%s\t%s\n",
			tableCell(m.Phone, 16, false),
			tableCell(m.CurrentName, 24, false),
			tableCell(m.SystemName, 24, false),
		)
	}
	_ = w.Flush()
}
