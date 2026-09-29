package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

func newMessagesDeleteCmd(flags *rootFlags) *cobra.Command {
	var chat string
	var id string
	var forMe bool
	var deleteMedia bool
	postSendWait := postSendRetryReceiptWait

	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a message for everyone or for you",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(chat) == "" || strings.TrimSpace(id) == "" {
				return fmt.Errorf("--chat and --id are required")
			}
			if deleteMedia && !forMe {
				return fmt.Errorf("--delete-media requires --for-me")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}

			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				return delegateAfterOpenFailure(ctx, flags, err, sendDelegateRequest{
					Kind:           messageDeleteDelegateKind(forMe),
					To:             chat,
					ID:             id,
					DeleteMedia:    deleteMedia,
					PostSendWaitMS: durationMillis(postSendWait),
				}, func(resp sendDelegateResponse) error {
					return writeMessageDeleted(flags, messageDeleteResult{
						forMe:        forMe,
						chat:         resp.To,
						target:       resp.Target,
						id:           resp.ID,
						deletedMedia: resp.DeletedMedia,
					})
				})
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			msg, chatJID, err := loadMessageDeleteTarget(ctx, a, chat, id, forMe)
			if err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}
			res, err := deleteStoredMessage(ctx, a, msg, chatJID, forMe, deleteMedia)
			if err != nil {
				return err
			}

			waitForPostSendRetryReceipts(ctx, postSendWait)

			return writeMessageDeleted(flags, res)
		},
	}
	cmd.Flags().StringVar(&chat, "chat", "", "chat JID, phone number, or contact/group/chat name")
	cmd.Flags().StringVar(&id, "id", "", "message ID to delete")
	cmd.Flags().BoolVar(&forMe, "for-me", false, "delete the message only for this WhatsApp account")
	cmd.Flags().BoolVar(&deleteMedia, "delete-media", false, "also remove local media when used with --for-me")
	cmd.Flags().DurationVar(&postSendWait, "post-send-wait", postSendRetryReceiptWait, "keep the connection alive after delete so retry receipts can be handled (0 disables)")
	return cmd
}

// Delete for everyone and delete for me are separate kinds, so a sync process
// can never run one when the other was asked for.
func messageDeleteDelegateKind(forMe bool) string {
	if forMe {
		return messageDeleteForMeKind
	}
	return messageDeleteKind
}

// messageDeleteResult is what delete reports, directly or through a running
// sync process.
type messageDeleteResult struct {
	forMe        bool
	chat         string
	target       string
	id           string // the revoke message ID; empty for --for-me
	deletedMedia bool
}

// loadMessageDeleteTarget finds the stored message and checks that it can be
// deleted the requested way.
func loadMessageDeleteTarget(ctx context.Context, a messageTargetApp, chat, id string, forMe bool) (store.Message, types.JID, error) {
	msg, chatJID, err := loadMessageMutationTarget(ctx, a, chat, id)
	if err != nil {
		return store.Message{}, types.JID{}, err
	}
	if forMe {
		err = validateMessageCanDeleteForMe(msg)
	} else {
		err = validateMessageCanRevoke(msg)
	}
	if err != nil {
		return store.Message{}, types.JID{}, err
	}
	return msg, chatJID, nil
}

// deleteStoredMessage deletes a checked message on WhatsApp and records the
// deletion locally.
func deleteStoredMessage(ctx context.Context, a messageMutationApp, msg store.Message, chatJID types.JID, forMe, deleteMedia bool) (messageDeleteResult, error) {
	if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
		return messageDeleteResult{}, err
	}
	if forMe {
		return deleteStoredMessageForMe(ctx, a, msg, chatJID, deleteMedia)
	}
	sentID, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (types.MessageID, error) {
		return a.WA().RevokeMessage(ctx, chatJID, types.MessageID(msg.MsgID))
	})
	if err != nil {
		return messageDeleteResult{}, err
	}
	if err := a.DB().MarkMessageRevoked(msg.ChatJID, msg.MsgID); err != nil {
		return messageDeleteResult{}, fmt.Errorf("store deleted message state: %w", err)
	}
	return messageDeleteResult{chat: chatJID.String(), target: msg.MsgID, id: string(sentID)}, nil
}

func deleteStoredMessageForMe(ctx context.Context, a messageMutationApp, msg store.Message, chatJID types.JID, deleteMedia bool) (messageDeleteResult, error) {
	info, err := messageInfoForDeleteForMe(msg, chatJID)
	if err != nil {
		return messageDeleteResult{}, err
	}
	if _, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (struct{}, error) {
		return struct{}{}, a.WA().DeleteMessageForMe(ctx, info, deleteMedia)
	}); err != nil {
		return messageDeleteResult{}, err
	}
	mediaPaths, err := a.DB().MessageLocalMediaPaths(msg.ChatJID, msg.MsgID)
	if err != nil {
		return messageDeleteResult{}, fmt.Errorf("load local media paths: %w", err)
	}
	deletedMediaCount, deleteMediaErr := deleteLocalMediaPathsIfRequested(deleteMedia, mediaPaths)
	if deleteMediaErr != nil {
		if err := a.DB().MarkMessageDeletedForMePreserveMedia(msg.ChatJID, msg.MsgID); err != nil {
			return messageDeleteResult{}, fmt.Errorf("store deleted-for-me message state: %w", err)
		}
		return messageDeleteResult{}, fmt.Errorf("delete local media: %w", deleteMediaErr)
	}
	if deleteMedia && strings.TrimSpace(msg.LocalPath) != "" {
		if err := a.DB().ClearMessageLocalMedia(msg.ChatJID, msg.MsgID); err != nil {
			return messageDeleteResult{}, fmt.Errorf("clear deleted local media state: %w", err)
		}
	}
	if err := a.DB().MarkMessageDeletedForMe(msg.ChatJID, msg.MsgID, msg.SenderJID, msg.FromMe, time.Now().UTC()); err != nil {
		return messageDeleteResult{}, fmt.Errorf("store deleted-for-me message state: %w", err)
	}
	return messageDeleteResult{forMe: true, chat: chatJID.String(), target: msg.MsgID, deletedMedia: deletedMediaCount > 0}, nil
}

func writeMessageDeleted(flags *rootFlags, res messageDeleteResult) error {
	if res.forMe {
		if flags.asJSON {
			return out.WriteJSON(os.Stdout, map[string]any{
				"deleted_for_me": true,
				"to":             res.chat,
				"target":         res.target,
				"deleted_media":  res.deletedMedia,
			})
		}
		fmt.Fprintf(os.Stdout, "Deleted message %s for me in %s\n", res.target, res.chat)
		return nil
	}
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{
			"revoked": true,
			"to":      res.chat,
			"id":      res.id,
			"target":  res.target,
		})
	}
	fmt.Fprintf(os.Stdout, "Deleted message %s in %s (id %s)\n", res.target, res.chat, res.id)
	return nil
}

func newMessagesRevokeCmd(flags *rootFlags) *cobra.Command {
	var chat string
	var id string
	postSendWait := postSendRetryReceiptWait

	cmd := &cobra.Command{
		Use:   "revoke",
		Short: "Delete one of your sent messages for everyone",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(chat) == "" || strings.TrimSpace(id) == "" {
				return fmt.Errorf("--chat and --id are required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}

			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				return delegateAfterOpenFailure(ctx, flags, err, sendDelegateRequest{
					Kind:           messageRevokeKind,
					To:             chat,
					ID:             id,
					PostSendWaitMS: durationMillis(postSendWait),
				}, func(resp sendDelegateResponse) error {
					return writeMessageRevoked(flags, resp.To, resp.ID, id)
				})
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			target, err := loadMessageRevokeTarget(ctx, a, chat, id)
			if err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}
			sentID, err := revokeMessage(ctx, a, target, id)
			if err != nil {
				return err
			}

			waitForPostSendRetryReceipts(ctx, postSendWait)

			return writeMessageRevoked(flags, target.chat.String(), string(sentID), id)
		},
	}
	cmd.Flags().StringVar(&chat, "chat", "", "chat JID or phone number")
	cmd.Flags().StringVar(&id, "id", "", "message ID to revoke")
	cmd.Flags().DurationVar(&postSendWait, "post-send-wait", postSendRetryReceiptWait, "keep the connection alive after revoke so retry receipts can be handled (0 disables)")
	return cmd
}

// messageRevokeTarget is a checked stored message, or only its chat when this
// store never saw the message.
type messageRevokeTarget struct {
	msg   store.Message
	chat  types.JID
	found bool
}

func loadMessageRevokeTarget(ctx context.Context, a messageTargetApp, chat, id string) (messageRevokeTarget, error) {
	msg, chatJID, err := loadMessageMutationTarget(ctx, a, chat, id)
	if err == nil {
		if err := validateMessageCanRevoke(msg); err != nil {
			return messageRevokeTarget{}, err
		}
		return messageRevokeTarget{msg: msg, chat: chatJID, found: true}, nil
	}
	if !isNoRows(err) {
		return messageRevokeTarget{}, err
	}
	chatJID, parseErr := wa.ParseUserOrJID(chat)
	if parseErr != nil {
		return messageRevokeTarget{}, err
	}
	return messageRevokeTarget{chat: chatJID}, nil
}

// revokeMessage deletes the message for everyone and records it when stored.
func revokeMessage(ctx context.Context, a messageMutationApp, target messageRevokeTarget, id string) (types.MessageID, error) {
	if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
		return "", err
	}
	targetID := id
	if target.found {
		targetID = target.msg.MsgID
	}
	sentID, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (types.MessageID, error) {
		return a.WA().RevokeMessage(ctx, target.chat, types.MessageID(targetID))
	})
	if err != nil {
		return "", err
	}
	if target.found {
		if err := a.DB().MarkMessageRevoked(target.msg.ChatJID, target.msg.MsgID); err != nil {
			return "", fmt.Errorf("store deleted message state: %w", err)
		}
	}
	return sentID, nil
}

func writeMessageRevoked(flags *rootFlags, chat, sentID, id string) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{
			"revoked": true,
			"to":      chat,
			"id":      sentID,
			"target":  id,
		})
	}
	fmt.Fprintf(os.Stdout, "Revoked message %s in %s (id %s)\n", id, chat, sentID)
	return nil
}

func deleteLocalMediaPathsIfRequested(deleteMedia bool, paths []string) (int, error) {
	if !deleteMedia {
		return 0, nil
	}
	deleted := 0
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

func validateMessageCanRevoke(msg store.Message) error {
	if msg.Revoked {
		return fmt.Errorf("message %s is already deleted", msg.MsgID)
	}
	if msg.DeletedForMe {
		return fmt.Errorf("message %s was deleted for me", msg.MsgID)
	}
	if !msg.FromMe {
		return fmt.Errorf("message %s was not sent by me", msg.MsgID)
	}
	return nil
}

func validateMessageCanDeleteForMe(msg store.Message) error {
	if msg.Revoked {
		return fmt.Errorf("message %s is already deleted", msg.MsgID)
	}
	if msg.DeletedForMe {
		return fmt.Errorf("message %s was deleted for me", msg.MsgID)
	}
	return nil
}

func messageInfoForDeleteForMe(msg store.Message, chat types.JID) (types.MessageInfo, error) {
	sender := types.EmptyJID
	if strings.TrimSpace(msg.SenderJID) != "" {
		parsed, err := types.ParseJID(msg.SenderJID)
		if err != nil {
			return types.MessageInfo{}, fmt.Errorf("stored sender JID is invalid: %w", err)
		}
		sender = parsed
	} else if !msg.FromMe && chat.Server == types.DefaultUserServer {
		sender = chat
	}
	if !msg.FromMe && chat.Server == types.GroupServer && sender.IsEmpty() {
		return types.MessageInfo{}, fmt.Errorf("stored sender JID is required to delete a group message for me")
	}
	return types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     chat,
			Sender:   sender,
			IsFromMe: msg.FromMe,
			IsGroup:  chat.Server == types.GroupServer,
		},
		ID:        types.MessageID(msg.MsgID),
		Timestamp: msg.Timestamp,
	}, nil
}
