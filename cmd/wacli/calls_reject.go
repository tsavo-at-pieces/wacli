package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

func newCallsRejectCmd(flags *rootFlags) *cobra.Command {
	var from string
	var callID string
	cmd := &cobra.Command{
		Use:   "reject",
		Short: "Decline a ringing incoming call",
		Long: "Decline an incoming call that is still ringing. Take the caller and call ID from\n" +
			"the call's offer event in `wacli calls list --json` (sender_jid and call_id). The\n" +
			"caller sees the call declined. While `sync --follow` runs, it sends the rejection.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(from) == "" || strings.TrimSpace(callID) == "" {
				return fmt.Errorf("--from and --call-id are required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				return delegateAfterOpenFailure(ctx, flags, err, sendDelegateRequest{Kind: callRejectKind, To: from, ID: callID}, func(resp sendDelegateResponse) error {
					var res callRejectResult
					if err := decodeDelegatedResult(resp, &res); err != nil {
						return err
					}
					return writeCallRejected(flags, res)
				})
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}
			res, err := rejectCall(ctx, a, from, callID)
			if err != nil {
				return err
			}
			return writeCallRejected(flags, res)
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "caller phone number or user JID")
	cmd.Flags().StringVar(&callID, "call-id", "", "call ID of the ringing call")
	return cmd
}

type callRejectResult struct {
	Rejected bool   `json:"rejected"`
	From     string `json:"from"`
	CallID   string `json:"call_id"`
}

func rejectCall(ctx context.Context, a waStoreApp, rawFrom, callID string) (callRejectResult, error) {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return callRejectResult{}, fmt.Errorf("--call-id is required")
	}
	from, err := wa.ParseUserOrJID(rawFrom)
	if err != nil {
		return callRejectResult{}, err
	}
	if from.Server != types.DefaultUserServer && from.Server != types.HiddenUserServer {
		return callRejectResult{}, fmt.Errorf("--from must be a caller's phone number or user JID, got %s", from)
	}
	from = from.ToNonAD()
	if err := a.WA().RejectCall(ctx, from, callID); err != nil {
		return callRejectResult{}, err
	}
	return callRejectResult{Rejected: true, From: from.String(), CallID: callID}, nil
}

func writeCallRejected(flags *rootFlags, res callRejectResult) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, res)
	}
	fmt.Fprintf(os.Stdout, "Rejected call %s from %s.\n", sanitize(res.CallID), res.From)
	return nil
}
