package wa

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/appstate/lthash"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waServerSync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/util/cbcutil"
	"go.mau.fi/whatsmeow/util/hkdfutil"
	"google.golang.org/protobuf/proto"
)

// contactActionVersion is the mutation version WhatsApp uses for entries at
// the "contact" index of critical_unblock_low.
const contactActionVersion = 2

// ErrNoSavedContact means the session's app state has no WhatsApp contact
// entry for the identity, so there is nothing to remove.
var ErrNoSavedContact = errors.New("no saved WhatsApp contact")

// ContactSaveRequest adds or renames one entry in the account's WhatsApp
// contacts, the address book WhatsApp syncs between linked devices.
type ContactSaveRequest struct {
	// JID is a phone-number JID, or a LID whose phone number the session knows.
	JID       types.JID
	FirstName string
	FullName  string
	// SaveOnPrimaryAddressbook asks the phone to also write the entry to its
	// own address book.
	SaveOnPrimaryAddressbook bool
}

// ContactSaveResult is the identity pair the saved entry refers to.
type ContactSaveResult struct {
	JID types.JID // phone-number JID the entry is stored under
	LID types.JID // paired LID; empty when the session does not know it
}

// ContactDeleteResult lists the entries that were removed.
type ContactDeleteResult struct {
	JID     types.JID   // phone-number JID when known, else the LID
	LID     types.JID   // paired LID; empty when unknown
	Removed []types.JID // entry identities that existed and were removed
}

// SaveContact writes a "contact" app-state entry for a phone number. WhatsApp
// keys these entries by phone-number JID and carries the LID in the action.
func (c *Client) SaveContact(ctx context.Context, req ContactSaveRequest) (ContactSaveResult, error) {
	cli, err := c.connectedSession()
	if err != nil {
		return ContactSaveResult{}, err
	}
	if strings.TrimSpace(req.FullName) == "" {
		return ContactSaveResult{}, fmt.Errorf("a contact name is required")
	}
	pn, lid, err := contactIdentities(ctx, cli, req.JID, true)
	if err != nil {
		return ContactSaveResult{}, err
	}
	if pn.IsEmpty() {
		return ContactSaveResult{}, fmt.Errorf("no phone number is known for %s; pass the contact's phone number", req.JID)
	}
	patch := buildContactSavePatch(pn, lid, req.FirstName, req.FullName, req.SaveOnPrimaryAddressbook)
	if err := cli.SendAppState(ctx, patch); err != nil {
		return ContactSaveResult{}, err
	}
	return ContactSaveResult{JID: pn, LID: lid}, nil
}

// DeleteContact removes the "contact" app-state entries stored for either
// identity of the target. It sends nothing when neither entry exists.
func (c *Client) DeleteContact(ctx context.Context, jid types.JID) (ContactDeleteResult, error) {
	cli, err := c.connectedSession()
	if err != nil {
		return ContactDeleteResult{}, err
	}
	pn, lid, err := contactIdentities(ctx, cli, jid, false)
	if err != nil {
		return ContactDeleteResult{}, err
	}
	result := ContactDeleteResult{JID: pn, LID: lid}
	if pn.IsEmpty() {
		result.JID = lid
	}
	var targets []types.JID
	for _, id := range []types.JID{pn, lid} {
		if !id.IsEmpty() {
			targets = append(targets, id)
		}
	}
	removed, err := sendAppStateRemoval(ctx, cli, appstate.WAPatchCriticalUnblockLow, contactRemovalMutations(targets))
	if err != nil {
		if errors.Is(err, errNothingToRemove) {
			return result, fmt.Errorf("%w for %s; nothing was changed", ErrNoSavedContact, result.JID)
		}
		return result, err
	}
	for _, index := range removed {
		id, parseErr := types.ParseJID(index[1])
		if parseErr != nil {
			continue
		}
		result.Removed = append(result.Removed, id)
		// whatsmeow applies contact SETs to its contact store but ignores
		// REMOVEs, so clear the saved names it would otherwise keep serving.
		if cli.Store != nil && cli.Store.Contacts != nil {
			_ = cli.Store.Contacts.PutContactName(ctx, id, "", "")
		}
	}
	return result, nil
}

// contactIdentities returns the phone-number and LID pair for a user JID from
// the session's mapping. lookup also asks WhatsApp for a phone number's LID
// when the session has none stored.
func contactIdentities(ctx context.Context, cli *whatsmeow.Client, jid types.JID, lookup bool) (pn, lid types.JID, err error) {
	jid = jid.ToNonAD()
	switch jid.Server {
	case types.DefaultUserServer:
		pn = jid
		var mapped types.JID
		if lookup {
			mapped = resolvePNToLID(ctx, cli, jid)
		} else if cli.Store != nil && cli.Store.LIDs != nil {
			mapped, _ = cli.Store.LIDs.GetLIDForPN(ctx, jid)
		}
		if mapped.Server == types.HiddenUserServer {
			lid = mapped.ToNonAD()
		}
	case types.HiddenUserServer:
		lid = jid
		if cli.Store != nil && cli.Store.LIDs != nil {
			mapped, _ := cli.Store.LIDs.GetPNForLID(ctx, jid)
			if mapped.Server == types.DefaultUserServer {
				pn = mapped.ToNonAD()
			}
		}
	default:
		return types.JID{}, types.JID{}, fmt.Errorf("unsupported contact %s: pass a phone number or user JID", jid)
	}
	return pn, lid, nil
}

// buildContactSavePatch builds the app-state patch that saves a contact under
// its phone-number JID, the way WhatsApp's own clients write it.
func buildContactSavePatch(pn, lid types.JID, firstName, fullName string, saveOnPrimary bool) appstate.PatchInfo {
	action := &waSyncAction.ContactAction{
		FullName: proto.String(strings.TrimSpace(fullName)),
	}
	if first := strings.TrimSpace(firstName); first != "" {
		action.FirstName = proto.String(first)
	}
	if !lid.IsEmpty() {
		action.LidJID = proto.String(lid.String())
	}
	if saveOnPrimary {
		action.SaveOnPrimaryAddressbook = proto.Bool(true)
	}
	return appstate.PatchInfo{
		Type: appstate.WAPatchCriticalUnblockLow,
		Mutations: []appstate.MutationInfo{{
			Index:   []string{appstate.IndexContact, pn.String()},
			Version: contactActionVersion,
			Value:   &waSyncAction.SyncActionValue{ContactAction: action},
		}},
	}
}

// contactRemovalMutations are REMOVE candidates for each identity's entry.
func contactRemovalMutations(targets []types.JID) []appstate.MutationInfo {
	mutations := make([]appstate.MutationInfo, 0, len(targets))
	for _, target := range targets {
		mutations = append(mutations, appstate.MutationInfo{
			Index:   []string{appstate.IndexContact, target.String()},
			Version: contactActionVersion,
			Value:   &waSyncAction.SyncActionValue{ContactAction: &waSyncAction.ContactAction{}},
		})
	}
	return mutations
}

var errNothingToRemove = errors.New("no existing app state entry to remove")

// sendAppStateRemoval sends REMOVE mutations for the candidates that exist in
// the session's app state and returns their indexes. whatsmeow's encoder only
// writes SET mutations, so the patch is encoded here with the same keys, MACs
// and LTHash update, then applied back from the server as whatsmeow does
// after its own sends.
func sendAppStateRemoval(ctx context.Context, cli *whatsmeow.Client, name appstate.WAPatchName, candidates []appstate.MutationInfo) ([][]string, error) {
	if cli.Store == nil {
		return nil, fmt.Errorf("session store not available")
	}
	stores := appStateStores{state: cli.Store.AppState, keys: cli.Store.AppStateKeys}
	for attempt := 0; ; attempt++ {
		removal, err := encodeAppStateRemoval(ctx, stores, name, candidates, time.Now())
		if err != nil {
			return nil, err
		}
		resp, err := cli.DangerousInternals().SendIQ(ctx, whatsmeow.DangerousInfoQuery{
			Namespace: "w:sync:app:state",
			Type:      "set",
			To:        types.ServerJID,
			Content: []waBinary.Node{{
				Tag: "sync",
				Content: []waBinary.Node{{
					Tag: "collection",
					Attrs: waBinary.Attrs{
						"name":            string(name),
						"version":         removal.fromVersion,
						"return_snapshot": false,
					},
					Content: []waBinary.Node{{Tag: "patch", Content: removal.patch}},
				}},
			}},
		})
		if err != nil {
			return nil, err
		}
		conflict, err := appStateSendError(resp, name)
		if conflict && attempt == 0 {
			// Another device changed the collection first: catch up, then
			// encode against the new state once.
			if fetchErr := cli.FetchAppState(ctx, name, false, false); fetchErr != nil {
				return nil, fmt.Errorf("%w (also, fetching the newer app state failed: %v)", err, fetchErr)
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := cli.FetchAppState(ctx, name, false, false); err != nil {
			return removal.removed, fmt.Errorf("the change was sent, but fetching app state after it failed: %w", err)
		}
		return removal.removed, nil
	}
}

// appStateSendError reports the error in an app-state send response and
// whether it is a version conflict worth one retry.
func appStateSendError(resp *waBinary.Node, name appstate.WAPatchName) (bool, error) {
	collection, ok := resp.GetOptionalChildByTag("sync", "collection")
	if !ok {
		return false, fmt.Errorf("app state send response has no collection")
	}
	if collection.AttrGetter().OptionalString("type") != "error" {
		return false, nil
	}
	errorTag, ok := collection.GetOptionalChildByTag("error")
	if !ok {
		return false, fmt.Errorf("%w: %s", whatsmeow.ErrAppStateUpdate, &collection)
	}
	err := fmt.Errorf("%w (%s): %s", whatsmeow.ErrAppStateUpdate, name, &errorTag)
	return errorTag.AttrGetter().Int("code") == 409, err
}

type appStateStores struct {
	state wastore.AppStateStore
	keys  wastore.AppStateSyncKeyStore
}

type appStateRemoval struct {
	patch       []byte
	fromVersion uint64
	removed     [][]string
}

// encodeAppStateRemoval encodes a patch removing the candidates that have a
// stored value, mirroring appstate.Processor.EncodePatch with REMOVE as the
// operation. It returns errNothingToRemove when none exist.
func encodeAppStateRemoval(ctx context.Context, st appStateStores, name appstate.WAPatchName, candidates []appstate.MutationInfo, now time.Time) (appStateRemoval, error) {
	version, hashState, err := st.state.GetAppStateVersion(ctx, string(name))
	if err != nil {
		return appStateRemoval{}, err
	}
	keyID, err := st.keys.GetLatestAppStateSyncKeyID(ctx)
	if err != nil {
		return appStateRemoval{}, fmt.Errorf("failed to get latest app state key ID: %w", err)
	} else if keyID == nil {
		return appStateRemoval{}, fmt.Errorf("no app state keys found")
	}
	keyData, err := st.keys.GetAppStateSyncKey(ctx, keyID)
	if err != nil {
		return appStateRemoval{}, fmt.Errorf("failed to get app state key: %w", err)
	} else if keyData == nil {
		return appStateRemoval{}, fmt.Errorf("app state key %X not found", keyID)
	}
	keys := expandMutationKeys(keyData.Data)

	var (
		mutations []*waServerSync.SyncdMutation
		valueMACs [][]byte
		previous  [][]byte
		removed   [][]string
	)
	for _, candidate := range candidates {
		indexBytes, err := json.Marshal(candidate.Index)
		if err != nil {
			return appStateRemoval{}, fmt.Errorf("failed to marshal mutation index: %w", err)
		}
		indexMAC := hmacSum(sha256.New, keys.index, indexBytes)
		prev, err := st.state.GetAppStateMutationMAC(ctx, string(name), indexMAC)
		if err != nil {
			return appStateRemoval{}, err
		}
		if prev == nil {
			continue
		}
		value := proto.Clone(candidate.Value).(*waSyncAction.SyncActionValue)
		value.Timestamp = proto.Int64(now.UnixMilli())
		mutationVersion := candidate.Version
		content, err := proto.Marshal(&waSyncAction.SyncActionData{
			Index:   indexBytes,
			Value:   value,
			Padding: []byte{},
			Version: &mutationVersion,
		})
		if err != nil {
			return appStateRemoval{}, fmt.Errorf("failed to marshal mutation: %w", err)
		}
		encrypted, err := cbcutil.Encrypt(keys.valueEncryption, nil, content)
		if err != nil {
			return appStateRemoval{}, fmt.Errorf("failed to encrypt mutation: %w", err)
		}
		valueMAC := contentMAC(waServerSync.SyncdMutation_REMOVE, encrypted, keyID, keys.valueMAC)
		mutations = append(mutations, &waServerSync.SyncdMutation{
			Operation: waServerSync.SyncdMutation_REMOVE.Enum(),
			Record: &waServerSync.SyncdRecord{
				Index: &waServerSync.SyncdIndex{Blob: indexMAC},
				Value: &waServerSync.SyncdValue{Blob: append(encrypted, valueMAC...)},
				KeyID: &waServerSync.KeyId{ID: keyID},
			},
		})
		valueMACs = append(valueMACs, valueMAC)
		previous = append(previous, prev)
		removed = append(removed, candidate.Index)
	}
	if len(mutations) == 0 {
		return appStateRemoval{}, errNothingToRemove
	}

	// A REMOVE takes the previous value out of the LTHash and adds nothing.
	lthash.WAPatchIntegrity.SubtractThenAddInPlace(hashState[:], previous, nil)
	newVersion := version + 1
	snapshotMAC := hmacSum(sha256.New, keys.snapshotMAC, hashState[:], uint64Bytes(newVersion), []byte(name))
	patchData := make([][]byte, 0, len(valueMACs)+3)
	patchData = append(patchData, snapshotMAC)
	patchData = append(patchData, valueMACs...)
	patchData = append(patchData, uint64Bytes(newVersion), []byte(name))
	patch := &waServerSync.SyncdPatch{
		SnapshotMAC: snapshotMAC,
		PatchMAC:    hmacSum(sha256.New, keys.patchMAC, patchData...),
		KeyID:       &waServerSync.KeyId{ID: keyID},
		Mutations:   mutations,
	}
	encoded, err := proto.Marshal(patch)
	if err != nil {
		return appStateRemoval{}, fmt.Errorf("failed to marshal compiled patch: %w", err)
	}
	return appStateRemoval{patch: encoded, fromVersion: version, removed: removed}, nil
}

type mutationKeys struct {
	index, valueEncryption, valueMAC, snapshotMAC, patchMAC []byte
}

func expandMutationKeys(keyData []byte) mutationKeys {
	expanded := hkdfutil.SHA256(keyData, nil, []byte("WhatsApp Mutation Keys"), 160)
	return mutationKeys{
		index:           expanded[0:32],
		valueEncryption: expanded[32:64],
		valueMAC:        expanded[64:96],
		snapshotMAC:     expanded[96:128],
		patchMAC:        expanded[128:160],
	}
}

func contentMAC(operation waServerSync.SyncdMutation_SyncdOperation, data, keyID, key []byte) []byte {
	return hmacSum(sha512.New, key, []byte{byte(operation) + 1}, keyID, data, uint64Bytes(uint64(len(keyID)+1)))[:32]
}

func hmacSum(alg func() hash.Hash, key []byte, data ...[]byte) []byte {
	h := hmac.New(alg, key)
	for _, item := range data {
		h.Write(item)
	}
	return h.Sum(nil)
}

func uint64Bytes(val uint64) []byte {
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, val)
	return data
}
