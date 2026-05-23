// SPDX-License-Identifier: AGPL-3.0-or-later

package lsp

import (
	"strings"
	"testing"
)

// TestFilterBySeverity pins the severity-threshold semantics: lower
// number is higher severity (Error=1 most severe, Hint=4 least), so a
// threshold of SeverityError keeps only entries at severity 0 (treated
// as Error) or 1. A threshold of SeverityWarning admits severities 1 and 2.
func TestFilterBySeverity(t *testing.T) {
	in := []Diagnostic{
		{Message: "err", Severity: SeverityError},
		{Message: "warn", Severity: SeverityWarning},
		{Message: "info", Severity: SeverityInformation},
		{Message: "hint", Severity: SeverityHint},
		{Message: "missing", Severity: 0}, // unset → treated as Error
	}

	cases := []struct {
		name string
		min  int
		want []string
	}{
		{"errors only", SeverityError, []string{"err", "missing"}},
		{"errors + warnings", SeverityWarning, []string{"err", "warn", "missing"}},
		{"everything", SeverityHint, []string{"err", "warn", "info", "hint", "missing"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterBySeverity(in, tc.min)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d entries, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, d := range got {
				if d.Message != tc.want[i] {
					t.Errorf("[%d] got %q, want %q", i, d.Message, tc.want[i])
				}
			}
		})
	}
}

// TestRenderDiagnostics_FormatAndSort confirms the model-facing render
// shape: header line + one entry per diagnostic, sorted by (line, col)
// regardless of input order, with 1-indexed positions.
func TestRenderDiagnostics_FormatAndSort(t *testing.T) {
	diags := []Diagnostic{
		// Intentionally out of order to exercise the sort.
		{Range: rangeAt(5, 0), Severity: SeverityError, Message: "second"},
		{Range: rangeAt(0, 2), Severity: SeverityError, Message: "first"},
		{Range: rangeAt(5, 4), Severity: SeverityWarning, Message: "third"},
	}
	out := renderDiagnostics("foo/bar.go", diags, 10)

	if !strings.HasPrefix(out, "\n\n[LSP diagnostics for foo/bar.go]\n") {
		t.Errorf("header line missing or malformed: %q", out)
	}
	// Sort order: line 0 col 2 → line 5 col 0 → line 5 col 4
	// 1-indexed in render: 1:3, 6:1, 6:5
	wantOrder := []string{
		"foo/bar.go:1:3: error: first",
		"foo/bar.go:6:1: error: second",
		"foo/bar.go:6:5: warning: third",
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1+len(wantOrder) {
		t.Fatalf("got %d lines, want %d: %s", len(lines), 1+len(wantOrder), out)
	}
	for i, want := range wantOrder {
		if lines[1+i] != want {
			t.Errorf("[%d] got %q, want %q", i, lines[1+i], want)
		}
	}
}

// TestRenderDiagnostics_MaxLinesTruncation verifies the long-list cap:
// MaxLines=2 against a 5-entry input renders 2 entries and a footer
// stating how many were dropped, so the model sees a hint that there's
// more to fix rather than acting on a partial picture.
func TestRenderDiagnostics_MaxLinesTruncation(t *testing.T) {
	diags := []Diagnostic{
		{Range: rangeAt(0, 0), Severity: SeverityError, Message: "a"},
		{Range: rangeAt(1, 0), Severity: SeverityError, Message: "b"},
		{Range: rangeAt(2, 0), Severity: SeverityError, Message: "c"},
		{Range: rangeAt(3, 0), Severity: SeverityError, Message: "d"},
		{Range: rangeAt(4, 0), Severity: SeverityError, Message: "e"},
	}
	out := renderDiagnostics("x.go", diags, 2)
	if !strings.Contains(out, "x.go:1:1: error: a") {
		t.Errorf("missing first entry: %s", out)
	}
	if !strings.Contains(out, "x.go:2:1: error: b") {
		t.Errorf("missing second entry: %s", out)
	}
	if !strings.Contains(out, "(3 more not shown)") {
		t.Errorf("missing truncation footer: %s", out)
	}
	if strings.Contains(out, "x.go:3:") {
		t.Errorf("third entry leaked past maxLines: %s", out)
	}
}

// TestNotifierOptions_Defaults pins the documented defaults: Wait
// 500ms, Dedup 100ms, MinSeverity Error, MaxLines 10. Drift here
// silently changes user-visible behaviour.
func TestNotifierOptions_Defaults(t *testing.T) {
	o := NotifierOptions{}
	if o.wait().Milliseconds() != 500 {
		t.Errorf("wait default: got %v, want 500ms", o.wait())
	}
	if o.dedup().Milliseconds() != 100 {
		t.Errorf("dedup default: got %v, want 100ms", o.dedup())
	}
	if o.minSeverity() != SeverityError {
		t.Errorf("minSeverity default: got %d, want SeverityError(1)", o.minSeverity())
	}
	if o.maxLines() != 10 {
		t.Errorf("maxLines default: got %d, want 10", o.maxLines())
	}
}

// TestNotifier_NilManagerReturnsEmpty covers the disabled path: a
// Notifier with no Manager (or a nil receiver) must never block or
// crash; it just returns "" so the caller's tool result is unchanged.
func TestNotifier_NilManagerReturnsEmpty(t *testing.T) {
	var n *Notifier
	if got := n.NotifyWrite(t.Context(), "/abs/x.go"); got != "" {
		t.Errorf("nil notifier returned %q, want empty", got)
	}
	n2 := &Notifier{Manager: nil}
	if got := n2.NotifyWrite(t.Context(), "/abs/x.go"); got != "" {
		t.Errorf("manager=nil returned %q, want empty", got)
	}
}

func rangeAt(line, char int) Range {
	return Range{
		Start: Position{Line: line, Character: char},
		End:   Position{Line: line, Character: char + 1},
	}
}
