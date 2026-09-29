package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

func newPrivacyCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "privacy",
		Short: "Show and change WhatsApp privacy settings",
	}
	cmd.AddCommand(newPrivacyShowCmd(flags))
	cmd.AddCommand(newPrivacySetCmd(flags))
	cmd.AddCommand(newPrivacyDisappearingDefaultCmd(flags))
	cmd.AddCommand(newPrivacyStatusCmd(flags))
	return cmd
}

// privacySettingSpec is one WhatsApp privacy setting: the name wacli uses,
// the category name on the wire, and the values WhatsApp accepts for it.
type privacySettingSpec struct {
	name   string
	wire   types.PrivacySettingType
	values []types.PrivacySetting
	get    func(types.PrivacySettings) types.PrivacySetting
}

var privacySettingSpecs = []privacySettingSpec{
	{"last-seen", types.PrivacySettingTypeLastSeen, []types.PrivacySetting{types.PrivacySettingAll, types.PrivacySettingContacts, types.PrivacySettingContactBlacklist, types.PrivacySettingNone},
		func(s types.PrivacySettings) types.PrivacySetting { return s.LastSeen }},
	{"online", types.PrivacySettingTypeOnline, []types.PrivacySetting{types.PrivacySettingAll, types.PrivacySettingMatchLastSeen},
		func(s types.PrivacySettings) types.PrivacySetting { return s.Online }},
	{"profile-photo", types.PrivacySettingTypeProfile, []types.PrivacySetting{types.PrivacySettingAll, types.PrivacySettingContacts, types.PrivacySettingContactBlacklist, types.PrivacySettingNone},
		func(s types.PrivacySettings) types.PrivacySetting { return s.Profile }},
	{"about", types.PrivacySettingTypeStatus, []types.PrivacySetting{types.PrivacySettingAll, types.PrivacySettingContacts, types.PrivacySettingContactBlacklist, types.PrivacySettingNone},
		func(s types.PrivacySettings) types.PrivacySetting { return s.Status }},
	{"read-receipts", types.PrivacySettingTypeReadReceipts, []types.PrivacySetting{types.PrivacySettingAll, types.PrivacySettingNone},
		func(s types.PrivacySettings) types.PrivacySetting { return s.ReadReceipts }},
	{"group-add", types.PrivacySettingTypeGroupAdd, []types.PrivacySetting{types.PrivacySettingAll, types.PrivacySettingContacts, types.PrivacySettingContactBlacklist},
		func(s types.PrivacySettings) types.PrivacySetting { return s.GroupAdd }},
	{"call-add", types.PrivacySettingTypeCallAdd, []types.PrivacySetting{types.PrivacySettingAll, types.PrivacySettingKnown},
		func(s types.PrivacySettings) types.PrivacySetting { return s.CallAdd }},
	{"messages", types.PrivacySettingTypeMessages, []types.PrivacySetting{types.PrivacySettingAll, types.PrivacySettingContacts},
		func(s types.PrivacySettings) types.PrivacySetting { return s.Messages }},
	{"defense", types.PrivacySettingTypeDefense, []types.PrivacySetting{types.PrivacySettingOnStandard, types.PrivacySettingOff},
		func(s types.PrivacySettings) types.PrivacySetting { return s.Defense }},
	{"stickers", types.PrivacySettingTypeStickers, []types.PrivacySetting{types.PrivacySettingContacts, types.PrivacySettingContactAllowlist, types.PrivacySettingNone},
		func(s types.PrivacySettings) types.PrivacySetting { return s.Stickers }},
}

func privacySettingNames() string {
	names := make([]string, len(privacySettingSpecs))
	for i, spec := range privacySettingSpecs {
		names[i] = spec.name
	}
	return strings.Join(names, ", ")
}

// parsePrivacyChange validates a setting and value. Settings are also
// accepted by their wire names (last, profile, status, ...) and with
// underscores.
func parsePrivacyChange(settingRaw, valueRaw string) (privacySettingSpec, types.PrivacySetting, error) {
	setting := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(settingRaw)), "_", "-")
	for _, spec := range privacySettingSpecs {
		if setting != spec.name && setting != string(spec.wire) {
			continue
		}
		value := types.PrivacySetting(strings.ToLower(strings.TrimSpace(valueRaw)))
		allowed := make([]string, len(spec.values))
		for i, v := range spec.values {
			if v == value {
				return spec, value, nil
			}
			allowed[i] = string(v)
		}
		return spec, "", fmt.Errorf("invalid value %q for %s; use one of: %s", valueRaw, spec.name, strings.Join(allowed, ", "))
	}
	return privacySettingSpec{}, "", fmt.Errorf("unknown privacy setting %q; use one of: %s", settingRaw, privacySettingNames())
}

// privacySettingsOutput maps each setting's JSON key (its wacli name with
// underscores) to its value.
type privacySettingsOutput map[string]string

func (spec privacySettingSpec) jsonKey() string {
	return strings.ReplaceAll(spec.name, "-", "_")
}

func formatPrivacySettings(s types.PrivacySettings) privacySettingsOutput {
	output := make(privacySettingsOutput, len(privacySettingSpecs))
	for _, spec := range privacySettingSpecs {
		output[spec.jsonKey()] = string(spec.get(s))
	}
	return output
}

func newPrivacyShowCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show WhatsApp privacy settings",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := flags.requireWritable(); err != nil {
				return err
			}
			return delegatedCommand[privacySettingsOutput]{
				req:  sendDelegateRequest{Kind: privacyShowKind},
				live: true,
				op:   showPrivacySettings,
				write: func(res privacySettingsOutput) error {
					if flags.asJSON {
						return out.WriteJSON(os.Stdout, res)
					}
					w := newTableWriter(os.Stdout)
					fmt.Fprintln(w, "SETTING\tVALUE")
					for _, spec := range privacySettingSpecs {
						value := res[spec.jsonKey()]
						if value == "" {
							value = "-"
						}
						fmt.Fprintf(w, "%s\t%s\n", spec.name, sanitize(value))
					}
					return w.Flush()
				},
			}.run(flags)
		},
	}
}

func showPrivacySettings(ctx context.Context, a waStoreApp, _ sendDelegateRequest) (privacySettingsOutput, error) {
	settings, err := a.WA().GetPrivacySettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("get privacy settings: %w", err)
	}
	return formatPrivacySettings(settings), nil
}

type privacySetResult struct {
	Setting  string `json:"setting"`
	Value    string `json:"value"`
	Previous string `json:"previous"`
}

func newPrivacySetCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "set <setting> <value>",
		Short: "Change one WhatsApp privacy setting",
		Long: "Change one WhatsApp privacy setting. Settings and their values:\n\n" +
			privacySettingHelp() + "\n" +
			"contact_blacklist (\"my contacts except\") and contact_allowlist use the\n" +
			"exception list already set on the phone; wacli cannot edit that list.\n" +
			"The output includes the previous value, so a change can be reverted.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, _, err := parsePrivacyChange(args[0], args[1]); err != nil {
				return err
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			return delegatedCommand[privacySetResult]{
				req:  sendDelegateRequest{Kind: privacySetKind, PrivacySetting: args[0], PrivacyValue: args[1]},
				live: true,
				op:   changePrivacySetting,
				write: func(res privacySetResult) error {
					if flags.asJSON {
						return out.WriteJSON(os.Stdout, res)
					}
					previous := res.Previous
					if previous == "" {
						previous = "-"
					}
					fmt.Fprintf(os.Stdout, "%s: %s -> %s\n", res.Setting, sanitize(previous), res.Value)
					return nil
				},
			}.run(flags)
		},
	}
}

func privacySettingHelp() string {
	var b strings.Builder
	for _, spec := range privacySettingSpecs {
		values := make([]string, len(spec.values))
		for i, v := range spec.values {
			values[i] = string(v)
		}
		fmt.Fprintf(&b, "  %-14s %s\n", spec.name, strings.Join(values, "|"))
	}
	return b.String()
}

// changePrivacySetting reads the current value first, so the result says
// what to set to undo the change.
func changePrivacySetting(ctx context.Context, a waStoreApp, req sendDelegateRequest) (privacySetResult, error) {
	spec, value, err := parsePrivacyChange(req.PrivacySetting, req.PrivacyValue)
	if err != nil {
		return privacySetResult{}, err
	}
	before, err := a.WA().GetPrivacySettings(ctx)
	if err != nil {
		return privacySetResult{}, fmt.Errorf("get privacy settings: %w", err)
	}
	if _, err := a.WA().SetPrivacySetting(ctx, spec.wire, value); err != nil {
		return privacySetResult{}, fmt.Errorf("set privacy setting %s: %w", spec.name, err)
	}
	return privacySetResult{Setting: spec.name, Value: string(value), Previous: string(spec.get(before))}, nil
}

type disappearingDefaultResult struct {
	Duration string `json:"duration"`
	Seconds  int64  `json:"seconds"`
}

// parseDisappearingDefault accepts the timers WhatsApp offers for new chats.
func parseDisappearingDefault(raw string) (disappearingDefaultResult, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "off":
		return disappearingDefaultResult{Duration: "off"}, nil
	case "24h", "1d":
		return disappearingDefaultResult{Duration: "24h", Seconds: int64((24 * time.Hour).Seconds())}, nil
	case "7d":
		return disappearingDefaultResult{Duration: "7d", Seconds: int64((7 * 24 * time.Hour).Seconds())}, nil
	case "90d":
		return disappearingDefaultResult{Duration: "90d", Seconds: int64((90 * 24 * time.Hour).Seconds())}, nil
	default:
		return disappearingDefaultResult{}, fmt.Errorf("invalid --duration %q; use 0 (off), 24h, 7d or 90d", raw)
	}
}

func newPrivacyDisappearingDefaultCmd(flags *rootFlags) *cobra.Command {
	var duration string
	cmd := &cobra.Command{
		Use:   "disappearing-default --duration 0|24h|7d|90d",
		Short: "Set the default disappearing-message timer for new chats",
		Long: "Set the disappearing-message timer that new one-to-one chats start with.\n" +
			"Existing chats keep their own timer. WhatsApp does not report the current\n" +
			"default to linked devices; check it on the phone before changing it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("duration") {
				return fmt.Errorf("--duration is required")
			}
			if _, err := parseDisappearingDefault(duration); err != nil {
				return err
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			return delegatedCommand[disappearingDefaultResult]{
				req:  sendDelegateRequest{Kind: privacyDisappearingDefaultKind, DisappearingTimer: duration},
				live: true,
				op:   setDisappearingDefault,
				write: func(res disappearingDefaultResult) error {
					if flags.asJSON {
						return out.WriteJSON(os.Stdout, res)
					}
					if res.Seconds == 0 {
						fmt.Fprintln(os.Stdout, "Default disappearing messages turned off.")
					} else {
						fmt.Fprintf(os.Stdout, "Default disappearing timer set to %s.\n", res.Duration)
					}
					return nil
				},
			}.run(flags)
		},
	}
	cmd.Flags().StringVar(&duration, "duration", "", "timer for new chats: 0 (off), 24h, 7d or 90d")
	return cmd
}

func setDisappearingDefault(ctx context.Context, a waStoreApp, req sendDelegateRequest) (disappearingDefaultResult, error) {
	timer, err := parseDisappearingDefault(req.DisappearingTimer)
	if err != nil {
		return disappearingDefaultResult{}, err
	}
	if err := a.WA().SetDefaultDisappearingTimer(ctx, time.Duration(timer.Seconds)*time.Second); err != nil {
		return disappearingDefaultResult{}, fmt.Errorf("set default disappearing timer: %w", err)
	}
	return timer, nil
}

type statusPrivacyOutput struct {
	Type    string   `json:"type"`
	Default bool     `json:"default"`
	List    []string `json:"list,omitempty"`
}

func newPrivacyStatusCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show who your status updates are shared with",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := flags.requireWritable(); err != nil {
				return err
			}
			return delegatedCommand[[]statusPrivacyOutput]{
				req:  sendDelegateRequest{Kind: privacyStatusKind},
				live: true,
				op:   fetchStatusPrivacy,
				write: func(res []statusPrivacyOutput) error {
					if flags.asJSON {
						return out.WriteJSON(os.Stdout, res)
					}
					w := newTableWriter(os.Stdout)
					fmt.Fprintln(w, "TYPE\tDEFAULT\tPEOPLE")
					for _, p := range res {
						fmt.Fprintf(w, "%s\t%t\t%d\n", sanitize(p.Type), p.Default, len(p.List))
					}
					return w.Flush()
				},
			}.run(flags)
		},
	}
}

func fetchStatusPrivacy(ctx context.Context, a waStoreApp, _ sendDelegateRequest) ([]statusPrivacyOutput, error) {
	lists, err := a.WA().GetStatusPrivacy(ctx)
	if err != nil {
		return nil, fmt.Errorf("get status privacy: %w", err)
	}
	result := make([]statusPrivacyOutput, 0, len(lists))
	for _, list := range lists {
		entry := statusPrivacyOutput{Type: string(list.Type), Default: list.IsDefault}
		for _, jid := range list.List {
			entry.List = append(entry.List, jid.String())
		}
		result = append(result, entry)
	}
	return result, nil
}
