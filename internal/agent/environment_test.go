// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestEnvironmentNote_EmptyCwdReturnsEmpty(t *testing.T) {
	if got := environmentNote("", time.Now(), nil, nil); got != "" {
		t.Errorf("empty cwd should return empty note, got %q", got)
	}
}

func TestEnvironmentNote_BasicShape(t *testing.T) {
	tmp := t.TempDir()
	now := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	got := environmentNote(tmp, now, nil, []string{tmp})

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
	got := environmentNote(tmp, time.Now(), nil, []string{tmp})
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
	got := environmentNote(sub, time.Now(), nil, []string{sub})
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
	got := environmentNote(tmp, time.Now(), nil, []string{tmp})
	if !strings.Contains(got, "Git repo: yes") {
		t.Errorf("submodule (.git as file) should count as repo, got:\n%s", got)
	}
}

// fakeSandbox satisfies tools.SandboxRunner for testing the sandboxed
// branch of environmentNote without spinning up a real container.
type fakeSandbox struct {
	runtime, image, container, mount string
}

func (f fakeSandbox) Exec(context.Context, io.Writer, string) error { return nil }
func (f fakeSandbox) ContainerName() string                         { return f.container }
func (f fakeSandbox) Runtime() string                               { return f.runtime }
func (f fakeSandbox) Image() string                                 { return f.image }
func (f fakeSandbox) WorkdirMount() string                          { return f.mount }

func TestEnvironmentNote_SandboxedReportsContainerCwd(t *testing.T) {
	tmp := t.TempDir()
	sb := fakeSandbox{
		runtime:   "docker",
		image:     "alpine:latest",
		container: "enso-test-abc123",
		mount:     "/work",
	}
	got := environmentNote(tmp, time.Now(), sb, []string{tmp})

	for _, want := range []string{
		"Working directory: /work (sandboxed; host path " + tmp + " is bind-mounted here)",
		"Sandbox: enabled — runtime docker, image alpine:latest, container enso-test-abc123",
		"Bash tool runs inside the sandbox",
		"file-touching tools (read/write/edit/grep/glob) run on the host process",
		"File-tool access: confined to " + tmp,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sandboxed env note missing %q\nfull:\n%s", want, got)
		}
	}
	// The host cwd should NOT appear as the primary "Working directory:"
	// line — only inside the parenthetical. Guard against regressions
	// where both lines are emitted and the model sees a contradiction.
	if strings.Contains(got, "Working directory: "+tmp+"\n") {
		t.Errorf("sandboxed note should not list the host cwd as the primary working directory, got:\n%s", got)
	}
}

func TestEnvironmentNote_UnconfinedFileToolsBanner(t *testing.T) {
	tmp := t.TempDir()
	got := environmentNote(tmp, time.Now(), nil, nil)
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
	got := environmentNote(tmp, time.Now(), nil, []string{tmp, extra})
	if !strings.Contains(got, "confined to "+tmp+", "+extra) {
		t.Errorf("expected both roots in confinement list, got:\n%s", got)
	}
}
