package main

import (
	"fmt"
	"os"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
)

func historyJobArgs(opts app.BackfillOptions) *delegateJobArgs {
	return &delegateJobArgs{
		Count:      opts.Count,
		Requests:   opts.Requests,
		WaitMS:     durationMillis(opts.WaitPerRequest),
		IdleExitMS: durationMillis(opts.IdleExit),
	}
}

func (args delegateJobArgs) historyOptions(chat string) app.BackfillOptions {
	return app.BackfillOptions{
		ChatJID:        chat,
		Count:          args.Count,
		Requests:       args.Requests,
		WaitPerRequest: millisDuration(args.WaitMS, 0),
		IdleExit:       millisDuration(args.IdleExitMS, 0),
	}
}

func writeHistoryBackfillResult(flags *rootFlags, res app.BackfillResult) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{
			"chat":            res.ChatJID,
			"requests_sent":   res.RequestsSent,
			"responses_seen":  res.ResponsesSeen,
			"messages_added":  res.MessagesAdded,
			"messages_synced": res.MessagesSynced,
		})
	}
	fmt.Fprintf(os.Stdout, "Backfill complete for %s. Added %d messages (%d requests).\n", res.ChatJID, res.MessagesAdded, res.RequestsSent)
	return nil
}
