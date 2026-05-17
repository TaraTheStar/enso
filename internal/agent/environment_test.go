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
	if got := environmentNote("", time.Now(), "", nil); got != "" {
		t.Errorf("empty cwd should return empty note, got %q", got)
	}
}

func TestEnvironmentNote_BasicShape(t *testing.T) {
	tmp := t.TempDir()
	now := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	got := environmentNote(tmp, now, "", []string{tmp})

	for _, want := range []string{
		"# Environment",
		"Working directory: " + tmp,
		"Platform: " + runtime.GOOS + "/" + runtime.GOARCH,
		"Today's date: 2026-05-05",
		"Git repo: no", // tempdir is not a git repo
		"File-tool access: confined to " + tmp,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("env note missing %q\nfull:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Sandbox:") {
		t.Errorf("non-sandboxed note should not mention Sandbox, got:\n%s", got)
	}
}

func TestEnvironmentNote_DetectsGitRepo(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := environmentNote(tmp, time.Now(), "", []string{tmp})
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
	got := environmentNote(sub, time.Now(), "", []string{sub})
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
	got := environmentNote(tmp, time.Now(), "", []string{tmp})
	if !strings.Contains(got, "Git repo: yes") {
		t.Errorf("submodule (.git as file) should count as repo, got:\n%s", got)
	}
}

func TestEnvironmentNote_IsolationLineHonest(t *testing.T) {
	tmp := t.TempDir()

	// Empty isolation → the conservative truth, and the cwd is the
	// REAL host path (one namespace; no /work translation anywhere).
	got := environmentNote(tmp, time.Now(), "", []string{tmp})
	if !strings.Contains(got, "Working directory: "+tmp) {
		t.Errorf("cwd should be the real path, got:\n%s", got)
	}
	if !strings.Contains(got, "Isolation: none") || !strings.Contains(got, "no automatic rollback") {
		t.Errorf("empty isolation should state the no-isolation truth, got:\n%s", got)
	}
	// The deleted split-brain caveat must not resurface.
	for _, gone := range []string{"/work", "Sandbox bind-mount", "never the host path"} {
		if strings.Contains(got, gone) {
			t.Errorf("path-translation caveat %q must be gone, got:\n%s", gone, got)
		}
	}

	// A supplied note is surfaced verbatim on the Isolation line.
	note := "container (image alpine:latest), network sealed."
	got = environmentNote(tmp, time.Now(), note, []string{tmp})
	if !strings.Contains(got, "Isolation: "+note) {
		t.Errorf("supplied isolation note not surfaced, got:\n%s", got)
	}
}

func TestEnvironmentNote_UnconfinedFileToolsBanner(t *testing.T) {
	tmp := t.TempDir()
	got := environmentNote(tmp, time.Now(), "", nil)
	want := "File-tool access: unrestricted"
	if !strings.Contains(got, want) {
		t.Errorf("unconfined env note missing %q\nfull:\n%s", want, got)
	}
	if strings.Contains(got, "confined to") {
		t.Errorf("unconfined env note should not mention confinement, got:\n%s", got)
	}
}

func TestEnvironmentNote_MultipleRestrictedRootsListed(t *testing.T) {
	tmp := t.TempDir()
	extra := t.TempDir()
	got := environmentNote(tmp, time.Now(), "", []string{tmp, extra})
	if !strings.Contains(got, "confined to "+tmp+", "+extra) {
		t.Errorf("expected both roots in confinement list, got:\n%s", got)
	}
}
