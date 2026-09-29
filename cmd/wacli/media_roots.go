package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// mediaRootsEnv confines the local files that send commands may upload. It is
// a guardrail for agents driving the CLI: a prompt-injected request cannot
// attach an arbitrary file from the machine. Unset keeps upstream behavior.
const mediaRootsEnv = "WACLI_MEDIA_ROOTS"

// checkOutboundMediaPath rejects path unless it resolves, symlinks included,
// to a file inside one of the WACLI_MEDIA_ROOTS directories.
func checkOutboundMediaPath(path string) error {
	raw := strings.TrimSpace(os.Getenv(mediaRootsEnv))
	if raw == "" {
		return nil
	}
	var roots []string
	for _, root := range filepath.SplitList(raw) {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if !filepath.IsAbs(root) {
			return fmt.Errorf("%s entries must be absolute paths, got %q", mediaRootsEnv, root)
		}
		roots = append(roots, root)
	}
	if len(roots) == 0 {
		return nil
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", path, err)
	}
	for _, root := range roots {
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue // a missing root cannot contain anything
		}
		if pathWithin(realRoot, real) {
			return nil
		}
	}
	return fmt.Errorf("%s is outside %s; copy it into one of: %s", path, mediaRootsEnv, strings.Join(roots, ", "))
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
