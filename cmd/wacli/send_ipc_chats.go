package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/types"
)

// Chat, list, message and send kinds delegated to a same-store
// `sync --follow`. As with the other management kinds, each command has its
// own kind so a sync process that predates it refuses it before doing anything.
const (
	chatDeleteKind       = "chat_delete"
	chatClearKind        = "chat_clear"
	chatLockKind         = "chat_lock"
	chatUnlockKind       = "chat_unlock"
	chatDisappearingKind = "chat_disappearing"
	chatFavoriteKind     = "chat_favorite"
	chatUnfavoriteKind   = "chat_unfavorite"

	chatListCreateKind = "chat_list_create"
	chatListRenameKind = "chat_list_rename"
	chatListDeleteKind = "chat_list_delete"
	chatListAddKind    = "chat_list_add"
	chatListRemoveKind = "chat_list_remove"

	messageStarKind   = "message_star"
	messageUnstarKind = "message_unstar"
	messagePinKind    = "message_pin"
	messageUnpinKind  = "message_unpin"
	messageKeepKind   = "message_keep"
	messageUnkeepKind = "message_unkeep"

	sendContactKind = "contact"
	sendEventKind   = "event"
	// View-once sends have their own kinds: a sync process that predates them
	// must refuse them rather than send an ordinary, re-viewable file.
	fileViewOnceKind  = "file_view_once"
	voiceViewOnceKind = "voice_view_once"
)

var chatsMessagesKinds = map[string]bool{
	chatDeleteKind: true, chatClearKind: true, chatLockKind: true, chatUnlockKind: true,
	chatDisappearingKind: true, chatFavoriteKind: true, chatUnfavoriteKind: true,
	chatListCreateKind: true, chatListRenameKind: true, chatListDeleteKind: true,
	chatListAddKind: true, chatListRemoveKind: true,
	messageStarKind: true, messageUnstarKind: true, messagePinKind: true, messageUnpinKind: true,
	messageKeepKind: true, messageUnkeepKind: true,
	sendContactKind: true, sendEventKind: true,
}

func isChatsMessagesKind(kind string) bool { return chatsMessagesKinds[kind] }

// chatsMessagesApp is what these commands need from the process that owns the
// store: *app.App directly or in sync --follow, a fake in tests.
type chatsMessagesApp interface {
	messageMutationApp
	DeleteChat(context.Context, types.JID, bool) (int64, error)
	ClearChat(context.Context, types.JID, bool, bool) (int64, error)
	LockChat(context.Context, types.JID, bool) error
	SetChatFavorite(context.Context, types.JID, bool) error
	CreateChatList(context.Context, string) (store.ChatList, error)
	RenameChatList(context.Context, string, string) (store.ChatList, error)
	DeleteChatList(context.Context, string) (store.ChatList, error)
	SetChatListMember(context.Context, string, types.JID, bool) (store.ChatList, error)
	StarMessage(context.Context, app.MessageRef, bool) error
}

// executeDelegatedChatsMessages runs one of these kinds inside sync --follow.
// It cannot prompt, so an ambiguous chat name fails as with --json.
func executeDelegatedChatsMessages(ctx context.Context, a chatsMessagesApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	return runChatsMessagesAction(ctx, a, req, recipientOptions{pick: req.Pick, asJSON: true})
}

// runChatsMessagesAction is the command core shared by the direct and the
// delegated path, so both do and print the same thing.
func runChatsMessagesAction(ctx context.Context, a chatsMessagesApp, req sendDelegateRequest, ropts recipientOptions) (sendDelegateResponse, error) {
	switch req.Kind {
	case chatDeleteKind, chatClearKind, chatLockKind, chatUnlockKind, chatDisappearingKind, chatFavoriteKind, chatUnfavoriteKind:
		return runChatAction(ctx, a, req, ropts)
	case chatListCreateKind, chatListRenameKind, chatListDeleteKind:
		return runChatListDefinition(ctx, a, req)
	case chatListAddKind, chatListRemoveKind:
		return runChatListMembership(ctx, a, req, ropts)
	case messageStarKind, messageUnstarKind:
		return runMessageStar(ctx, a, req)
	case messagePinKind, messageUnpinKind, messageKeepKind, messageUnkeepKind:
		return runMessagePinOrKeep(ctx, a, req)
	case sendContactKind:
		return runSendContact(ctx, a, req, ropts)
	case sendEventKind:
		return runSendEvent(ctx, a, req, ropts)
	default:
		return sendDelegateResponse{}, fmt.Errorf("unsupported send kind %q", req.Kind)
	}
}

// runChatsMessagesCommand runs req directly, or through a same-store
// `sync --follow` that holds the store lock, and prints the result.
func runChatsMessagesCommand(flags *rootFlags, req sendDelegateRequest) error {
	if err := flags.requireWritable(); err != nil {
		return err
	}
	write := func(resp sendDelegateResponse) error { return writeChatsMessagesResult(flags, req.Kind, resp) }

	ctx, cancel := withTimeout(context.Background(), flags)
	defer cancel()

	a, lk, err := newApp(ctx, flags, true, false)
	if err != nil {
		return delegateAfterOpenFailure(ctx, flags, err, req, write)
	}
	defer closeApp(a, lk)

	if err := a.EnsureAuthed(ctx); err != nil {
		return err
	}
	removePersistenceHandler, err := a.AddChatStatePersistenceHandler(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// Keep the handler until the socket closes; App.Close drains persistence.
		a.WA().Disconnect()
		removePersistenceHandler()
	}()
	if err := a.Connect(ctx, false, nil); err != nil {
		return err
	}
	resp, err := runChatsMessagesAction(ctx, a, req, recipientOptions{pick: req.Pick, asJSON: flags.asJSON})
	if err != nil {
		return err
	}
	return write(resp)
}

// writeChatsMessagesResult prints a result the same way for both paths.
func writeChatsMessagesResult(flags *rootFlags, kind string, resp sendDelegateResponse) error {
	switch kind {
	case sendContactKind, sendEventKind, messagePinKind, messageUnpinKind:
		warnSendStoreFailureMsg(os.Stderr, resp.ID, resp.StoreWarning)
	}
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, chatsMessagesJSON(kind, resp))
	}
	fmt.Fprintln(os.Stdout, chatsMessagesHuman(kind, resp))
	return nil
}

func chatsMessagesJSON(kind string, resp sendDelegateResponse) map[string]any {
	switch kind {
	case chatListCreateKind, chatListRenameKind, chatListDeleteKind:
		return map[string]any{"ok": true, "action": resp.Action, "list": map[string]any{"id": resp.ListID, "name": resp.Name}}
	case chatListAddKind, chatListRemoveKind:
		return map[string]any{"ok": true, "action": resp.Action, "chat": resp.Chat, "list": map[string]any{"id": resp.ListID, "name": resp.Name}}
	case messageStarKind, messageUnstarKind:
		return map[string]any{"ok": true, "action": resp.Action, "chat": resp.Chat, "target": resp.Target}
	case messagePinKind, messageUnpinKind, messageKeepKind, messageUnkeepKind:
		body := map[string]any{"sent": true, "action": resp.Action, "to": resp.To, "id": resp.ID, "target": resp.Target}
		if resp.Duration != "" {
			body["duration"] = resp.Duration
		}
		return addStoreWarningString(body, resp.StoreWarning)
	case sendContactKind:
		return addStoreWarningString(map[string]any{"sent": true, "to": resp.To, "id": resp.ID, "contacts": resp.Count}, resp.StoreWarning)
	case sendEventKind:
		return addStoreWarningString(map[string]any{"sent": true, "to": resp.To, "id": resp.ID, "name": resp.Name}, resp.StoreWarning)
	}
	body := map[string]any{"ok": true, "action": resp.Action, "chat": resp.Chat}
	switch kind {
	case chatDeleteKind, chatClearKind:
		body["messages_hidden"] = resp.Count
	case chatDisappearingKind:
		body["duration"] = resp.Duration
	}
	return body
}

func chatsMessagesHuman(kind string, resp sendDelegateResponse) string {
	switch kind {
	case chatDeleteKind, chatClearKind:
		if resp.Count == 1 {
			return fmt.Sprintf("%s: %s (kept 1 local message as a tombstone)", resp.Action, resp.Chat)
		}
		return fmt.Sprintf("%s: %s (kept %d local messages as tombstones)", resp.Action, resp.Chat, resp.Count)
	case chatDisappearingKind:
		return fmt.Sprintf("%s: %s (%s)", resp.Action, resp.Chat, resp.Duration)
	case chatListCreateKind:
		return fmt.Sprintf("Created list %s (id %s)", resp.Name, resp.ListID)
	case chatListRenameKind:
		return fmt.Sprintf("Renamed list %s to %s", resp.ListID, resp.Name)
	case chatListDeleteKind:
		return fmt.Sprintf("Deleted list %s (id %s)", resp.Name, resp.ListID)
	case chatListAddKind:
		return fmt.Sprintf("Added %s to list %s (id %s)", resp.Chat, resp.Name, resp.ListID)
	case chatListRemoveKind:
		return fmt.Sprintf("Removed %s from list %s (id %s)", resp.Chat, resp.Name, resp.ListID)
	case messageStarKind:
		return fmt.Sprintf("Starred message %s in %s", resp.Target, resp.Chat)
	case messageUnstarKind:
		return fmt.Sprintf("Unstarred message %s in %s", resp.Target, resp.Chat)
	case messagePinKind:
		return fmt.Sprintf("Pinned message %s in %s for %s (id %s)", resp.Target, resp.To, resp.Duration, resp.ID)
	case messageUnpinKind:
		return fmt.Sprintf("Unpinned message %s in %s (id %s)", resp.Target, resp.To, resp.ID)
	case messageKeepKind:
		return fmt.Sprintf("Kept message %s in %s (id %s)", resp.Target, resp.To, resp.ID)
	case messageUnkeepKind:
		return fmt.Sprintf("Stopped keeping message %s in %s (id %s)", resp.Target, resp.To, resp.ID)
	case sendContactKind:
		if resp.Count == 1 {
			return fmt.Sprintf("Sent contact card to %s (id %s)", resp.To, resp.ID)
		}
		return fmt.Sprintf("Sent %d contact cards to %s (id %s)", resp.Count, resp.To, resp.ID)
	case sendEventKind:
		return fmt.Sprintf("Sent event %s to %s (id %s)", resp.Name, resp.To, resp.ID)
	default:
		return fmt.Sprintf("%s: %s", resp.Action, resp.Chat)
	}
}

func addStoreWarningString(body map[string]any, warning string) map[string]any {
	if warning != "" {
		body["store_warning"] = warning
	}
	return body
}

// actionName is the command name a kind reports as its action.
func actionName(kind string) string {
	switch kind {
	case chatListCreateKind:
		return "list-create"
	case chatListRenameKind:
		return "list-rename"
	case chatListDeleteKind:
		return "list-delete"
	case chatListAddKind:
		return "list-add"
	case chatListRemoveKind:
		return "list-remove"
	}
	for _, prefix := range []string{"chat_", "message_"} {
		if strings.HasPrefix(kind, prefix) {
			return strings.TrimPrefix(kind, prefix)
		}
	}
	return kind
}

func sendFileDelegateKind(viewOnce bool) string {
	if viewOnce {
		return fileViewOnceKind
	}
	return "file"
}

func sendVoiceDelegateKind(viewOnce bool) string {
	if viewOnce {
		return voiceViewOnceKind
	}
	return "voice"
}

// validateViewOnceTarget refuses view once where WhatsApp does not offer it,
// before anything is uploaded.
func validateViewOnceTarget(viewOnce bool, to types.JID, mediaType string, ptt bool) error {
	if !viewOnce {
		return nil
	}
	if to.Server == types.NewsletterServer || to == types.StatusBroadcastJID {
		return fmt.Errorf("view once is not supported for channels or status")
	}
	switch {
	case mediaType == "image", mediaType == "video", mediaType == "audio" && ptt:
		return nil
	default:
		return fmt.Errorf("view once only works for images, videos and voice notes, not %s", mediaType)
	}
}
