package app

import (
	"context"
	"errors"

	"go.mau.fi/whatsmeow/types"
)

func (f *fakeWA) NewsletterToggleMute(context.Context, types.JID, bool) error { return nil }

func (f *fakeWA) NewsletterSendReaction(context.Context, types.JID, types.MessageServerID, string) (types.MessageID, error) {
	return "REACT01", nil
}

func (f *fakeWA) GetNewsletterMessages(context.Context, types.JID, int, types.MessageServerID) ([]*types.NewsletterMessage, error) {
	return nil, nil
}

func (f *fakeWA) NewsletterMarkViewed(context.Context, types.JID, []types.MessageServerID) error {
	return nil
}

func (f *fakeWA) CreateNewsletter(context.Context, string, string) (*types.NewsletterMetadata, error) {
	return nil, errors.New("fake: no channel creation")
}

func (f *fakeWA) RejectCall(context.Context, types.JID, string) error { return nil }
