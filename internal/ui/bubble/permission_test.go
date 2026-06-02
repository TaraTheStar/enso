// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/TaraTheStar/enso/internal/permissions"
)

// printlnText extracts the plain (ANSI-stripped) text from a tea.Println
// Cmd. tea.Println returns an unexported printLineMessage whose body is
// read via reflection (read-only access works for unexported fields).
func printlnText(t *testing.T, cmd tea.Cmd) string {
	t.Helper()
	if cmd == nil {
		return ""
	}
	v := reflect.ValueOf(cmd())
	f := v.FieldByName("messageBody")
	if !f.IsValid() {
		t.Fatalf("cmd did not produce a printLineMessage: %T", cmd())
	}
	return ansi.Strip(f.String())
}

// TestResolvePerm_AttachModeDegrades: with no enforcement surface (both
// the host display checker and the worker session nil — true attach
// mode), "always"/"turn" honestly fall back to allow-once and say so.
func TestResolvePerm_AttachModeDegrades(t *testing.T) {
	for _, key := range []string{"a", "t"} {
		p := &permPending{
			req: &permissions.PromptRequest{ToolName: "bash", Args: map[string]any{"cmd": "ls"}},
			// checker + sess both nil → no path to enforcement.
		}
		decision, decided, cmd := resolvePerm(p, key)
		if !decided || decision != permissions.Allow {
			t.Fatalf("key %q: got (%v, decided=%v), want (Allow, true)", key, decision, decided)
		}
		if got := printlnText(t, cmd); !strings.Contains(got, "unavailable in attach mode") {
			t.Fatalf("key %q: notice = %q, want the attach-mode fallback", key, got)
		}
	}
}

// TestResolvePerm_TurnGrantApplied is the U1 regression for the
// in-process enforcement surface: when a checker is present (sess nil),
// "turn" actually mutates it and does NOT show the misleading
// "unavailable in attach mode" notice. (The default local path applies
// the grant worker-side over the seam — covered in the worker package.)
func TestResolvePerm_TurnGrantApplied(t *testing.T) {
	checker := permissions.NewChecker(nil, nil, nil, "deny")
	p := &permPending{
		req:     &permissions.PromptRequest{ToolName: "bash", Args: map[string]any{"cmd": "ls -la"}},
		checker: checker,
	}
	if checker.HasTurnAllows() {
		t.Fatalf("precondition: no turn allows yet")
	}
	decision, decided, cmd := resolvePerm(p, "t")
	if !decided || decision != permissions.Allow {
		t.Fatalf("got (%v, decided=%v), want (Allow, true)", decision, decided)
	}
	if !checker.HasTurnAllows() {
		t.Fatalf("turn grant was not applied to the enforcing checker")
	}
	got := printlnText(t, cmd)
	if strings.Contains(got, "unavailable in attach mode") {
		t.Fatalf("notice still lies about attach mode: %q", got)
	}
	if !strings.Contains(got, "for this turn") {
		t.Fatalf("notice = %q, want a turn-grant confirmation", got)
	}
}
