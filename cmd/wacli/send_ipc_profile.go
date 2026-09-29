package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// Profile, privacy and WhatsApp-contact kinds delegated to a same-store
// `sync --follow`. As with the management kinds, every command has its own
// kind so a sync process that predates it rejects it without running it.
const (
	profileSetNameKind       = "profile_set_name"
	profileSetAboutKind      = "profile_set_about"
	profileSetPictureKind    = "profile_set_picture"
	profileRemovePictureKind = "profile_remove_picture"
	profilePictureInfoKind   = "profile_picture_info"
	profileGetAboutKind      = "profile_get_about"
	profileBusinessKind      = "profile_business"

	// contactsCheckKind, its "phones" request field and "contacts" reply
	// field match upstream wacli (#453), so mixed builds still interoperate.
	contactsCheckKind              = "contacts_check"
	contactsImportSystemKind       = "contacts_import_system"
	contactsImportSystemClearKind  = "contacts_import_system_clear"
	contactSaveKind                = "contact_save"
	contactDeleteKind              = "contact_delete"
	contactBlockKind               = "contact_block"
	contactUnblockKind             = "contact_unblock"
	contactsBlocklistKind          = "contacts_blocklist"
	privacyShowKind                = "privacy_show"
	privacySetKind                 = "privacy_set"
	privacyDisappearingDefaultKind = "privacy_disappearing_default"
	privacyStatusKind              = "privacy_status"
)

// delegatedOperation is one command's core. The direct command runs it with
// its own store and session; a same-store `sync --follow` runs the same
// function for the same request, so both paths behave alike.
type delegatedOperation[T any] func(ctx context.Context, a waStoreApp, req sendDelegateRequest) (T, error)

// delegatedCommand runs op directly, or has the sync process that holds the
// store lock run req.Kind, then prints the result with write either way.
type delegatedCommand[T any] struct {
	req sendDelegateRequest
	// live connects to WhatsApp before running op directly.
	live  bool
	op    delegatedOperation[T]
	write func(T) error
	// decode reads a delegated reply; nil reads the JSON payload.
	decode func(sendDelegateResponse) (T, error)
}

func (c delegatedCommand[T]) run(flags *rootFlags) error {
	ctx, cancel := withTimeout(context.Background(), flags)
	defer cancel()

	a, lk, err := newApp(ctx, flags, true, false)
	if err != nil {
		return delegateAfterOpenFailure(ctx, flags, err, c.req, func(resp sendDelegateResponse) error {
			decode := c.decode
			if decode == nil {
				decode = func(resp sendDelegateResponse) (T, error) { return decodeDelegatedPayload[T](c.req.Kind, resp) }
			}
			result, err := decode(resp)
			if err != nil {
				return err
			}
			return c.write(result)
		})
	}
	defer closeApp(a, lk)

	if c.live {
		if err := a.EnsureAuthed(ctx); err != nil {
			return err
		}
		if err := a.Connect(ctx, false, nil); err != nil {
			return err
		}
	}
	result, err := c.op(ctx, a, c.req)
	if err != nil {
		return err
	}
	return c.write(result)
}

// delegatedPayload wraps an operation's result for the caller.
func delegatedPayload[T any](result T, err error) (sendDelegateResponse, error) {
	if err != nil {
		return sendDelegateResponse{}, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return sendDelegateResponse{}, fmt.Errorf("encode result: %w", err)
	}
	return sendDelegateResponse{OK: true, Payload: raw}, nil
}

func decodeDelegatedPayload[T any](kind string, resp sendDelegateResponse) (T, error) {
	var result T
	if len(resp.Payload) == 0 {
		return result, fmt.Errorf("the running sync process returned no result for %s", kind)
	}
	if err := json.Unmarshal(resp.Payload, &result); err != nil {
		return result, fmt.Errorf("decode result from the running sync process: %w", err)
	}
	return result, nil
}

// executeDelegatedProfilePrivacy runs a profile, privacy or WhatsApp-contact
// kind inside the sync process that owns the store and session.
func executeDelegatedProfilePrivacy(ctx context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	switch req.Kind {
	case profileSetNameKind:
		return delegatedPayload(setProfileName(ctx, a, req))
	case profileSetAboutKind:
		return delegatedPayload(setProfileAbout(ctx, a, req))
	case profileSetPictureKind:
		return delegatedPayload(setProfilePicture(ctx, a, req))
	case profileRemovePictureKind:
		return delegatedPayload(removeProfilePicture(ctx, a, req))
	case profilePictureInfoKind:
		return delegatedPayload(fetchProfilePictureInfo(ctx, a, req))
	case profileGetAboutKind:
		return delegatedPayload(fetchTargetProfileAbout(ctx, a, req))
	case profileBusinessKind:
		return delegatedPayload(fetchBusinessProfile(ctx, a, req))
	case contactsCheckKind:
		results, err := checkContactRegistrations(ctx, a, req)
		if err != nil {
			return sendDelegateResponse{}, err
		}
		return sendDelegateResponse{OK: true, Contacts: results}, nil
	case contactsImportSystemKind:
		return delegatedPayload(importSystemNames(ctx, a, req))
	case contactsImportSystemClearKind:
		return delegatedPayload(clearSystemNames(ctx, a, req))
	case contactSaveKind:
		return delegatedPayload(saveWhatsAppContact(ctx, a, req))
	case contactDeleteKind:
		return delegatedPayload(deleteWhatsAppContact(ctx, a, req))
	case contactBlockKind, contactUnblockKind:
		return delegatedPayload(changeContactBlock(ctx, a, req))
	case contactsBlocklistKind:
		return delegatedPayload(fetchBlocklist(ctx, a, req))
	case privacyShowKind:
		return delegatedPayload(showPrivacySettings(ctx, a, req))
	case privacySetKind:
		return delegatedPayload(changePrivacySetting(ctx, a, req))
	case privacyDisappearingDefaultKind:
		return delegatedPayload(setDisappearingDefault(ctx, a, req))
	case privacyStatusKind:
		return delegatedPayload(fetchStatusPrivacy(ctx, a, req))
	default:
		return sendDelegateResponse{}, fmt.Errorf("unsupported send kind %q", req.Kind)
	}
}
