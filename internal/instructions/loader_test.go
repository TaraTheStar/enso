// SPDX-License-Identifier: AGPL-3.0-or-later

package instructions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildSystemPrompt_DefaultOnly(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp+"/no-such-home") // no ~/.enso/ENSO.md

	prompt, err := BuildSystemPrompt(tmp)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if prompt == "" {
		t.Errorf("expected default prompt content, got empty")
	}
	// No project ENSO.md / AGENTS.md headers when nothing is around.
	if strings.Contains(prompt, "# Project Instructions") {
		t.Errorf("unexpected project section in:\n%s", prompt)
	}
}

func TestBuildSystemPrompt_UserOverridesDefault(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	userENSO := filepath.Join(tmp, ".enso", "ENSO.md")
	if err := os.MkdirAll(filepath.Dir(userENSO), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userENSO, []byte("USER-LEVEL OVERRIDE\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cwd := filepath.Join(tmp, "proj-no-enso-md")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	prompt, err := BuildSystemPrompt(cwd)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(prompt, "USER-LEVEL OVERRIDE") {
		t.Errorf("user prompt didn't replace default:\n%s", prompt)
	}
}

func TestBuildSystemPrompt_ProjectENSOAppended(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp+"/empty-home")

	cwd := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "ENSO.md"), []byte("PROJECT ENSO\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("PROJECT AGENTS\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prompt, err := BuildSystemPrompt(cwd)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(prompt, "# Project Instructions (ENSO.md)") {
		t.Errorf("missing ENSO header in:\n%s", prompt)
	}
	if !strings.Contains(prompt, "PROJECT ENSO") {
		t.Errorf("missing ENSO body")
	}
	if !strings.Contains(prompt, "# Project Instructions (AGENTS.md)") {
		t.Errorf("missing AGENTS header")
	}
	if !strings.Contains(prompt, "PROJECT AGENTS") {
		t.Errorf("missing AGENTS body")
	}
	// Order: default/user prompt, then ENSO, then AGENTS.
	idxENSO := strings.Index(prompt, "PROJECT ENSO")
	idxAGENTS := strings.Index(prompt, "PROJECT AGENTS")
	if idxENSO >= idxAGENTS {
		t.Errorf("ENSO must come before AGENTS in output")
	}
}

func TestFindClosest_WalksUp(t *testing.T) {
	tmp := t.TempDir()
	deep := filepath.Join(tmp, "a", "b", "c", "d")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	// ENSO.md two levels up.
	if err := os.WriteFile(filepath.Join(tmp, "a", "b", "ENSO.md"), []byte("FROM-B"), 0o644); err != nil {
		t.Fatal(err)
	}
	// AGENTS.md three levels up.
	if err := os.WriteFile(filepath.Join(tmp, "a", "AGENTS.md"), []byte("FROM-A"), 0o644); err != nil {
		t.Fatal(err)
	}

	enso, agents := findClosest(deep)
	if !strings.Contains(enso, "FROM-B") {
		t.Errorf("ENSO walk up failed, got %q", enso)
	}
	if !strings.Contains(agents, "FROM-A") {
		t.Errorf("AGENTS walk up failed, got %q", agents)
	}
}

func TestFindClosest_TerminatesOnRelativeDot(t *testing.T) {
	// filepath.Dir(".") == ".", so the previous fixpoint check
	// (d == "/") hung forever. Verify the loop now terminates cleanly
	// for relative input. Test must complete well under the global
	// `go test` timeout — we time-bound it to be loud about regressions.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = findClosest(".")
	}()
	select {
	case <-done:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("findClosest(\".\") did not terminate")
	}
}

func TestFindClosest_PrefersClosest(t *testing.T) {
	tmp := t.TempDir()
	deep := filepath.Join(tmp, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	// Two ENSO.md files; the closest (in `b`) should win.
	if err := os.WriteFile(filepath.Join(tmp, "ENSO.md"), []byte("ROOT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "ENSO.md"), []byte("DEEPEST"), 0o644); err != nil {
		t.Fatal(err)
	}

	enso, _ := findClosest(deep)
	if !strings.Contains(enso, "DEEPEST") {
		t.Errorf("closest didn't win, got %q", enso)
	}
}

func TestBuildSystemPrompt_LoadsMemories(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	// User memory.
	userMem := filepath.Join(tmp, ".enso", "memory")
	if err := os.MkdirAll(userMem, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userMem, "user-pref.md"), []byte("user prefers terse output"), 0o644); err != nil {
		t.Fatal(err)
	}
	// User memory that the project overrides.
	if err := os.WriteFile(filepath.Join(userMem, "shared.md"), []byte("user version of shared"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Project memory.
	cwd := filepath.Join(tmp, "proj")
	projMem := filepath.Join(cwd, ".enso", "memory")
	if err := os.MkdirAll(projMem, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projMem, "db-policy.md"), []byte("integration tests must hit a real database"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projMem, "shared.md"), []byte("project version of shared"), 0o644); err != nil {
		t.Fatal(err)
	}

	prompt, err := BuildSystemPrompt(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "# Auto-memory") {
		t.Errorf("missing Auto-memory header in prompt")
	}
	for _, want := range []string{
		"user prefers terse output",
		"integration tests must hit a real database",
		"project version of shared",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing memory content %q", want)
		}
	}
	if strings.Contains(prompt, "user version of shared") {
		t.Errorf("project should shadow user for same-named memory file")
	}
}

func TestLoadMemories_NoDirsReturnsEmpty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "no-such-home"))
	if got := loadMemories(filepath.Join(tmp, "proj")); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}
