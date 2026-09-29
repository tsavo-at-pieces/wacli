package main

import (
	"context"
	"fmt"
	"time"

	"github.com/openclaw/wacli/internal/app"
)

// Media retry for `media transcribe`, whose direct CDN download failed
// because the media expired.
const (
	// transcribeRetryWait bounds each of the retry's two attempts.
	transcribeRetryWait = 15 * time.Second
	// transcribeRetryTimeout bounds the whole delegated retry, download
	// included.
	transcribeRetryTimeout = 90 * time.Second
)

// errRecoveryReadOnly: read-only mode never asks the phone to re-upload.
var errRecoveryReadOnly = fmt.Errorf("read-only mode does not ask the phone to re-upload media")

// recoverExpiredThroughSync asks a same-store `sync --follow` to run a media
// retry for one message and returns the recovered file. It returns an error
// wrapping errSendDelegateUnavailable when no sync process is running.
func recoverExpiredThroughSync(ctx context.Context, flags *rootFlags, chat, id string) (string, error) {
	if flags.isReadOnly() {
		return "", errRecoveryReadOnly
	}
	req := sendDelegateRequest{Kind: mediaRetryKind, Chat: chat, ID: id, Job: retryJobArgs(app.RetryMediaOptions{
		BatchSize: 1,
		Wait:      transcribeRetryWait,
	})}
	resp, err := delegateJob(ctx, flags, req, transcribeRetryTimeout, nil)
	if err != nil {
		return "", explainUnsupportedDelegateKind(err, req.Kind)
	}
	var res app.MediaRetryResult
	if err := decodeDelegateJobResult(resp, &res); err != nil {
		return "", err
	}
	for _, o := range res.Outcomes {
		if o.MsgID != id {
			continue
		}
		if o.Status == "recovered" && o.Path != "" {
			return o.Path, nil
		}
		if o.Detail != "" {
			return "", fmt.Errorf("media retry: %s (%s)", o.Status, o.Detail)
		}
		return "", fmt.Errorf("media retry: %s", o.Status)
	}
	return "", fmt.Errorf("media retry returned no outcome for message %s", id)
}
