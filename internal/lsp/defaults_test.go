// SPDX-License-Identifier: AGPL-3.0-or-later

package lsp

import (
	"errors"
	"os/exec"
	"sort"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/config"
)

// stubLookPath replaces lookPath for the duration of the test with a
// resolver that pretends `present` are on PATH and nothing else is.
// Restored on test exit so other tests see the real exec.LookPath.
func stubLookPath(t *testing.T, present ...string) {
	t.Helper()
	saved := lookPath
	set := map[string]bool{}
	for _, p := range present {
		set[p] = true
	}
	lookPath = func(name string) (string, error) {
		if set[name] {
			return "/fake/path/" + name, nil
		}
		return "", &exec.Error{Name: name, Err: errors.New("not found")}
	}
	t.Cleanup(func() { lookPath = saved })
}

// keys returns the sorted slice of map keys for stable test asserts.
func keys(m map[string]config.LSPConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestMergeBuiltinLSPs_ActivatesOnlyPresentBinaries verifies that an
// empty user config picks up just the builtins whose command is on
// PATH — nothing more, nothing less.
func TestMergeBuiltinLSPs_ActivatesOnlyPresentBinaries(t *testing.T) {
	stubLookPath(t, "gopls", "rust-analyzer")
	got := mergeBuiltinLSPs(nil, false)
	want := []string{"go", "rust"}
	if !equalSlices(keys(got), want) {
		t.Errorf("got %v, want %v", keys(got), want)
	}
}

// TestMergeBuiltinLSPs_UserOverrideWins exercises the precedence rule:
// a user-supplied [lsp.go] block keeps its command/args even when the
// builtin would resolve to a different binary.
func TestMergeBuiltinLSPs_UserOverrideWins(t *testing.T) {
	stubLookPath(t, "gopls")
	user := map[string]config.LSPConfig{
		"go": {Command: "/opt/custom/gopls", Args: []string{"-debug"}, Extensions: []string{".go"}},
	}
	got := mergeBuiltinLSPs(user, false)
	if got["go"].Command != "/opt/custom/gopls" {
		t.Errorf("got command %q, want user override", got["go"].Command)
	}
	if len(got["go"].Args) != 1 || got["go"].Args[0] != "-debug" {
		t.Errorf("got args %v, want user override", got["go"].Args)
	}
}

// TestMergeBuiltinLSPs_UserEmptyCommandDisables confirms the per-server
// disable form: [lsp.go] command = "" drops the slot AND blocks the
// builtin from filling it back in.
func TestMergeBuiltinLSPs_UserEmptyCommandDisables(t *testing.T) {
	stubLookPath(t, "gopls", "rust-analyzer")
	user := map[string]config.LSPConfig{
		"go": {Command: ""}, // explicit disable
	}
	got := mergeBuiltinLSPs(user, false)
	if _, present := got["go"]; present {
		t.Errorf("go entry survived user disable: %+v", got)
	}
	if _, present := got["rust"]; !present {
		t.Errorf("rust should still activate (separate builtin): %+v", got)
	}
}

// TestMergeBuiltinLSPs_GlobalDisable suppresses all builtins regardless
// of PATH; user-declared servers still pass through.
func TestMergeBuiltinLSPs_GlobalDisable(t *testing.T) {
	stubLookPath(t, "gopls", "rust-analyzer", "pyright-langserver")
	user := map[string]config.LSPConfig{
		"custom": {Command: "my-server", Extensions: []string{".q"}},
	}
	got := mergeBuiltinLSPs(user, true)
	if !equalSlices(keys(got), []string{"custom"}) {
		t.Errorf("global disable still merged builtins: %v", keys(got))
	}
}

// TestMergeBuiltinLSPs_NoBinariesNoBuiltins covers the boring case:
// fresh machine, no LSP binaries, nothing on PATH — output is the user
// map unchanged (just an allocation copy).
func TestMergeBuiltinLSPs_NoBinariesNoBuiltins(t *testing.T) {
	stubLookPath(t /* nothing present */)
	got := mergeBuiltinLSPs(nil, false)
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", keys(got))
	}
}

// TestBuiltinLSPs_TableSanity is a tiny structural sanity check on the
// builtin table — every entry must declare extensions (otherwise it
// would silently match nothing) and a non-empty command.
func TestBuiltinLSPs_TableSanity(t *testing.T) {
	for name, cfg := range builtinLSPs {
		if cfg.Command == "" {
			t.Errorf("builtin %q has empty Command", name)
		}
		if len(cfg.Extensions) == 0 {
			t.Errorf("builtin %q declares no Extensions (would match nothing)", name)
		}
		for _, ext := range cfg.Extensions {
			if !strings.HasPrefix(ext, ".") {
				t.Errorf("builtin %q extension %q missing leading dot", name, ext)
			}
		}
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
