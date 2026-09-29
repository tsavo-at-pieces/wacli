package app

import (
	"context"

	"go.mau.fi/whatsmeow/types"
)

// fakeStatusChannelsState configures and records fakeWA's status methods.
// Guarded by fakeWA.mu.
type fakeStatusChannelsState struct {
	statusMuteCalls []fakeStatusMuteCall
	statusMuteErr   error
	// statusMuteEvent returns the app-state event whatsmeow would report after
	// the patch is applied.
	statusMuteEvent func(target types.JID, mute bool) any
}

type fakeStatusMuteCall struct {
	target types.JID
	mute   bool
}

func (f *fakeWA) MuteUserStatus(ctx context.Context, target types.JID, mute bool, beforeApply func()) ([]any, error) {
	f.mu.Lock()
	f.statusChannels.statusMuteCalls = append(f.statusChannels.statusMuteCalls, fakeStatusMuteCall{target: target, mute: mute})
	err := f.statusChannels.statusMuteErr
	eventCB := f.statusChannels.statusMuteEvent
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	beforeApply()
	if eventCB != nil {
		if evt := eventCB(target, mute); evt != nil {
			return []any{evt}, nil
		}
	}
	return nil, nil
}

func (f *fakeWA) statusMuteCalls() []fakeStatusMuteCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeStatusMuteCall(nil), f.statusChannels.statusMuteCalls...)
}

func (f *fakeWA) GetStatusPrivacy(context.Context) ([]types.StatusPrivacy, error) {
	return []types.StatusPrivacy{{Type: types.StatusPrivacyTypeContacts, IsDefault: true}}, nil
}
