// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rivo/tview"
)

// writeWorkflow drops a workflow file at <cwd>/.enso/workflows/<name>.md
// so LoadByName can find it. Returns the cwd that should be passed to
// the slash context.
func writeWorkflow(t *testing.T, name, body string) string {
	t.Helper()
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".enso", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return cwd
}

func TestWorkflowValidate_GoodWorkflow(t *testing.T) {
	cwd := writeWorkflow(t, "good", `---
roles:
  planner: {}
  coder: {}
edges:
  - planner -> coder
---

## planner

Plan the work.

## coder

Write the code.
`)
	chat := tview.NewTextView()
	sc := &slashContext{chat: chat, cwd: cwd}
	c := &workflowCmd{sc: sc}

	if err := c.Run(context.Background(), "validate good"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := chat.GetText(true)
	if !strings.Contains(out, "ok") {
		t.Errorf("expected 'ok' on valid workflow: %q", out)
	}
	if !strings.Contains(out, "planner") || !strings.Contains(out, "coder") {
		t.Errorf("expected role list in output: %q", out)
	}
}

func TestWorkflowValidate_MissingRoleSection(t *testing.T) {
	// `coder` is declared in roles but has no `## coder` body section.
	cwd := writeWorkflow(t, "broken", `---
roles:
  planner: {}
  coder: {}
---

## planner

Plan only — no coder section.
`)
	chat := tview.NewTextView()
	sc := &slashContext{chat: chat, cwd: cwd}
	c := &workflowCmd{sc: sc}

	if err := c.Run(context.Background(), "validate broken"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := chat.GetText(true)
	if !strings.Contains(out, "validate") {
		t.Errorf("expected 'validate' in error label: %q", out)
	}
	if !strings.Contains(out, "coder") {
		t.Errorf("error should mention the missing role 'coder': %q", out)
	}
	if strings.Contains(out, " ok ") {
		t.Errorf("validate must not report ok on broken workflow: %q", out)
	}
}

func TestWorkflowValidate_NoArgUsage(t *testing.T) {
	chat := tview.NewTextView()
	sc := &slashContext{chat: chat, cwd: t.TempDir()}
	c := &workflowCmd{sc: sc}

	if err := c.Run(context.Background(), "validate"); err != nil {
		t.Fatal(err)
	}
	out := chat.GetText(true)
	if !strings.Contains(out, "usage") {
		t.Errorf("expected usage hint when name is missing: %q", out)
	}
}

func TestWorkflowValidate_UnknownWorkflow(t *testing.T) {
	chat := tview.NewTextView()
	sc := &slashContext{chat: chat, cwd: t.TempDir()}
	c := &workflowCmd{sc: sc}

	if err := c.Run(context.Background(), "validate ghost-workflow"); err != nil {
		t.Fatal(err)
	}
	out := chat.GetText(true)
	if !strings.Contains(out, "validate") {
		t.Errorf("expected 'validate' label in error: %q", out)
	}
}

func TestWorkflowValidate_DoesNotRunWorkflow(t *testing.T) {
	// Sanity-guard: the validate path must never call workflow.Run,
	// which would require runDeps wiring (Providers, Bus, etc.) that
	// the slash context here doesn't have. If Run were invoked we'd
	// nil-panic on c.sc.runDeps fields.
	cwd := writeWorkflow(t, "good", `---
roles:
  one: {}
---

## one

Do the thing.
`)
	chat := tview.NewTextView()
	sc := &slashContext{chat: chat, cwd: cwd}
	c := &workflowCmd{sc: sc}

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("validate path panicked (workflow.Run probably called): %v", r)
		}
	}()
	if err := c.Run(context.Background(), "validate good"); err != nil {
		t.Fatal(err)
	}
}
