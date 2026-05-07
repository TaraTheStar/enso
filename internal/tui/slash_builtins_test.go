// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/rivo/tview"
)

// TestInitCmdSubmits verifies /init builds a non-empty prompt, applies a
// read+write tool restriction, and routes through the slash context's submit
// hook.
func TestInitCmdSubmits(t *testing.T) {
	var gotText string
	var gotTools []string
	sc := &slashContext{
		chat: tview.NewTextView(),
		submit: func(text string, allowed []string) {
			gotText = text
			gotTools = allowed
		},
	}
	c := &initCmd{sc: sc}

	if err := c.Run(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotText, "ENSO.md") {
		t.Errorf("default prompt should target ENSO.md, got:\n%s", gotText)
	}
	for _, want := range []string{"read", "grep", "glob", "write"} {
		if !contains(gotTools, want) {
			t.Errorf("expected tool %q in restriction, got %v", want, gotTools)
		}
	}

	gotText, gotTools = "", nil
	if err := c.Run(context.Background(), "AGENTS.md"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotText, "AGENTS.md") {
		t.Errorf("explicit target should appear in prompt, got:\n%s", gotText)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
