package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/openclaw/wacli/internal/out"
	"github.com/spf13/cobra"
)

func newGroupsPhotoCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "photo",
		Short: "Set or remove a group's photo",
	}
	cmd.AddCommand(newGroupsPhotoSetCmd(flags))
	cmd.AddCommand(newGroupsPhotoRemoveCmd(flags))
	return cmd
}

func newGroupsPhotoSetCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	var filePath string
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Set the group photo (JPEG or PNG, auto-resized to <=640px)",
		Long: `Set the group photo from a local JPEG or PNG file.

The image is converted to JPEG and scaled down to at most 640px on its longer
side before upload, as "profile set-picture" does. When WACLI_MEDIA_ROOTS is
set, the file must be inside one of those directories.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" || strings.TrimSpace(filePath) == "" {
				return fmt.Errorf("--jid and --file are required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			if _, err := parseGroupJID(jidStr); err != nil {
				return err
			}
			if err := checkOutboundMediaPath(filePath); err != nil {
				return err
			}
			// Convert here, so a sync process running the upload gets JPEG
			// bytes and never opens a path from the request.
			photo, err := readAsJPEG(filePath)
			if err != nil {
				return fmt.Errorf("read image: %w", err)
			}
			return runLiveGroupCommand(flags, sendDelegateRequest{Kind: groupPhotoSetKind, To: jidStr, Photo: photo}, func(resp sendDelegateResponse) error {
				if flags.asJSON {
					return out.WriteJSON(os.Stdout, map[string]any{"jid": resp.Chat, "picture_id": resp.ID})
				}
				fmt.Fprintf(os.Stdout, "Group photo updated (id: %s)\n", sanitize(resp.ID))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "group JID (…@g.us)")
	cmd.Flags().StringVar(&filePath, "file", "", "path to a JPEG or PNG image")
	return cmd
}

func newGroupsPhotoRemoveCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Remove the group photo",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" {
				return fmt.Errorf("--jid is required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			if _, err := parseGroupJID(jidStr); err != nil {
				return err
			}
			return runLiveGroupCommand(flags, sendDelegateRequest{Kind: groupPhotoRemoveKind, To: jidStr}, func(resp sendDelegateResponse) error {
				if flags.asJSON {
					return out.WriteJSON(os.Stdout, map[string]any{"jid": resp.Chat, "removed": true})
				}
				fmt.Fprintln(os.Stdout, "Group photo removed.")
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "group JID (…@g.us)")
	return cmd
}

// executeGroupPhotoSet uploads req.Photo, JPEG bytes the caller prepared, as
// the group photo.
func executeGroupPhotoSet(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	gjid, err := parseGroupJID(req.To)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	if len(req.Photo) == 0 {
		return sendDelegateResponse{}, fmt.Errorf("no photo in the request")
	}
	pictureID, err := a.WA().SetGroupPhoto(ctx, gjid, req.Photo)
	if err != nil {
		return sendDelegateResponse{}, fmt.Errorf("set group photo: %w", err)
	}
	refreshGroupInfo(ctx, a, gjid)
	return sendDelegateResponse{OK: true, Chat: gjid.String(), ID: pictureID}, nil
}

// executeGroupPhotoRemove removes the group photo.
func executeGroupPhotoRemove(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	gjid, err := parseGroupJID(req.To)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	pictureID, err := a.WA().SetGroupPhoto(ctx, gjid, nil)
	if err != nil {
		return sendDelegateResponse{}, fmt.Errorf("remove group photo: %w", err)
	}
	refreshGroupInfo(ctx, a, gjid)
	return sendDelegateResponse{OK: true, Chat: gjid.String(), ID: pictureID}, nil
}
