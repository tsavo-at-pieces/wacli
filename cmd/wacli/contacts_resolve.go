package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

// contactResolution answers "which phone number is this LID?" (and the reverse)
// from the session's verified PN/LID map, so consumers never have to read
// whatsmeow's private tables (#420). An identity without a known pair is
// reported with resolved=false instead of being dropped.
type contactResolution struct {
	Input    string `json:"input"`
	JID      string `json:"jid,omitempty"`
	Phone    string `json:"phone,omitempty"`
	LID      string `json:"lid,omitempty"`
	Name     string `json:"name,omitempty"`
	Resolved bool   `json:"resolved"`
	Error    string `json:"error,omitempty"`
}

func newContactsResolveCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resolve <lid|phone|jid> [...]",
		Short: "Map LIDs to phone numbers and phone numbers to LIDs",
		Long: "Map each LID to its phone number (and each phone number to its LID) using the\n" +
			"local session's verified mapping. Reads only local state, so it works while\n" +
			"sync --follow is running. Unknown identities are reported with resolved=false.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, false, false)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			resolver, err := contactReadResolver(a)
			if err != nil {
				return err
			}
			results := make([]contactResolution, 0, len(args))
			for _, arg := range args {
				results = append(results, resolveContactIdentity(ctx, resolver, arg))
			}
			return writeContactResolutions(os.Stdout, flags.asJSON, fullTableOutput(flags.fullOutput), results)
		},
	}
	return cmd
}

func resolveContactIdentity(ctx context.Context, resolver app.LocalResolver, raw string) contactResolution {
	res := contactResolution{Input: raw}
	jid, err := wa.ParseUserOrJID(raw)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	jid = jid.ToNonAD()
	var pn, lid types.JID
	switch jid.Server {
	case types.HiddenUserServer:
		lid = jid
		if resolver != nil {
			if mapped := resolver.ResolveLIDToPN(ctx, jid); mapped.Server == types.DefaultUserServer && mapped.User != "" {
				pn = mapped.ToNonAD()
			}
		}
	case types.DefaultUserServer:
		pn = jid
		if resolver != nil {
			if mapped := resolver.ResolvePNToLID(ctx, jid); mapped.Server == types.HiddenUserServer && mapped.User != "" {
				lid = mapped.ToNonAD()
			}
		}
	default:
		res.Error = fmt.Sprintf("%s is not a user JID; groups and channels have no phone/LID pair", jid)
		return res
	}

	if !lid.IsEmpty() {
		res.LID = lid.String()
	}
	if !pn.IsEmpty() {
		res.JID = pn.String()
		res.Phone = pn.User
	}
	res.Resolved = !pn.IsEmpty() && !lid.IsEmpty()
	if res.Resolved && resolver != nil {
		// The name lookup falls back to the bare number, which is not a name.
		if name := resolver.ResolveChatName(ctx, pn, ""); name != pn.User {
			res.Name = name
		}
	}
	return res
}

func writeContactResolutions(w io.Writer, asJSON, fullOutput bool, results []contactResolution) error {
	if asJSON {
		return out.WriteJSON(w, results)
	}
	tw := newTableWriter(w)
	fmt.Fprintln(tw, "INPUT\tRESOLVED\tPHONE\tLID\tNAME")
	for _, r := range results {
		resolved := "yes"
		if !r.Resolved {
			resolved = "no"
		}
		if r.Error != "" {
			resolved = "error: " + r.Error
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			tableCell(r.Input, 28, fullOutput),
			sanitize(resolved),
			tableCell(r.Phone, 16, fullOutput),
			tableCell(r.LID, 28, fullOutput),
			tableCell(r.Name, 24, fullOutput),
		)
	}
	return tw.Flush()
}
