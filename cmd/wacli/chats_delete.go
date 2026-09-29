package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

type chatActionOptions struct {
	chat          string
	pick          int
	deleteMedia   bool
	deleteStarred bool
	confirm       bool
	duration      string
}

func newChatsDeleteCmd(flags *rootFlags) *cobra.Command {
	opts := chatActionOptions{}
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a chat on all your devices (local messages are kept as tombstones)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireChatFlag(opts.chat); err != nil {
				return err
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			if err := confirmChatAction(flags, opts.confirm, fmt.Sprintf("Delete chat %s on WhatsApp for all your devices?", sanitize(opts.chat))); err != nil {
				return err
			}
			return runChatsMessagesCommand(flags, sendDelegateRequest{Kind: chatDeleteKind, To: opts.chat, Pick: opts.pick, DeleteMedia: opts.deleteMedia})
		},
	}
	addChatActionFlags(cmd, &opts)
	cmd.Flags().BoolVar(&opts.deleteMedia, "delete-media", false, "also ask your devices to delete the chat's media (wacli keeps its downloaded copies)")
	cmd.Flags().BoolVar(&opts.confirm, "confirm", false, "skip the confirmation prompt")
	return cmd
}

func newChatsClearCmd(flags *rootFlags) *cobra.Command {
	opts := chatActionOptions{}
	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Clear a chat's messages on all your devices (local messages are kept as tombstones)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireChatFlag(opts.chat); err != nil {
				return err
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			if err := confirmChatAction(flags, opts.confirm, fmt.Sprintf("Clear all messages in %s on WhatsApp for all your devices?", sanitize(opts.chat))); err != nil {
				return err
			}
			return runChatsMessagesCommand(flags, sendDelegateRequest{
				Kind: chatClearKind, To: opts.chat, Pick: opts.pick, DeleteMedia: opts.deleteMedia, DeleteStarred: opts.deleteStarred,
			})
		},
	}
	addChatActionFlags(cmd, &opts)
	cmd.Flags().BoolVar(&opts.deleteStarred, "delete-starred", false, "also clear starred messages (kept by default, as on the phone)")
	cmd.Flags().BoolVar(&opts.deleteMedia, "delete-media", false, "also ask your devices to delete the chat's media (wacli keeps its downloaded copies)")
	cmd.Flags().BoolVar(&opts.confirm, "confirm", false, "skip the confirmation prompt")
	return cmd
}

func newChatsLockCmd(flags *rootFlags, lock bool) *cobra.Command {
	use, short, kind := "lock", "Lock a chat with WhatsApp chat lock", chatLockKind
	if !lock {
		use, short, kind = "unlock", "Unlock a locked chat", chatUnlockKind
	}
	opts := chatActionOptions{}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireChatFlag(opts.chat); err != nil {
				return err
			}
			return runChatsMessagesCommand(flags, sendDelegateRequest{Kind: kind, To: opts.chat, Pick: opts.pick})
		},
	}
	addChatActionFlags(cmd, &opts)
	return cmd
}

func newChatsFavoriteCmd(flags *rootFlags, favorite bool) *cobra.Command {
	use, short, kind := "favorite", "Add a chat to your WhatsApp favorites", chatFavoriteKind
	if !favorite {
		use, short, kind = "unfavorite", "Remove a chat from your WhatsApp favorites", chatUnfavoriteKind
	}
	opts := chatActionOptions{}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireChatFlag(opts.chat); err != nil {
				return err
			}
			return runChatsMessagesCommand(flags, sendDelegateRequest{Kind: kind, To: opts.chat, Pick: opts.pick})
		},
	}
	addChatActionFlags(cmd, &opts)
	return cmd
}

func newChatsDisappearingCmd(flags *rootFlags) *cobra.Command {
	opts := chatActionOptions{}
	cmd := &cobra.Command{
		Use:   "disappearing",
		Short: "Set a chat's disappearing-message timer",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireChatFlag(opts.chat); err != nil {
				return err
			}
			if _, _, err := parseDisappearingDuration(opts.duration); err != nil {
				return err
			}
			return runChatsMessagesCommand(flags, sendDelegateRequest{Kind: chatDisappearingKind, To: opts.chat, Pick: opts.pick, Duration: opts.duration})
		},
	}
	addChatActionFlags(cmd, &opts)
	cmd.Flags().StringVar(&opts.duration, "duration", "", "off, 24h, 7d or 90d")
	return cmd
}

func addChatActionFlags(cmd *cobra.Command, opts *chatActionOptions) {
	cmd.Flags().StringVar(&opts.chat, "chat", "", "chat name, phone number, or JID")
	cmd.Flags().IntVar(&opts.pick, "pick", 0, "choose match N when --chat is ambiguous")
}

func requireChatFlag(chat string) error {
	if strings.TrimSpace(chat) == "" {
		return fmt.Errorf("--chat is required")
	}
	return nil
}

// confirmChatAction asks before a change that removes content on the phone.
// Scripts and --json runs must pass --confirm.
func confirmChatAction(flags *rootFlags, confirmed bool, question string) error {
	if confirmed {
		return nil
	}
	if flags.asJSON || !isInteractive() {
		return fmt.Errorf("this changes WhatsApp on all your devices; pass --confirm to proceed")
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", question)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		return fmt.Errorf("aborted")
	}
	return nil
}

// runChatAction resolves the chat and applies one chat-level change.
func runChatAction(ctx context.Context, a chatsMessagesApp, req sendDelegateRequest, ropts recipientOptions) (sendDelegateResponse, error) {
	var duration time.Duration
	var durationLabel string
	if req.Kind == chatDisappearingKind {
		var err error
		if duration, durationLabel, err = parseDisappearingDuration(req.Duration); err != nil {
			return sendDelegateResponse{}, err
		}
	}
	jid, err := resolveRecipient(a, req.To, ropts)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	resp := sendDelegateResponse{OK: true, Chat: jid.String(), Action: actionName(req.Kind)}
	switch req.Kind {
	case chatDeleteKind:
		n, err := a.DeleteChat(ctx, jid, req.DeleteMedia)
		if err != nil {
			return sendDelegateResponse{}, err
		}
		resp.Count = int(n)
	case chatClearKind:
		n, err := a.ClearChat(ctx, jid, req.DeleteStarred, req.DeleteMedia)
		if err != nil {
			return sendDelegateResponse{}, err
		}
		resp.Count = int(n)
	case chatLockKind, chatUnlockKind:
		if err := a.LockChat(ctx, jid, req.Kind == chatLockKind); err != nil {
			return sendDelegateResponse{}, err
		}
	case chatFavoriteKind, chatUnfavoriteKind:
		if err := a.SetChatFavorite(ctx, jid, req.Kind == chatFavoriteKind); err != nil {
			return sendDelegateResponse{}, err
		}
	case chatDisappearingKind:
		if jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer && jid.Server != types.GroupServer {
			return sendDelegateResponse{}, fmt.Errorf("disappearing messages can only be set for one-to-one chats and groups, not %s", jid)
		}
		if _, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (struct{}, error) {
			return struct{}{}, a.WA().SetDisappearingTimer(ctx, jid, duration)
		}); err != nil {
			return sendDelegateResponse{}, err
		}
		resp.Duration = durationLabel
	default:
		return sendDelegateResponse{}, fmt.Errorf("unsupported send kind %q", req.Kind)
	}
	return resp, nil
}

// parseDisappearingDuration accepts the timers WhatsApp offers.
func parseDisappearingDuration(value string) (time.Duration, string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "off", "0", "0s", "none":
		return 0, "off", nil
	case "24h", "1d", "day":
		return 24 * time.Hour, "24h", nil
	case "7d", "168h", "1w", "week":
		return 7 * 24 * time.Hour, "7d", nil
	case "90d", "2160h":
		return 90 * 24 * time.Hour, "90d", nil
	case "":
		return 0, "", fmt.Errorf("--duration is required (off, 24h, 7d or 90d)")
	default:
		return 0, "", fmt.Errorf("invalid --duration %q (want off, 24h, 7d or 90d)", value)
	}
}

// writeChatActionState adds lock and delete/clear state to `chats show`.
func writeChatActionState(w io.Writer, c store.Chat) {
	fmt.Fprintf(w, "Locked: %t\n", c.Locked)
	if c.DeletedAt != nil {
		fmt.Fprintf(w, "Deleted through: %s\n", c.DeletedAt.Local().Format(time.RFC3339))
	}
	if c.ClearedAt != nil {
		fmt.Fprintf(w, "Cleared through: %s\n", c.ClearedAt.Local().Format(time.RFC3339))
	}
}
