package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/spf13/cobra"
)

func newGroupsPruneCmd(flags *rootFlags) *cobra.Command {
	var days int
	var leftOnly bool
	var includeActive bool
	var dryRun bool
	var confirm bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove old or left groups from local storage",
		Long: `Clean up groups that you have left or that have been inactive.

By default, removes groups you have left. Use --days to prune only left
groups older than the threshold. Add --include-active to also prune active
groups whose last local message is older than the threshold.

This only deletes local wacli store rows. It does not leave WhatsApp groups
or delete anything from WhatsApp servers. Use --dry-run to preview targets.

While "sync --follow" runs, --dry-run reads the store without its lock and
the deletion itself runs in the sync process, limited to the groups listed
for confirmation that are still prunable.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := flags.requireWritable(); err != nil {
				return err
			}
			if days < 0 {
				return fmt.Errorf("days must not be negative")
			}
			if !leftOnly {
				includeActive = true
			}

			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			if dryRun {
				groups, err := listPrunableGroupsUnlocked(ctx, flags, days, includeActive)
				if err != nil {
					return err
				}
				return writePruneDryRun(groups, flags.asJSON)
			}

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				return delegateGroupsPrune(ctx, flags, err, days, includeActive, confirm)
			}
			defer closeApp(a, lk)

			groups, err := a.DB().ListPrunableGroups(days, includeActive)
			if err != nil {
				return err
			}
			if len(groups) == 0 {
				return writeNothingToPrune(flags.asJSON)
			}
			if !confirm && !confirmGroupsPrune(os.Stdin, len(groups)) {
				return nil
			}
			deleted, err := deleteGroupsLocally(a.DB(), groups)
			return writeGroupsPruned(flags.asJSON, deleted, err)
		},
	}
	cmd.Flags().IntVar(&days, "days", 0, "prune groups older than N days (0 = all left groups)")
	cmd.Flags().BoolVar(&leftOnly, "left-only", true, "only remove groups you have left")
	cmd.Flags().BoolVar(&includeActive, "include-active", false, "also remove active groups with no messages in the last N days")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be deleted without deleting")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "skip confirmation prompt")
	return cmd
}

// listPrunableGroupsUnlocked reads the prune targets without the store lock,
// as `groups list` does, so it works while `sync --follow` holds the lock.
func listPrunableGroupsUnlocked(ctx context.Context, flags *rootFlags, days int, includeActive bool) ([]store.Group, error) {
	a, lk, err := newApp(ctx, flags, false, false)
	if err != nil {
		return nil, err
	}
	defer closeApp(a, lk)
	return a.DB().ListPrunableGroups(days, includeActive)
}

// delegateGroupsPrune runs a prune in the same-store `sync --follow` that
// holds the lock. The caller confirms the targets here, and the sync process
// deletes only those that are still prunable when it runs. Without a running
// sync process it returns lockErr.
func delegateGroupsPrune(ctx context.Context, flags *rootFlags, lockErr error, days int, includeActive, confirm bool) error {
	if !lock.IsLocked(lockErr) || !sendDelegateSocketPresent(flags) {
		return lockErr
	}
	groups, err := listPrunableGroupsUnlocked(ctx, flags, days, includeActive)
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		return writeNothingToPrune(flags.asJSON)
	}
	if !confirm && !confirmGroupsPrune(os.Stdin, len(groups)) {
		return nil
	}
	jids := make([]string, 0, len(groups))
	for _, g := range groups {
		jids = append(jids, g.JID)
	}
	req := sendDelegateRequest{Kind: groupsPruneKind, Groups: jids, PruneDays: days, IncludeActive: includeActive}
	return delegateAfterOpenFailure(ctx, flags, lockErr, req, func(resp sendDelegateResponse) error {
		var deleted []prunedGroup
		if err := decodeGroupResult(resp.Result, &deleted); err != nil {
			return err
		}
		return writeGroupsPruned(flags.asJSON, deleted, nil)
	})
}

// sendDelegateSocketPresent reports whether a sync process has opened the
// store's delegate socket, before asking the user to confirm work only it
// could do.
func sendDelegateSocketPresent(flags *rootFlags) bool {
	storeDir, err := resolveStoreDir(flags)
	if err != nil {
		return false
	}
	info, err := os.Lstat(sendDelegateSocketPath(storeDir))
	return err == nil && info.Mode()&os.ModeSocket != 0
}

// prunedGroup is a group whose local rows were deleted.
type prunedGroup struct {
	JID  string `json:"jid"`
	Name string `json:"name,omitempty"`
}

// executeGroupsPrune deletes the local rows of the groups the caller
// confirmed (req.Groups) that are still prunable under the same criteria.
func executeGroupsPrune(_ context.Context, a waStoreApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	if req.PruneDays < 0 {
		return sendDelegateResponse{}, fmt.Errorf("days must not be negative")
	}
	if len(req.Groups) == 0 {
		return sendDelegateResponse{}, fmt.Errorf("no confirmed groups to prune")
	}
	confirmed := make(map[string]bool, len(req.Groups))
	for _, jid := range req.Groups {
		confirmed[jid] = true
	}
	current, err := a.DB().ListPrunableGroups(req.PruneDays, req.IncludeActive)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	var targets []store.Group
	for _, g := range current {
		if confirmed[g.JID] {
			targets = append(targets, g)
		}
	}
	deleted, err := deleteGroupsLocally(a.DB(), targets)
	if err != nil {
		return sendDelegateResponse{}, fmt.Errorf("prune incomplete: deleted %d group(s): %w", len(deleted), err)
	}
	if deleted == nil {
		deleted = []prunedGroup{}
	}
	raw, err := encodeGroupResult(deleted)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Count: len(deleted), Result: raw}, nil
}

// deleteGroupsLocally deletes each group's local rows and returns the ones it
// deleted, with every failure joined into err.
func deleteGroupsLocally(db *store.DB, groups []store.Group) ([]prunedGroup, error) {
	var deleted []prunedGroup
	var failures []error
	for _, g := range groups {
		if err := db.DeleteGroupLocalData(g.JID); err != nil {
			failures = append(failures, fmt.Errorf("delete group %s: %w", g.JID, err))
			continue
		}
		deleted = append(deleted, prunedGroup{JID: g.JID, Name: g.Name})
	}
	return deleted, errors.Join(failures...)
}

func confirmGroupsPrune(in io.Reader, n int) bool {
	fmt.Fprintf(os.Stderr, "About to delete %d group(s) from the local wacli store. This cannot be undone.\n", n)
	fmt.Fprint(os.Stderr, "Continue? [y/N] ")
	answer, _ := bufio.NewReader(in).ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		fmt.Fprintln(os.Stderr, "Aborted.")
		return false
	}
	return true
}

func writeNothingToPrune(asJSON bool) error {
	if asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{"deleted": 0, "message": "no groups to prune"})
	}
	fmt.Fprintln(os.Stderr, "No groups to prune.")
	return nil
}

func writePruneDryRun(groups []store.Group, asJSON bool) error {
	if len(groups) == 0 {
		return writeNothingToPrune(asJSON)
	}
	if asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{"would_delete": len(groups), "groups": groups})
	}
	writePruneTargets(os.Stderr, "Would delete", groups)
	fmt.Fprintln(os.Stderr, "\nRun without --dry-run to actually delete.")
	return nil
}

// writeGroupsPruned reports the deleted groups. With deleteErr set, it lists
// what was deleted and returns the failure.
func writeGroupsPruned(asJSON bool, deleted []prunedGroup, deleteErr error) error {
	if !asJSON {
		for _, g := range deleted {
			name := g.Name
			if name == "" {
				name = g.JID
			}
			fmt.Fprintf(os.Stderr, "Deleted %s\n", sanitize(name))
		}
	}
	if deleteErr != nil {
		return fmt.Errorf("prune incomplete: deleted %d group(s): %w", len(deleted), deleteErr)
	}
	if asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{"deleted": len(deleted)})
	}
	fmt.Fprintf(os.Stderr, "\nDone. Deleted %d group(s).\n", len(deleted))
	return nil
}

func writePruneTargets(w *os.File, prefix string, groups []store.Group) {
	fmt.Fprintf(w, "%s %d group(s):\n", prefix, len(groups))
	for _, g := range groups {
		name := g.Name
		if name == "" {
			name = g.JID
		}
		state := "left"
		if g.LeftAt.IsZero() {
			state = "inactive"
		}
		fmt.Fprintf(w, "  - %s (%s, %s)\n", sanitize(name), sanitize(g.JID), state)
	}
}
