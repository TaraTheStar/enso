// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// setupWorktree creates a fresh git worktree and chdirs into it. Called
// from PersistentPreRunE when --worktree is set, so every subsequent
// step (config load, session creation, agent setup) sees the new cwd.
//
// The worktree path is `~/.enso/worktrees/<repo-basename>-<rand>` and
// the branch name is `enso/<rand>` rooted at the current HEAD. Errors
// out if the cwd isn't inside a git repo.
//
// Cleanup is intentionally NOT automated. The user runs
// `git worktree remove <path>` (or `git worktree prune`) when done. This
// matches how worktrees usually work and avoids surprising the user
// when an in-progress branch silently disappears.
func setupWorktree() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("worktree: get cwd: %w", err)
	}
	repoRoot, err := findGitRoot(cwd)
	if err != nil {
		return fmt.Errorf("worktree: %w (--worktree requires a git repo)", err)
	}

	short := randSlug()
	branch := "enso/" + short
	worktreePath, err := worktreeRootDir(repoRoot, short)
	if err != nil {
		return err
	}

	out, err := runIn(repoRoot, "git", "worktree", "add", "-b", branch, worktreePath)
	if err != nil {
		return fmt.Errorf("worktree: git worktree add failed: %w\n%s", err, out)
	}

	if err := os.Chdir(worktreePath); err != nil {
		return fmt.Errorf("worktree: chdir %s: %w", worktreePath, err)
	}
	fmt.Fprintf(os.Stderr, "[worktree] %s (branch %s)\n", worktreePath, branch)
	return nil
}

// findGitRoot walks up from `start` looking for a `.git` entry. It
// accepts either a directory or a file (which `git worktree`'s linked
// repos use to point back at the main repo).
func findGitRoot(start string) (string, error) {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("not in a git repo (no .git found from %s upward)", start)
		}
		dir = parent
	}
}

// worktreeRootDir picks `~/.enso/worktrees/<repo>-<short>` and ensures
// the parent directory exists. The path itself must NOT exist when
// `git worktree add` is called.
func worktreeRootDir(repoRoot, short string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("worktree: home: %w", err)
	}
	parent := filepath.Join(home, ".enso", "worktrees")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("worktree: mkdir %s: %w", parent, err)
	}
	return filepath.Join(parent, filepath.Base(repoRoot)+"-"+short), nil
}

// runIn runs cmd in dir, returning combined output for diagnostics.
func runIn(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// randSlug returns 6 hex chars — 24 bits of entropy, enough to
// distinguish concurrent worktrees per repo without making the path
// noisy. Falls back to "rand" on an unlikely entropy failure.
func randSlug() string {
	var buf [3]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "rand"
	}
	return hex.EncodeToString(buf[:])
}
