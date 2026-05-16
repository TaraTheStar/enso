// SPDX-License-Identifier: AGPL-3.0-or-later

package instructions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setUserHome points HOME at dir and clears the XDG_* vars so user-scoped
// config resolves under dir (see internal/paths). CI runners set
// XDG_CONFIG_HOME, which otherwise takes precedence over HOME and makes
// these fixtures invisible to the loaders.
func setUserHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
}

func TestBuildSystemPrompt_DefaultOnly(t *testing.T) {
	tmp := t.TempDir()
	setUserHome(t, tmp+"/no-such-home") // no ~/.config/enso/ENSO.md

	prompt, err := BuildSystemPrompt(tmp, nil)
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

// User ENSO.md without frontmatter should APPEND to the default
// (append-by-default — see BuildSystemPrompt). Both the default content
// AND the user content must be present.
func TestBuildSystemPrompt_UserAppendsToDefault(t *testing.T) {
	tmp := t.TempDir()
	setUserHome(t, tmp)

	userENSO := filepath.Join(tmp, ".config", "enso", "ENSO.md")
	if err := os.MkdirAll(filepath.Dir(userENSO), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userENSO, []byte("USER-LEVEL ADDENDUM\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cwd := filepath.Join(tmp, "proj-no-enso-md")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	prompt, err := BuildSystemPrompt(cwd, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(prompt, "USER-LEVEL ADDENDUM") {
		t.Errorf("user prompt missing from output:\n%s", prompt)
	}
	// Default must still be present — append, not replace.
	if !strings.Contains(prompt, "ensō") && !strings.Contains(prompt, "enso") {
		t.Errorf("default prompt seems to have been discarded — append should preserve it:\n%s", prompt)
	}
}

// User ENSO.md with `--- replace: true ---` discards the default.
func TestBuildSystemPrompt_UserReplaceDiscardsDefault(t *testing.T) {
	tmp := t.TempDir()
	setUserHome(t, tmp)

	userENSO := filepath.Join(tmp, ".config", "enso", "ENSO.md")
	if err := os.MkdirAll(filepath.Dir(userENSO), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nreplace: true\n---\nUSER-LEVEL OVERRIDE\n"
	if err := os.WriteFile(userENSO, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cwd := filepath.Join(tmp, "proj-no-enso-md")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	prompt, err := BuildSystemPrompt(cwd, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(prompt, "USER-LEVEL OVERRIDE") {
		t.Errorf("user override missing:\n%s", prompt)
	}
	// Default content must be GONE.
	if strings.Contains(prompt, "ensō") || strings.Contains(prompt, "Default") {
		t.Errorf("replace: true should have discarded default — still present in:\n%s", prompt)
	}
	// Frontmatter itself must NOT leak into the prompt.
	if strings.Contains(prompt, "replace:") || strings.Contains(prompt, "---") {
		t.Errorf("frontmatter leaked into prompt body:\n%s", prompt)
	}
}

func TestBuildSystemPrompt_ProjectENSOAppended(t *testing.T) {
	tmp := t.TempDir()
	setUserHome(t, tmp+"/empty-home")

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

	prompt, err := BuildSystemPrompt(cwd, nil)
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
	// Order: default, then ENSO, then AGENTS.
	idxENSO := strings.Index(prompt, "PROJECT ENSO")
	idxAGENTS := strings.Index(prompt, "PROJECT AGENTS")
	if idxENSO >= idxAGENTS {
		t.Errorf("ENSO must come before AGENTS in output")
	}
}

// Project-level ENSO.md with replace:true discards default AND user-level
// content. This is the team-shared-exact-context use case.
func TestBuildSystemPrompt_ProjectReplaceDiscardsAll(t *testing.T) {
	tmp := t.TempDir()
	setUserHome(t, tmp)

	// Seed user ENSO.md so we can verify it gets discarded.
	userENSO := filepath.Join(tmp, ".config", "enso", "ENSO.md")
	if err := os.MkdirAll(filepath.Dir(userENSO), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userENSO, []byte("USER STUFF\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cwd := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nreplace: true\n---\nTEAM CANONICAL PROMPT\n"
	if err := os.WriteFile(filepath.Join(cwd, "ENSO.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	prompt, err := BuildSystemPrompt(cwd, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(prompt, "TEAM CANONICAL PROMPT") {
		t.Errorf("project replace body missing:\n%s", prompt)
	}
	if strings.Contains(prompt, "USER STUFF") {
		t.Errorf("project replace should discard user layer; still present:\n%s", prompt)
	}
}

// AGENTS.md with replace:true zeroes the stack just like ENSO.md does —
// the rule is uniform across every prompt-content file.
func TestBuildSystemPrompt_AgentsReplaceDiscardsAll(t *testing.T) {
	tmp := t.TempDir()
	setUserHome(t, tmp+"/no-home")

	cwd := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "ENSO.md"), []byte("PROJECT ENSO BODY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "---\nreplace: true\n---\nAGENTS-ONLY PROMPT\n"
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	prompt, err := BuildSystemPrompt(cwd, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(prompt, "AGENTS-ONLY PROMPT") {
		t.Errorf("agents replace body missing:\n%s", prompt)
	}
	if strings.Contains(prompt, "PROJECT ENSO BODY") {
		t.Errorf("agents replace should discard ENSO layer; still present:\n%s", prompt)
	}
}

func TestFindClosestPaths_WalksUp(t *testing.T) {
	tmp := t.TempDir()
	deep := filepath.Join(tmp, "a", "b", "c", "d")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "a", "b", "ENSO.md"), []byte("FROM-B"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "a", "AGENTS.md"), []byte("FROM-A"), 0o644); err != nil {
		t.Fatal(err)
	}

	ensoPath, agentsPath := findClosestPaths(deep)
	if !strings.HasSuffix(ensoPath, filepath.Join("a", "b", "ENSO.md")) {
		t.Errorf("ENSO walk up failed, got %q", ensoPath)
	}
	if !strings.HasSuffix(agentsPath, filepath.Join("a", "AGENTS.md")) {
		t.Errorf("AGENTS walk up failed, got %q", agentsPath)
	}
}

func TestFindClosestPaths_TerminatesOnRelativeDot(t *testing.T) {
	// filepath.Dir(".") == ".", so an earlier fixpoint check
	// (d == "/") hung forever. Verify the loop now terminates cleanly
	// for relative input.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = findClosestPaths(".")
	}()
	select {
	case <-done:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("findClosestPaths(\".\") did not terminate")
	}
}

func TestFindClosestPaths_PrefersClosest(t *testing.T) {
	tmp := t.TempDir()
	deep := filepath.Join(tmp, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "ENSO.md"), []byte("ROOT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "ENSO.md"), []byte("DEEPEST"), 0o644); err != nil {
		t.Fatal(err)
	}

	ensoPath, _ := findClosestPaths(deep)
	if !strings.HasSuffix(ensoPath, filepath.Join("a", "b", "ENSO.md")) {
		t.Errorf("closest didn't win, got %q", ensoPath)
	}
}

func TestBuildSystemPrompt_LoadsMemories(t *testing.T) {
	tmp := t.TempDir()
	setUserHome(t, tmp)

	// User memory (XDG data dir fallback under $HOME/.local/share/enso).
	userMem := filepath.Join(tmp, ".local", "share", "enso", "memory")
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

	prompt, err := BuildSystemPrompt(cwd, nil)
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
	setUserHome(t, filepath.Join(tmp, "no-such-home"))
	if got := loadMemories(filepath.Join(tmp, "proj")); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

// BuildSystemPromptLayered exposes the addressable structure /prompt and
// future tooling will need. Verify it returns one Layer per active source.
func TestBuildSystemPromptLayered_ReturnsLayers(t *testing.T) {
	tmp := t.TempDir()
	setUserHome(t, tmp)

	userENSO := filepath.Join(tmp, ".config", "enso", "ENSO.md")
	if err := os.MkdirAll(filepath.Dir(userENSO), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userENSO, []byte("USER ADDENDUM\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cwd := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "ENSO.md"), []byte("PROJECT ENSO BODY\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	layers, err := BuildSystemPromptLayered(cwd, nil)
	if err != nil {
		t.Fatalf("layered: %v", err)
	}
	// Expect: default, user, project ENSO. (No AGENTS.md, no memories.)
	if len(layers) != 3 {
		t.Fatalf("expected 3 layers, got %d: %+v", len(layers), names(layers))
	}
	if layers[0].Name != "default" {
		t.Errorf("layer 0 = %q, want %q", layers[0].Name, "default")
	}
	if !strings.Contains(layers[1].Body, "USER ADDENDUM") {
		t.Errorf("layer 1 missing user content")
	}
	if !strings.Contains(layers[2].Body, "PROJECT ENSO BODY") {
		t.Errorf("layer 2 missing project content")
	}
	for _, l := range layers {
		if l.Replace {
			t.Errorf("no layer should report replace=true here: %q", l.Name)
		}
	}
}

// When a layer carries replace:true, BuildSystemPromptLayered keeps every
// considered layer in the slice but marks the earlier ones Discarded so
// /prompt can show the user what got dropped and by whom.
func TestBuildSystemPromptLayered_ReplaceMarksDiscarded(t *testing.T) {
	tmp := t.TempDir()
	setUserHome(t, tmp)

	userENSO := filepath.Join(tmp, ".config", "enso", "ENSO.md")
	if err := os.MkdirAll(filepath.Dir(userENSO), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nreplace: true\n---\nUSER FULL CUSTOM\n"
	if err := os.WriteFile(userENSO, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cwd := filepath.Join(tmp, "proj-no-enso")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	layers, err := BuildSystemPromptLayered(cwd, nil)
	if err != nil {
		t.Fatalf("layered: %v", err)
	}
	if len(layers) != 2 {
		t.Fatalf("expected 2 layers (default discarded + replace), got %d: %+v", len(layers), names(layers))
	}
	if layers[0].Name != "default" || !layers[0].Discarded {
		t.Errorf("layer 0 should be default with Discarded=true; got name=%q discarded=%v", layers[0].Name, layers[0].Discarded)
	}
	if !layers[1].Replace || layers[1].Discarded {
		t.Errorf("layer 1 should be the replace layer (Replace=true, Discarded=false); got %+v", layers[1])
	}
	if !strings.Contains(layers[1].Body, "USER FULL CUSTOM") {
		t.Errorf("layer 1 missing user body")
	}
}

func names(layers []Layer) []string {
	out := make([]string, len(layers))
	for i, l := range layers {
		out[i] = l.Name
	}
	return out
}
