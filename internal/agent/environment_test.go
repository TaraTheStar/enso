// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestEnvironmentNote_EmptyCwdReturnsEmpty(t *testing.T) {
	if got := environmentNote("", time.Now()); got != "" {
		t.Errorf("empty cwd should return empty note, got %q", got)
	}
}

func TestEnvironmentNote_BasicShape(t *testing.T) {
	tmp := t.TempDir()
	now := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	got := environmentNote(tmp, now)

	for _, want := range []string{
		"# Environment",
		"Working directory: " + tmp,
		"Platform: " + runtime.GOOS + "/" + runtime.GOARCH,
		"Today's date: 2026-05-05",
		"Git repo: no", // tempdir is not a git repo
	} {
		if !strings.Contains(got, want) {
			t.Errorf("env note missing %q\nfull:\n%s", want, got)
		}
	}
}

func TestEnvironmentNote_DetectsGitRepo(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := environmentNote(tmp, time.Now())
	if !strings.Contains(got, "Git repo: yes") {
		t.Errorf("expected 'Git repo: yes' for dir with .git, got:\n%s", got)
	}
}

func TestEnvironmentNote_DetectsGitRepoFromSubdir(t *testing.T) {
	// Mimic running from a nested directory inside a repo.
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(tmp, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got := environmentNote(sub, time.Now())
	if !strings.Contains(got, "Git repo: yes") {
		t.Errorf("walk-up should find .git from subdir, got:\n%s", got)
	}
}

func TestEnvironmentNote_GitFileSubmodule(t *testing.T) {
	// In a submodule, .git is a regular file, not a directory. Make sure
	// we still detect that as a repo.
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, ".git"), []byte("gitdir: ../.git/modules/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := environmentNote(tmp, time.Now())
	if !strings.Contains(got, "Git repo: yes") {
		t.Errorf("submodule (.git as file) should count as repo, got:\n%s", got)
	}
}
