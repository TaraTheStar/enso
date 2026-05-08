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

func TestRenameCmd(t *testing.T) {
	chat := tview.NewTextView()
	var gotInput string
	gotSlug := "stale"
	sc := &slashContext{
		chat: chat,
		setSessionLabel: func(label string) (string, error) {
			gotInput = label
			gotSlug = "refactor-auth-flow"
			return gotSlug, nil
		},
	}
	c := &renameCmd{sc: sc}

	// Override path: callback receives raw input; chat reflects the
	// stored slug.
	if err := c.Run(context.Background(), "Refactor: Auth Flow!"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if gotInput != "Refactor: Auth Flow!" {
		t.Errorf("callback got %q, want raw input", gotInput)
	}
	if !strings.Contains(chat.GetText(true), "refactor-auth-flow") {
		t.Errorf("chat missing slug: %q", chat.GetText(true))
	}

	// No-arg with no writer: reports "no label set yet".
	chat.Clear()
	if err := c.Run(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(chat.GetText(true), "no label set yet") {
		t.Errorf("expected 'no label set yet': %q", chat.GetText(true))
	}

	// Slugifies-to-empty input is reported, callback isn't told about it.
	chat.Clear()
	gotInput = ""
	sc.setSessionLabel = func(label string) (string, error) {
		gotInput = label
		return "", nil
	}
	if err := c.Run(context.Background(), "!!!"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(chat.GetText(true), "no usable characters") {
		t.Errorf("expected 'no usable characters': %q", chat.GetText(true))
	}
}
