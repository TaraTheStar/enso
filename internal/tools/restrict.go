// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveRestricted returns the absolute path that `p` resolves to,
// after joining with ac.Cwd if relative. If ac.RestrictedRoots is
// non-empty, the path's symlink-resolved target must lie under at least
// one of them or the call is rejected — this is the file-tool half of
// sandboxing, covering the read/write/edit/grep/glob tools that don't
// go through the container.
//
// Symlinks are evaluated for the confinement check: a symlink at
// /repo/secrets pointing to /etc/passwd is rejected even though
// /repo/secrets is lexically under /repo. Callers receive the
// user-supplied path back; the underlying os.Open will follow the
// symlink as usual once we've verified the target is in-bounds.
//
// Limitation: the check is non-atomic. A TOCTOU race where a regular
// file is replaced with a symlink between this check and the actual
// os.Open is not defended against here — that would need openat2 with
// RESOLVE_BENEATH on Linux. Realistic threat is the model creating
// symlinks via bash, not racing the agent goroutine.
func resolveRestricted(p string, ac *AgentContext) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(ac.Cwd, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("abs %s: %w", p, err)
	}
	resolved, err := resolveForCheck(abs)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", abs, err)
	}
	if !pathUnderRoots(resolved, ac.RestrictedRoots) {
		return "", fmt.Errorf("path %s resolves to %s which is outside the allowed roots %v (set permissions.disable_file_confinement = true to lift this restriction)", abs, resolved, ac.RestrictedRoots)
	}
	return abs, nil
}

// resolveForCheck returns the symlink-resolved form of abs. If abs (or
// some prefix of it) doesn't exist yet — e.g. a write/edit creating a
// new file — we walk up to the deepest existing ancestor, EvalSymlinks
// that, and re-append the not-yet-existing tail. The non-existent tail
// can't itself be a symlink, so this is sound.
func resolveForCheck(abs string) (string, error) {
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// Walk up to the first ancestor that exists.
	dir, tail := filepath.Split(abs)
	dir = filepath.Clean(dir)
	for dir != "" && dir != string(filepath.Separator) {
		r, err := filepath.EvalSymlinks(dir)
		if err == nil {
			return filepath.Join(r, tail), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent, name := filepath.Split(dir)
		parent = filepath.Clean(parent)
		tail = filepath.Join(name, tail)
		if parent == dir {
			break
		}
		dir = parent
	}
	// No existing ancestor up to the root: nothing can be a symlink, so
	// the lexical path is fine.
	return abs, nil
}

// pathUnderRoots reports whether `abs` resolves under one of the roots.
// Empty `roots` means "no restriction"; returns true.
func pathUnderRoots(abs string, roots []string) bool {
	if len(roots) == 0 {
		return true
	}
	abs = filepath.Clean(abs)
	for _, r := range roots {
		root, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			// Root that doesn't exist (or is itself broken) can't
			// contain anything; skip rather than match.
			continue
		}
		root = filepath.Clean(root)
		if abs == root || strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
