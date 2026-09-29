package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// fakeProfileApp stands in for the sync process that owns the store: the
// management fake with a WhatsApp client for profile, privacy and contacts.
type fakeProfileApp struct {
	*fakeManagementApp
	wa *fakeProfileWA
}

func (f *fakeProfileApp) WA() app.WAClient { return f.wa }

// fakeProfileWA records the WhatsApp calls; any other call panics through the
// embedded nil interface. Fictional data only.
type fakeProfileWA struct {
	app.WAClient
	log      *callLog
	mu       sync.Mutex
	privacy  types.PrivacySettings
	blocked  []types.JID
	business *types.BusinessProfile
}

func newFakeProfileWA(log *callLog) *fakeProfileWA {
	return &fakeProfileWA{
		log: log,
		privacy: types.PrivacySettings{
			LastSeen: types.PrivacySettingContacts, Online: types.PrivacySettingAll, Profile: types.PrivacySettingContacts,
			Status: types.PrivacySettingAll, ReadReceipts: types.PrivacySettingAll, GroupAdd: types.PrivacySettingContacts,
			CallAdd: types.PrivacySettingAll, Messages: types.PrivacySettingAll, Defense: types.PrivacySettingOff,
		},
		blocked:  []types.JID{types.NewJID("100000000001", types.HiddenUserServer), types.NewJID("12025550142", types.DefaultUserServer)},
		business: testBusinessProfile(),
	}
}

func testBusinessProfile() *types.BusinessProfile {
	return &types.BusinessProfile{
		JID:                   types.NewJID("15550000001", types.DefaultUserServer),
		Address:               "1 Fictional Way",
		Email:                 "shop@example.invalid",
		Categories:            []types.Category{{ID: "1", Name: "Test category"}},
		BusinessHoursTimeZone: "UTC",
	}
}

func testStatusPrivacy() []types.StatusPrivacy {
	return []types.StatusPrivacy{
		{Type: types.StatusPrivacyTypeContacts, IsDefault: true},
		{Type: types.StatusPrivacyTypeBlacklist, List: []types.JID{types.NewJID("15550000002", types.DefaultUserServer)}},
	}
}

var (
	profPN  = types.NewJID("15550000001", types.DefaultUserServer)
	profLID = types.NewJID("100000000001", types.HiddenUserServer)
)

func (f *fakeProfileWA) ResolveLIDToPN(_ context.Context, jid types.JID) types.JID {
	if jid.ToNonAD() == profLID {
		return profPN
	}
	return jid
}

func (f *fakeProfileWA) ResolvePNToLID(_ context.Context, jid types.JID) types.JID {
	if jid.ToNonAD() == profPN {
		return profLID
	}
	return jid
}

func (f *fakeProfileWA) SetProfileName(_ context.Context, name string) error {
	f.log.add("set-name %q", name)
	return nil
}

func (f *fakeProfileWA) SetStatusMessage(_ context.Context, msg string) error {
	f.log.add("set-about %q", msg)
	return nil
}

func (f *fakeProfileWA) SetProfilePicture(_ context.Context, avatar []byte) (string, error) {
	if avatar == nil {
		f.log.add("remove-picture")
		return "remove", nil
	}
	if !bytes.HasPrefix(avatar, []byte{0xff, 0xd8}) {
		return "", fmt.Errorf("not a JPEG")
	}
	f.log.add("set-picture jpeg")
	return "PIC01", nil
}

func (f *fakeProfileWA) GetProfilePictureInfo(_ context.Context, jid types.JID, preview bool, existingID string) (*types.ProfilePictureInfo, error) {
	f.log.add("picture-info %s preview=%t existing=%q", jid, preview, existingID)
	if existingID == "PIC02" {
		return nil, nil
	}
	return &types.ProfilePictureInfo{ID: "PIC02", URL: "https://example.invalid/pic.jpg", Type: "preview", DirectPath: "/v/fictional", Hash: []byte{1, 2, 3}}, nil
}

func (f *fakeProfileWA) GetUserInfo(_ context.Context, jids []types.JID) (map[types.JID]types.UserInfo, error) {
	f.log.add("user-info %v", jids)
	out := make(map[types.JID]types.UserInfo, len(jids))
	for _, jid := range jids {
		out[jid] = types.UserInfo{Status: "Fictional status"}
	}
	return out, nil
}

func (f *fakeProfileWA) GetBusinessProfile(_ context.Context, jid types.JID) (*types.BusinessProfile, error) {
	f.log.add("business %s", jid)
	return f.business, nil
}

func (f *fakeProfileWA) IsOnWhatsApp(_ context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error) {
	f.log.add("is-on-whatsapp %v", phones)
	return []types.IsOnWhatsAppResponse{{Query: "+12025550142", JID: types.NewJID("12025550142", types.DefaultUserServer), IsIn: true}}, nil
}

func (f *fakeProfileWA) SaveContact(_ context.Context, req wa.ContactSaveRequest) (wa.ContactSaveResult, error) {
	f.log.add("save-contact %s first=%q full=%q phone=%t", req.JID, req.FirstName, req.FullName, req.SaveOnPrimaryAddressbook)
	return wa.ContactSaveResult{JID: profPN, LID: profLID}, nil
}

func (f *fakeProfileWA) DeleteContact(_ context.Context, jid types.JID) (wa.ContactDeleteResult, error) {
	f.log.add("delete-contact %s", jid)
	return wa.ContactDeleteResult{JID: profPN, LID: profLID, Removed: []types.JID{profPN}}, nil
}

func (f *fakeProfileWA) UpdateBlocklist(_ context.Context, jid types.JID, action events.BlocklistChangeAction) (*types.Blocklist, error) {
	f.log.add("blocklist %s %s", action, jid)
	return &types.Blocklist{}, nil
}

func (f *fakeProfileWA) GetBlocklist(context.Context) (*types.Blocklist, error) {
	f.log.add("get-blocklist")
	return &types.Blocklist{DHash: "fictional", JIDs: f.blocked}, nil
}

func (f *fakeProfileWA) GetPrivacySettings(context.Context) (types.PrivacySettings, error) {
	f.log.add("get-privacy")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.privacy, nil
}

func (f *fakeProfileWA) SetPrivacySetting(_ context.Context, name types.PrivacySettingType, value types.PrivacySetting) (types.PrivacySettings, error) {
	f.log.add("set-privacy %s %s", name, value)
	return f.privacy, nil
}

func (f *fakeProfileWA) SetDefaultDisappearingTimer(_ context.Context, timer time.Duration) error {
	f.log.add("disappearing-default %s", timer)
	return nil
}

func (f *fakeProfileWA) GetStatusPrivacy(context.Context) ([]types.StatusPrivacy, error) {
	f.log.add("status-privacy")
	return testStatusPrivacy(), nil
}

// profileDaemon plays `sync --follow` for one store with the production
// delegate server and dispatcher.
type profileDaemon struct {
	storeDir string
	fake     *fakeProfileApp
	mu       sync.Mutex
	requests []sendDelegateRequest
}

func newFakeProfileApp(t *testing.T, storeDir string) *fakeProfileApp {
	mgmt := newFakeManagementApp(t, storeDir)
	return &fakeProfileApp{fakeManagementApp: mgmt, wa: newFakeProfileWA(mgmt.callLog)}
}

func startProfileDaemon(t *testing.T, seed func(*testing.T, *fakeProfileApp)) *profileDaemon {
	t.Helper()
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	storeDir := shortPresenceDelegateStoreDir(t)
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatalf("lock store: %v", err)
	}
	t.Cleanup(func() { _ = lk.Release() })
	d := &profileDaemon{storeDir: storeDir, fake: newFakeProfileApp(t, storeDir)}
	if seed != nil {
		seed(t, d.fake)
	}
	stop, err := startSendDelegateServerForStore(context.Background(), storeDir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		d.mu.Lock()
		d.requests = append(d.requests, req)
		d.mu.Unlock()
		if req.Version != sendDelegateVersion {
			return sendDelegateResponse{}, fmt.Errorf("unsupported send delegate version %d", req.Version)
		}
		return executeDelegatedManagement(ctx, d.fake, req)
	})
	if err != nil {
		t.Fatalf("start delegate server: %v", err)
	}
	t.Cleanup(stop)
	return d
}

func (d *profileDaemon) received() []sendDelegateRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.requests)
}

type profileCommandCase struct {
	name    string
	kind    string
	args    []string
	seed    func(*testing.T, *fakeProfileApp)
	request func(*testing.T, sendDelegateRequest)
	calls   []string
	human   string
	json    string
	after   func(*testing.T, *fakeProfileApp)
}

func tableText(t *testing.T, rows ...[]string) string {
	t.Helper()
	var buf bytes.Buffer
	w := newTableWriter(&buf)
	for _, row := range rows {
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func seedProfileContact(t *testing.T, f *fakeProfileApp) {
	t.Helper()
	if err := f.db.UpsertContact(mgmtPhone, "15550000001", "Sammy", "Old Name", "", ""); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
}

func wantContactName(jid, name string) func(*testing.T, *fakeProfileApp) {
	return func(t *testing.T, f *fakeProfileApp) {
		t.Helper()
		c, err := f.db.GetContact(jid)
		if err != nil || c.Name != name {
			t.Fatalf("contact %s = %+v, %v; want name %q", jid, c, err, name)
		}
	}
}

func wantBlocked(want bool, jids ...string) func(*testing.T, *fakeProfileApp) {
	return func(t *testing.T, f *fakeProfileApp) {
		t.Helper()
		for _, jid := range jids {
			got, err := f.db.AnyContactBlocked([]string{jid})
			if err != nil || got != want {
				t.Fatalf("blocked(%s) = %t, %v; want %t", jid, got, err, want)
			}
		}
	}
}

// profileCommandCases writes its input files under dir.
func profileCommandCases(t *testing.T) []profileCommandCase {
	dir := t.TempDir()
	picture := filepath.Join(dir, "avatar.png")
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{R: 200, A: 255})
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(picture, pngBuf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	systemInput := filepath.Join(dir, "contacts.json")
	if err := os.WriteFile(systemInput, []byte(`[{"full_name":"Sam Fictional","phones":["+1 555 000 0001"]}]`), 0o600); err != nil {
		t.Fatal(err)
	}

	pictureInfo := formatProfilePictureInfo(types.NewJID("12025550142", types.DefaultUserServer),
		&types.ProfilePictureInfo{ID: "PIC02", URL: "https://example.invalid/pic.jpg", Type: "preview", DirectPath: "/v/fictional", Hash: []byte{1, 2, 3}})
	one := 1
	importResult := systemImportResult{
		Applied: &one, Matched: 1,
		Matches: []systemContactMatch{{JID: mgmtPhone, Phone: "15550000001", CurrentName: "Old Name", SystemName: "Sam Fictional"}},
	}
	privacy := formatPrivacySettings(newFakeProfileWA(nil).privacy)
	statusPrivacy := []statusPrivacyOutput{{Type: "contacts", Default: true}, {Type: "blacklist", List: []string{mgmtOtherPhone}}}

	return []profileCommandCase{
		{
			name: "profile set-name",
			kind: profileSetNameKind,
			args: []string{"profile", "set-name", " Test Name "},
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.Name != "Test Name" {
					t.Fatalf("name = %q", req.Name)
				}
			},
			calls: []string{`set-name "Test Name"`},
			human: "Profile name updated.\n",
			json:  `{"success":true,"data":{"name":"Test Name"},"error":null}` + "\n",
		},
		{
			name: "profile set-about",
			kind: profileSetAboutKind,
			args: []string{"profile", "set-about", "Fictional <about>"},
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.Message != "Fictional <about>" {
					t.Fatalf("about = %q", req.Message)
				}
			},
			calls: []string{`set-about "Fictional <about>"`},
			human: "Profile About updated.\n",
			json:  directJSON(t, map[string]any{"about": "Fictional <about>"}),
		},
		{
			name: "profile set-picture",
			kind: profileSetPictureKind,
			args: []string{"profile", "set-picture", picture},
			request: func(t *testing.T, req sendDelegateRequest) {
				if !bytes.HasPrefix(req.ImageJPEG, []byte{0xff, 0xd8}) || req.To != "" {
					t.Fatalf("request = %d image bytes, to %q; want the converted JPEG and no path", len(req.ImageJPEG), req.To)
				}
			},
			calls: []string{"set-picture jpeg"},
			human: "Profile picture updated (id: PIC01)\n",
			json:  `{"success":true,"data":{"picture_id":"PIC01"},"error":null}` + "\n",
		},
		{
			name:  "profile remove-picture",
			kind:  profileRemovePictureKind,
			args:  []string{"profile", "remove-picture"},
			calls: []string{"remove-picture"},
			human: "Profile picture removed.\n",
			json:  `{"success":true,"data":{"picture_id":"remove","removed":true},"error":null}` + "\n",
		},
		{
			name: "profile picture-info",
			kind: profilePictureInfoKind,
			args: []string{"profile", "picture-info", "--jid", "+1 202 555 0142", "--preview"},
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.To != "+1 202 555 0142" || !req.Preview || req.ID != "" {
					t.Fatalf("request = %+v", req)
				}
			},
			calls: []string{`picture-info ` + mgmtDialed + ` preview=true existing=""`},
			human: "JID: " + mgmtDialed + "\nID: PIC02\nType: preview\nURL: https://example.invalid/pic.jpg\nDirect path: /v/fictional\n",
			json:  directJSON(t, pictureInfo),
		},
		{
			name:  "profile picture-info unchanged",
			kind:  profilePictureInfoKind,
			args:  []string{"profile", "picture-info", "--jid", mgmtDialed, "--existing-id", "PIC02"},
			calls: []string{`picture-info ` + mgmtDialed + ` preview=false existing="PIC02"`},
			human: mgmtDialed + " profile picture is unchanged.\n",
			json:  `{"success":true,"data":{"jid":"` + mgmtDialed + `","unchanged":true},"error":null}` + "\n",
		},
		{
			name:  "profile get-about",
			kind:  profileGetAboutKind,
			args:  []string{"profile", "get-about", "--jid", mgmtPhone},
			calls: []string{"user-info [" + mgmtPhone + "]"},
			human: "JID: " + mgmtPhone + "\nAbout: Fictional status\n",
			json:  directJSON(t, profileAboutOutput{JID: mgmtPhone, About: "Fictional status"}),
		},
		{
			name:  "profile business",
			kind:  profileBusinessKind,
			args:  []string{"profile", "business", "--jid", "+1 555 000 0001"},
			calls: []string{"business " + mgmtPhone},
			human: "JID: " + mgmtPhone + "\nAddress: 1 Fictional Way\nEmail: shop@example.invalid\nTimezone: UTC\n",
			json:  directJSON(t, formatBusinessProfile(testBusinessProfile())),
		},
		{
			name: "contacts check",
			kind: contactsCheckKind,
			args: []string{"contacts", "check", "+1 202 555 0142", "15550000002"},
			request: func(t *testing.T, req sendDelegateRequest) {
				if !slices.Equal(req.Phones, []string{"+1 202 555 0142", "15550000002"}) {
					t.Fatalf("phones = %q", req.Phones)
				}
			},
			calls: []string{"is-on-whatsapp [+12025550142 +15550000002]"},
			human: "12025550142\tregistered\n15550000002\tno response\n",
			json: directJSON(t, []contactCheckResult{
				{Query: "+1 202 555 0142", Phone: "12025550142", JID: mgmtDialed, Registered: true, Responded: true},
				{Query: "15550000002", Phone: "15550000002"},
			}),
		},
		{
			name: "contacts import-system",
			kind: contactsImportSystemKind,
			args: []string{"contacts", "import-system", "--input", systemInput},
			seed: seedProfileContact,
			request: func(t *testing.T, req sendDelegateRequest) {
				if !reflect.DeepEqual(req.SystemNames, map[string]string{"15550000001": "Sam Fictional"}) {
					t.Fatalf("system names = %v; want only the phone-to-name map", req.SystemNames)
				}
			},
			human: "Applied 1 system contact name(s).\n",
			json:  directJSON(t, importResult),
			after: wantContactName(mgmtPhone, "Sam Fictional"),
		},
		{
			name: "contacts import-system clear",
			kind: contactsImportSystemClearKind,
			args: []string{"contacts", "import-system", "--clear"},
			seed: func(t *testing.T, f *fakeProfileApp) {
				seedProfileContact(t, f)
				if err := f.db.SetSystemName(mgmtPhone, "Sam Fictional"); err != nil {
					t.Fatal(err)
				}
			},
			human: "Cleared 1 system contact name(s).\n",
			json:  `{"success":true,"data":{"cleared":1},"error":null}` + "\n",
			after: wantContactName(mgmtPhone, "Old Name"),
		},
		{
			name: "contacts save",
			kind: contactSaveKind,
			args: []string{"contacts", "save", "--phone", "+1 555 000 0001", "--first-name", "Sam", "--full-name", "Sam Fictional", "--save-to-phone"},
			seed: seedProfileContact,
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.To != "+1 555 000 0001" || req.FirstName != "Sam" || req.FullName != "Sam Fictional" || !req.SaveToPhone {
					t.Fatalf("request = %+v", req)
				}
			},
			calls: []string{`save-contact ` + mgmtPhone + ` first="Sam" full="Sam Fictional" phone=true`},
			human: "Saved " + mgmtPhone + " as Sam Fictional\n",
			json:  `{"success":true,"data":{"jid":"` + mgmtPhone + `","lid":"` + mgmtLID + `","full_name":"Sam Fictional","first_name":"Sam","save_to_phone":true},"error":null}` + "\n",
			after: wantContactName(mgmtPhone, "Sam Fictional"),
		},
		{
			name:  "contacts save first name only",
			kind:  contactSaveKind,
			args:  []string{"contacts", "save", "--jid", mgmtLID, "--first-name", "Sam"},
			calls: []string{`save-contact ` + mgmtLID + ` first="Sam" full="Sam" phone=false`},
			human: "Saved " + mgmtPhone + " as Sam\n",
			json:  `{"success":true,"data":{"jid":"` + mgmtPhone + `","lid":"` + mgmtLID + `","full_name":"Sam","first_name":"Sam","save_to_phone":false},"error":null}` + "\n",
			after: wantContactName(mgmtPhone, "Sam"),
		},
		{
			name:    "contacts delete",
			kind:    contactDeleteKind,
			args:    []string{"contacts", "delete", "--jid", mgmtLID},
			seed:    seedProfileContact,
			request: wantTo(mgmtLID),
			calls:   []string{"delete-contact " + mgmtLID},
			human:   "Deleted contact " + mgmtPhone + "\n",
			json:    `{"success":true,"data":{"jid":"` + mgmtPhone + `","lid":"` + mgmtLID + `","deleted":true,"removed":["` + mgmtPhone + `"]},"error":null}` + "\n",
			// The saved name is gone; the push name remains.
			after: wantContactName(mgmtPhone, "Sammy"),
		},
		{
			name:    "contacts block",
			kind:    contactBlockKind,
			args:    []string{"contacts", "block", "--jid", "+1 555 000 0001"},
			request: wantTo("+1 555 000 0001"),
			calls:   []string{"blocklist block " + mgmtPhone},
			human:   "Blocked " + mgmtPhone + "\n",
			json:    `{"success":true,"data":{"jid":"` + mgmtPhone + `","blocked":true},"error":null}` + "\n",
			after:   wantBlocked(true, mgmtPhone, mgmtLID),
		},
		{
			name: "contacts unblock",
			kind: contactUnblockKind,
			args: []string{"contacts", "unblock", "--jid", mgmtLID},
			seed: func(t *testing.T, f *fakeProfileApp) {
				if err := f.db.SetContactsBlocked([]string{mgmtPhone, mgmtLID}, true, time.Now()); err != nil {
					t.Fatal(err)
				}
			},
			calls: []string{"blocklist unblock " + mgmtLID},
			human: "Unblocked " + mgmtLID + "\n",
			json:  `{"success":true,"data":{"jid":"` + mgmtLID + `","blocked":false},"error":null}` + "\n",
			after: wantBlocked(false, mgmtPhone, mgmtLID),
		},
		{
			name: "contacts blocklist",
			kind: contactsBlocklistKind,
			args: []string{"contacts", "blocklist"},
			seed: func(t *testing.T, f *fakeProfileApp) {
				if err := f.db.SetContactsBlocked([]string{mgmtOtherPhone}, true, time.Now()); err != nil {
					t.Fatal(err)
				}
			},
			calls: []string{"get-blocklist"},
			human: tableText(t, []string{"JID", "PHONE JID"}, []string{mgmtLID, mgmtPhone}, []string{mgmtDialed, ""}),
			json:  `{"success":true,"data":{"count":2,"blocked":[{"jid":"` + mgmtLID + `","phone_jid":"` + mgmtPhone + `"},{"jid":"` + mgmtDialed + `"}]},"error":null}` + "\n",
			after: func(t *testing.T, f *fakeProfileApp) {
				wantBlocked(true, mgmtLID, mgmtPhone, mgmtDialed)(t, f)
				// The fetched list replaces what was stored.
				wantBlocked(false, mgmtOtherPhone)(t, f)
			},
		},
		{
			name:  "privacy show",
			kind:  privacyShowKind,
			args:  []string{"privacy", "show"},
			calls: []string{"get-privacy"},
			human: tableText(t,
				[]string{"SETTING", "VALUE"}, []string{"last-seen", "contacts"}, []string{"online", "all"}, []string{"profile-photo", "contacts"},
				[]string{"about", "all"}, []string{"read-receipts", "all"}, []string{"group-add", "contacts"}, []string{"call-add", "all"},
				[]string{"messages", "all"}, []string{"defense", "off"}, []string{"stickers", "-"}),
			json: directJSON(t, privacy),
		},
		{
			name: "privacy set",
			kind: privacySetKind,
			args: []string{"privacy", "set", "last-seen", "none"},
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.PrivacySetting != "last-seen" || req.PrivacyValue != "none" {
					t.Fatalf("request = %+v", req)
				}
			},
			calls: []string{"get-privacy", "set-privacy last none"},
			human: "last-seen: contacts -> none\n",
			json:  `{"success":true,"data":{"setting":"last-seen","value":"none","previous":"contacts"},"error":null}` + "\n",
		},
		{
			name:  "privacy set by wire name",
			kind:  privacySetKind,
			args:  []string{"privacy", "set", "status", "Contacts"},
			calls: []string{"get-privacy", "set-privacy status contacts"},
			human: "about: all -> contacts\n",
			json:  `{"success":true,"data":{"setting":"about","value":"contacts","previous":"all"},"error":null}` + "\n",
		},
		{
			name: "privacy disappearing-default",
			kind: privacyDisappearingDefaultKind,
			args: []string{"privacy", "disappearing-default", "--duration", "7d"},
			request: func(t *testing.T, req sendDelegateRequest) {
				if req.DisappearingTimer != "7d" {
					t.Fatalf("timer = %q", req.DisappearingTimer)
				}
			},
			calls: []string{"disappearing-default 168h0m0s"},
			human: "Default disappearing timer set to 7d.\n",
			json:  `{"success":true,"data":{"duration":"7d","seconds":604800},"error":null}` + "\n",
		},
		{
			name:  "privacy disappearing-default off",
			kind:  privacyDisappearingDefaultKind,
			args:  []string{"privacy", "disappearing-default", "--duration", "0"},
			calls: []string{"disappearing-default 0s"},
			human: "Default disappearing messages turned off.\n",
			json:  `{"success":true,"data":{"duration":"off","seconds":0},"error":null}` + "\n",
		},
		{
			name:  "privacy status",
			kind:  privacyStatusKind,
			args:  []string{"privacy", "status"},
			calls: []string{"status-privacy"},
			human: tableText(t, []string{"TYPE", "DEFAULT", "PEOPLE"}, []string{"contacts", "true", "0"}, []string{"blacklist", "false", "1"}),
			json:  directJSON(t, statusPrivacy),
		},
	}
}

// With the store locked by sync --follow, each command runs in that process
// and prints what the direct command prints, with and without --json.
func TestProfilePrivacyContactCommandsDelegateToFollowProcess(t *testing.T) {
	for _, tc := range profileCommandCases(t) {
		for _, asJSON := range []bool{false, true} {
			mode := "human"
			if asJSON {
				mode = "json"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				d := startProfileDaemon(t, tc.seed)
				global := []string{"--store", d.storeDir, "--timeout", "5s"}
				if asJSON {
					global = append(global, "--json")
				}
				stdout, stderr, err := runPresenceDelegateHelper(t, append(global, tc.args...))
				stdout = strings.TrimSuffix(stdout, "PASS\n")
				if err != nil {
					t.Fatalf("command failed: %v stdout=%q stderr=%q", err, stdout, stderr)
				}
				if strings.Contains(stderr, "store is locked") || strings.Contains(stderr, "warning") {
					t.Fatalf("unexpected stderr: %q", stderr)
				}
				want := tc.human
				if asJSON {
					want = tc.json
				}
				if stdout != want {
					t.Fatalf("stdout = %q\nwant     %q", stdout, want)
				}
				reqs := d.received()
				if len(reqs) != 1 {
					t.Fatalf("sync process received %d requests, want 1", len(reqs))
				}
				req := reqs[0]
				if req.Version != sendDelegateVersion || req.Kind != tc.kind {
					t.Fatalf("version/kind = %d/%q, want %d/%q", req.Version, req.Kind, sendDelegateVersion, tc.kind)
				}
				if req.TimeoutMS != 5000 || req.DeadlineUnixMS == 0 {
					t.Fatalf("request budget = %dms, deadline %d", req.TimeoutMS, req.DeadlineUnixMS)
				}
				if tc.request != nil {
					tc.request(t, req)
				}
				if got := d.fake.list(); !slices.Equal(got, tc.calls) {
					t.Fatalf("sync process calls = %q\nwant %q", got, tc.calls)
				}
				if tc.after != nil {
					tc.after(t, d.fake)
				}
			})
		}
	}
}

// A sync process started before a kind existed rejects it without running it.
func TestProfilePrivacyContactCommandsExplainOlderFollowProcessRejection(t *testing.T) {
	for _, tc := range profileCommandCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			skipPresenceDelegateSocketTestOnUnsupportedOS(t)
			storeDir := shortPresenceDelegateStoreDir(t)
			lk, err := lock.Acquire(storeDir)
			if err != nil {
				t.Fatalf("lock store: %v", err)
			}
			defer lk.Release()
			server := startPresenceDelegateTestSocket(t, storeDir, func(req sendDelegateRequest) sendDelegateResponse {
				return sendDelegateResponse{OK: false, Error: fmt.Sprintf("unsupported send kind %q", req.Kind)}
			})
			defer server.stop()

			stdout, stderr, err := runPresenceDelegateHelper(t, append([]string{"--store", storeDir, "--timeout", "5s"}, tc.args...))
			if err == nil || stdout != "" {
				t.Fatalf("older sync process: err=%v stdout=%q, want a failure and no success output", err, stdout)
			}
			if req := server.nextRequest(t); req.Kind != tc.kind {
				t.Fatalf("kind = %q, want %q", req.Kind, tc.kind)
			}
			for _, want := range []string{"does not support this command and did not run it", "restart `wacli sync`", fmt.Sprintf("unsupported send kind %q", tc.kind)} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr = %q, missing %q", stderr, want)
				}
			}
		})
	}
}

// Read-only mode stops every command before it reaches a running sync
// process, including live lookups, which open the session store directly.
func TestProfilePrivacyContactCommandsHonorReadOnlyBeforeDelegating(t *testing.T) {
	for _, viaEnv := range []bool{false, true} {
		for _, tc := range profileCommandCases(t) {
			name := tc.name + "/flag"
			if viaEnv {
				name = tc.name + "/env"
			}
			t.Run(name, func(t *testing.T) {
				d := startProfileDaemon(t, tc.seed)
				args := []string{"--store", d.storeDir, "--timeout", "5s"}
				if viaEnv {
					t.Setenv("WACLI_READONLY", "1")
				} else {
					args = append(args, "--read-only")
				}
				var err error
				captureRootStderr(t, func() {
					captureRootStdout(t, func() { err = execute(append(args, tc.args...)) })
				})
				if err == nil || !strings.Contains(err.Error(), "read-only mode") {
					t.Fatalf("error = %v, want read-only rejection", err)
				}
				if reqs := d.received(); len(reqs) != 0 {
					t.Fatalf("read-only command reached the sync process: %+v", reqs)
				}
				if calls := d.fake.list(); len(calls) != 0 {
					t.Fatalf("read-only command ran %q", calls)
				}
			})
		}
	}
}

func TestProfileCommandWithoutFollowProcessKeepsLockError(t *testing.T) {
	storeDir := shortPresenceDelegateStoreDir(t)
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatalf("lock store: %v", err)
	}
	defer lk.Release()
	for _, args := range [][]string{{"privacy", "show"}, {"contacts", "save", "--jid", mgmtPhone, "--full-name", "Sam"}, {"profile", "set-name", "Test"}} {
		stdout, stderr, err := runPresenceDelegateHelper(t, append([]string{"--store", storeDir, "--timeout", "750ms"}, args...))
		if err == nil || stdout != "" || !strings.Contains(stderr, "store is locked") || strings.Contains(stderr, "send delegate unavailable") {
			t.Fatalf("%v: err=%v stdout=%q stderr=%q, want the original lock error", args, err, stdout, stderr)
		}
	}
}

// Dry runs and local reads never need the lock, so they run next to sync
// without delegating.
func TestContactsImportSystemDryRunDoesNotDelegate(t *testing.T) {
	d := startProfileDaemon(t, seedProfileContact)
	input := filepath.Join(t.TempDir(), "contacts.json")
	if err := os.WriteFile(input, []byte(`[{"full_name":"Sam Fictional","phones":["+15550000001"]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"contacts", "import-system", "--input", input, "--dry-run"}, {"contacts", "import-system", "--clear", "--dry-run"}} {
		stdout, stderr, err := runPresenceDelegateHelper(t, append([]string{"--store", d.storeDir, "--timeout", "5s", "--json"}, args...))
		if err != nil {
			t.Fatalf("%v: %v stderr=%q", args, err, stderr)
		}
		if !strings.Contains(stdout, `"dry_run":true`) {
			t.Fatalf("%v: stdout = %q", args, stdout)
		}
	}
	if reqs := d.received(); len(reqs) != 0 {
		t.Fatalf("dry runs delegated %+v", reqs)
	}
	wantContactName(mgmtPhone, "Old Name")(t, d.fake)
}

func TestContactsShowReportsLocalBlockState(t *testing.T) {
	d := startProfileDaemon(t, func(t *testing.T, f *fakeProfileApp) {
		seedProfileContact(t, f)
		if err := f.db.SetContactsBlocked([]string{mgmtPhone}, true, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := f.db.UpsertContact(mgmtOtherPhone, "15550000002", "Other", "", "", ""); err != nil {
			t.Fatal(err)
		}
	})
	stdout, stderr, err := runPresenceDelegateHelper(t, []string{"--store", d.storeDir, "--json", "contacts", "show", "--jid", mgmtPhone})
	if err != nil || !strings.Contains(stdout, `"blocked":true`) {
		t.Fatalf("show blocked: err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
	stdout, _, err = runPresenceDelegateHelper(t, []string{"--store", d.storeDir, "contacts", "show", "--jid", mgmtPhone})
	if err != nil || !strings.Contains(stdout, "Blocked: yes\n") {
		t.Fatalf("show blocked human: err=%v stdout=%q", err, stdout)
	}
	stdout, _, err = runPresenceDelegateHelper(t, []string{"--store", d.storeDir, "--json", "contacts", "show", "--jid", mgmtOtherPhone})
	if err != nil || strings.Contains(stdout, "blocked") {
		t.Fatalf("show unblocked: err=%v stdout=%q, want no blocked key", err, stdout)
	}
	if reqs := d.received(); len(reqs) != 0 {
		t.Fatalf("contacts show delegated %+v", reqs)
	}
}

// The production dispatcher sends every new kind on to its executor.
func TestExecuteDelegatedSendRoutesProfilePrivacyContactKinds(t *testing.T) {
	a, err := app.New(app.Options{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	t.Cleanup(a.Close)
	for _, tc := range profileCommandCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			defer func() { _ = recover() }()
			_, err := executeDelegatedSend(context.Background(), a, sendDelegateRequest{Version: sendDelegateVersion, Kind: tc.kind, TimeoutMS: 200})
			if err != nil && strings.Contains(err.Error(), "unsupported send kind") {
				t.Fatalf("kind %q rejected: %v", tc.kind, err)
			}
		})
	}
}

func TestExecuteDelegatedProfilePrivacyRejectsUnknownKind(t *testing.T) {
	fake := newFakeProfileApp(t, t.TempDir())
	for _, kind := range []string{"profile_set", "privacy_set_all", "contact_block_all", ""} {
		_, err := executeDelegatedProfilePrivacy(context.Background(), fake, sendDelegateRequest{Kind: kind})
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("unsupported send kind %q", kind)) {
			t.Fatalf("kind %q: error = %v", kind, err)
		}
	}
	if calls := fake.list(); len(calls) != 0 {
		t.Fatalf("unknown kinds ran %q", calls)
	}
}

// The sync process validates requests itself before touching WhatsApp, so a
// malformed or foreign request changes nothing.
func TestExecuteDelegatedProfilePrivacyValidatesBeforeWhatsApp(t *testing.T) {
	fake := newFakeProfileApp(t, t.TempDir())
	tests := []struct {
		req  sendDelegateRequest
		want string
	}{
		{sendDelegateRequest{Kind: profileSetNameKind, Name: "  "}, "profile name is required"},
		{sendDelegateRequest{Kind: profileSetAboutKind}, "about text is required"},
		{sendDelegateRequest{Kind: profileSetPictureKind}, "image is required"},
		{sendDelegateRequest{Kind: profileGetAboutKind}, "--jid is required"},
		{sendDelegateRequest{Kind: contactsCheckKind}, "at least one phone is required"},
		{sendDelegateRequest{Kind: contactsCheckKind, Phones: []string{mgmtGroup}}, "unsupported recipient"},
		{sendDelegateRequest{Kind: contactSaveKind, To: mgmtPhone}, "--full-name or --first-name is required"},
		{sendDelegateRequest{Kind: contactSaveKind, To: mgmtGroup, FullName: "Group"}, "unsupported contact"},
		{sendDelegateRequest{Kind: contactDeleteKind, To: "120363000000000001@newsletter"}, "unsupported contact"},
		{sendDelegateRequest{Kind: contactBlockKind}, "--jid is required"},
		{sendDelegateRequest{Kind: privacySetKind, PrivacySetting: "last-seen", PrivacyValue: "match_last_seen"}, `invalid value "match_last_seen" for last-seen`},
		{sendDelegateRequest{Kind: privacySetKind, PrivacySetting: "hidden", PrivacyValue: "all"}, `unknown privacy setting "hidden"`},
		{sendDelegateRequest{Kind: privacyDisappearingDefaultKind, DisappearingTimer: "30d"}, `invalid --duration "30d"`},
		{sendDelegateRequest{Kind: privacyDisappearingDefaultKind}, `invalid --duration ""`},
	}
	for _, tt := range tests {
		_, err := executeDelegatedProfilePrivacy(context.Background(), fake, tt.req)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Fatalf("%+v: error = %v, want %q", tt.req, err, tt.want)
		}
	}
	if calls := fake.list(); len(calls) != 0 {
		t.Fatalf("rejected requests reached WhatsApp: %q", calls)
	}
}

// The CLI rejects bad input before it opens the store or delegates.
func TestProfilePrivacyContactCommandsValidateBeforeDelegating(t *testing.T) {
	d := startProfileDaemon(t, nil)
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"contacts", "save", "--full-name", "Sam"}, "--jid or --phone is required"},
		{[]string{"contacts", "save", "--jid", mgmtPhone, "--phone", "+15550000001", "--full-name", "Sam"}, "pass --jid or --phone, not both"},
		{[]string{"contacts", "save", "--phone", mgmtPhone, "--full-name", "Sam"}, "--phone takes a phone number"},
		{[]string{"contacts", "save", "--jid", mgmtPhone}, "--full-name or --first-name is required"},
		{[]string{"contacts", "save", "--jid", mgmtGroup, "--full-name", "Group"}, "unsupported contact"},
		{[]string{"contacts", "delete"}, "--jid is required"},
		{[]string{"contacts", "block", "--jid", mgmtGroup}, "unsupported contact"},
		{[]string{"contacts", "unblock"}, "--jid is required"},
		{[]string{"privacy", "set", "last-seen", "everyone"}, `invalid value "everyone" for last-seen; use one of: all, contacts, contact_blacklist, none`},
		{[]string{"privacy", "set", "group-add", "none"}, "use one of: all, contacts, contact_blacklist"},
		{[]string{"privacy", "set", "typing", "all"}, `unknown privacy setting "typing"`},
		{[]string{"privacy", "disappearing-default"}, "--duration is required"},
		{[]string{"privacy", "disappearing-default", "--duration", "1h"}, `invalid --duration "1h"`},
		{[]string{"profile", "picture-info"}, "--jid is required"},
		{[]string{"profile", "set-name", "  "}, "profile name is required"},
	}
	for _, tt := range tests {
		stdout, stderr, err := runPresenceDelegateHelper(t, append([]string{"--store", d.storeDir, "--timeout", "5s"}, tt.args...))
		if err == nil || stdout != "" || !strings.Contains(stderr, tt.want) {
			t.Fatalf("%v: err=%v stdout=%q stderr=%q, want %q", tt.args, err, stdout, stderr, tt.want)
		}
	}
	if reqs := d.received(); len(reqs) != 0 {
		t.Fatalf("invalid commands were delegated: %+v", reqs)
	}
}

func TestParsePrivacyChangeAcceptsEveryWhatsmeowSetting(t *testing.T) {
	want := map[types.PrivacySettingType]string{
		types.PrivacySettingTypeLastSeen: "last-seen", types.PrivacySettingTypeOnline: "online",
		types.PrivacySettingTypeProfile: "profile-photo", types.PrivacySettingTypeStatus: "about",
		types.PrivacySettingTypeReadReceipts: "read-receipts", types.PrivacySettingTypeGroupAdd: "group-add",
		types.PrivacySettingTypeCallAdd: "call-add", types.PrivacySettingTypeMessages: "messages",
		types.PrivacySettingTypeDefense: "defense", types.PrivacySettingTypeStickers: "stickers",
	}
	if len(privacySettingSpecs) != len(want) {
		t.Fatalf("specs = %d, want %d", len(privacySettingSpecs), len(want))
	}
	for wire, name := range want {
		for _, input := range []string{name, string(wire), strings.ToUpper(strings.ReplaceAll(name, "-", "_"))} {
			spec, _, err := parsePrivacyChange(input, "")
			if spec.wire != wire || err == nil || !strings.Contains(err.Error(), "invalid value") {
				t.Fatalf("%q: spec %q err %v", input, spec.wire, err)
			}
		}
	}
	// Every value is one whatsmeow documents for the category, and reading
	// a setting returns its own field.
	settings := types.PrivacySettings{
		GroupAdd: "g", LastSeen: "l", Status: "s", Profile: "p", ReadReceipts: "r",
		CallAdd: "c", Online: "o", Messages: "m", Defense: "d", Stickers: "k",
	}
	fields := map[string]types.PrivacySetting{
		"last-seen": "l", "online": "o", "profile-photo": "p", "about": "s", "read-receipts": "r",
		"group-add": "g", "call-add": "c", "messages": "m", "defense": "d", "stickers": "k",
	}
	for _, spec := range privacySettingSpecs {
		if got := spec.get(settings); got != fields[spec.name] {
			t.Fatalf("%s reads %q", spec.name, got)
		}
		for _, v := range spec.values {
			if _, got, err := parsePrivacyChange(spec.name, strings.ToUpper(string(v))); err != nil || got != v {
				t.Fatalf("%s %s: %q %v", spec.name, v, got, err)
			}
		}
	}
}

func TestParseDisappearingDefault(t *testing.T) {
	tests := map[string]disappearingDefaultResult{
		"0": {Duration: "off"}, "off": {Duration: "off"}, " OFF ": {Duration: "off"},
		"24h": {Duration: "24h", Seconds: 86400}, "1d": {Duration: "24h", Seconds: 86400},
		"7d": {Duration: "7d", Seconds: 604800}, "90d": {Duration: "90d", Seconds: 7776000},
	}
	for input, want := range tests {
		got, err := parseDisappearingDefault(input)
		if err != nil || got != want {
			t.Fatalf("%q = %+v, %v; want %+v", input, got, err, want)
		}
	}
	for _, input := range []string{"", "1h", "30d", "-1", "7"} {
		if _, err := parseDisappearingDefault(input); err == nil {
			t.Fatalf("%q accepted", input)
		}
	}
}

func TestSendDelegateRequestPreservesProfilePrivacyContactFieldsInJSON(t *testing.T) {
	req := sendDelegateRequest{
		Version: sendDelegateVersion, Kind: contactSaveKind, To: "+1 555 000 0001", Name: "Test", Message: "About", ID: "PIC02",
		Preview: true, ImageJPEG: []byte{0xff, 0xd8, 0x01}, Phones: []string{"+1 202 555 0142"},
		SystemNames: map[string]string{"15550000001": "Sam Fictional"}, FirstName: "Sam", FullName: "Sam Fictional", SaveToPhone: true,
		PrivacySetting: "last-seen", PrivacyValue: "none", DisappearingTimer: "7d",
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"preview":true`, `"image_jpeg":"/9gB"`, `"phones":["+1 202 555 0142"]`, `"system_names":{"15550000001":"Sam Fictional"}`,
		`"first_name":"Sam"`, `"full_name":"Sam Fictional"`, `"save_to_phone":true`, `"privacy_setting":"last-seen"`, `"privacy_value":"none"`, `"disappearing_timer":"7d"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("encoded request %s missing %s", raw, key)
		}
	}
	var got sendDelegateRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, req) {
		t.Fatalf("round trip = %+v\nwant %+v", got, req)
	}

	// The reply carries results unchanged, and upstream's contacts field.
	resp, err := delegatedPayload(formatBusinessProfile(testBusinessProfile()), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Contacts = []contactCheckResult{{Query: "+1 202 555 0142", Phone: "12025550142", Registered: true, Responded: true}}
	raw, err = json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var decoded sendDelegateResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	business, err := decodeDelegatedPayload[businessProfileOutput](profileBusinessKind, decoded)
	if err != nil || !reflect.DeepEqual(business, formatBusinessProfile(testBusinessProfile())) {
		t.Fatalf("business round trip = %+v, %v", business, err)
	}
	if !reflect.DeepEqual(decoded.Contacts, resp.Contacts) || !strings.Contains(string(raw), `"contacts":[{"query":"+1 202 555 0142"`) {
		t.Fatalf("contacts round trip = %s", raw)
	}
	if _, err := decodeDelegatedPayload[privacySetResult](privacySetKind, sendDelegateResponse{OK: true}); err == nil || !strings.Contains(err.Error(), "returned no result") {
		t.Fatalf("empty payload error = %v", err)
	}

	// Requests of the existing kinds do not grow new keys.
	raw, err = json.Marshal(sendDelegateRequest{Version: sendDelegateVersion, Kind: "text", To: mgmtPhone, Message: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"version":1,"kind":"text","to":"`+mgmtPhone+`","message":"hi"}` {
		t.Fatalf("text request = %s", raw)
	}
}
