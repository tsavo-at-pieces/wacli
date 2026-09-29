package main

import (
	"context"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/types"
)

func canonicalCLIJID(jid types.JID) types.JID {
	if jid.Server == types.DefaultUserServer {
		return jid.ToNonAD()
	}
	return jid
}

// waStoreApp is the WhatsApp client and local store: *app.App, or a test fake.
type waStoreApp interface {
	WA() app.WAClient
	DB() *store.DB
}

// refreshGroupInfo stores a group's live info after a change. A failed lookup
// leaves the local snapshot as it was.
func refreshGroupInfo(ctx context.Context, a waStoreApp, gjid types.JID) {
	if info, err := a.WA().GetGroupInfo(ctx, gjid); err == nil && info != nil {
		_ = persistGroupInfo(ctx, a.DB(), a.WA(), info)
	}
}

type groupJIDResolver interface {
	ResolveLIDToPN(context.Context, types.JID) types.JID
}

func persistGroupInfo(ctx context.Context, db *store.DB, resolver groupJIDResolver, info *types.GroupInfo) error {
	if info == nil {
		return nil
	}
	ownerJID := canonicalCLIJID(resolver.ResolveLIDToPN(ctx, info.OwnerJID)).String()
	if err := db.UpsertGroupWithHierarchy(
		info.JID.String(),
		info.GroupName.Name,
		ownerJID,
		info.GroupCreated,
		info.IsParent,
		info.LinkedParentJID.String(),
	); err != nil {
		return err
	}
	var ps []store.GroupParticipant
	for _, p := range info.Participants {
		role := "member"
		if p.IsSuperAdmin {
			role = "superadmin"
		} else if p.IsAdmin {
			role = "admin"
		}
		ps = append(ps, store.GroupParticipant{
			GroupJID: info.JID.String(),
			UserJID:  canonicalCLIJID(resolver.ResolveLIDToPN(ctx, p.JID)).String(),
			Role:     role,
		})
	}
	return db.ReplaceGroupParticipants(info.JID.String(), ps)
}

func groupKindLabel(isParent bool, linkedParentJID string) string {
	if isParent {
		return "community"
	}
	if linkedParentJID != "" {
		return "subgroup"
	}
	return "group"
}
