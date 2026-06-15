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

// printlnMsgText extracts the plain (ANSI-stripped) text from an already
// -produced printLineMessage. The body is read via reflection (read-only
// access works for unexported fields).
func printlnMsgText(t *testing.T, msg tea.Msg) string {
	t.Helper()
	if msg == nil {
		return ""
	}
	v := reflect.ValueOf(msg)
	f := v.FieldByName("messageBody")
	if !f.IsValid() {
		t.Fatalf("cmd did not produce a printLineMessage: %T", msg)
	}
	return ansi.Strip(f.String())
}

// TestResolvePerm_AttachModeDegrades: with no enforcement surface (both
// the host display checker and the worker session nil — true attach
// mode), "always"/"turn" honestly fall back to allow-once and say so.
func TestResolvePerm_AttachModeDegrades(t *testing.T) {
	for _, key := range []string{"a", "t"} {
		// Buffered Respond so the Cmd's decision send doesn't block.
		p := &permPending{
			req: &permissions.PromptRequest{
				ToolName: "bash",
				Args:     map[string]any{"cmd": "ls"},
				Respond:  make(chan permissions.Decision, 1),
			},
			// checker + sess both nil → no path to enforcement.
		}
		decided, cmd := resolvePerm(p, key)
		if !decided {
			t.Fatalf("key %q: decided=false, want true", key)
		}
		msg := cmd()
		if d := <-p.req.Respond; d != permissions.Allow {
			t.Fatalf("key %q: decision = %v, want Allow", key, d)
		}
		if got := printlnMsgText(t, msg); !strings.Contains(got, "unavailable in attach mode") {
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
		req: &permissions.PromptRequest{
			ToolName: "bash",
			Args:     map[string]any{"cmd": "ls -la"},
			Respond:  make(chan permissions.Decision, 1),
		},
		checker: checker,
	}
	if checker.HasTurnAllows() {
		t.Fatalf("precondition: no turn allows yet")
	}
	decided, cmd := resolvePerm(p, "t")
	if !decided {
		t.Fatalf("decided=false, want true")
	}
	// Enforcement + notice now run inside the Cmd (off the event loop),
	// so execute it before asserting the checker was mutated.
	msg := cmd()
	if d := <-p.req.Respond; d != permissions.Allow {
		t.Fatalf("decision = %v, want Allow", d)
	}
	if !checker.HasTurnAllows() {
		t.Fatalf("turn grant was not applied to the enforcing checker")
	}
	got := printlnMsgText(t, msg)
	if strings.Contains(got, "unavailable in attach mode") {
		t.Fatalf("notice still lies about attach mode: %q", got)
	}
	if !strings.Contains(got, "for this turn") {
		t.Fatalf("notice = %q, want a turn-grant confirmation", got)
	}
}
