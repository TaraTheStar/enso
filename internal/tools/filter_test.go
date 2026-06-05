// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"strings"
	"testing"
)

// TestEmbeddedFiltersSelfTest is the safety net for the shipped defaults:
// every embedded filter must compile and pass its own inline tests. A
// broken default filter fails here rather than silently mangling output in
// production.
func TestEmbeddedFiltersSelfTest(t *testing.T) {
	fs := LoadFilterSet("", nil)
	if len(fs.Names()) == 0 {
		t.Fatal("no embedded filters loaded")
	}
	if err := fs.RunSelfTests(); err != nil {
		t.Fatalf("embedded filter self-test failed: %v", err)
	}
	t.Logf("loaded embedded filters: %v", fs.Names())
}

func TestFilterMatchAndApply(t *testing.T) {
	fs := LoadFilterSet("", nil)

	in := "=== RUN   TestX\n--- PASS: TestX (0.00s)\nok  \tgithub.com/x\t0.01s\n"
	out, changed := fs.Apply("go test ./...", in)
	if !changed {
		t.Fatal("expected go test output to be filtered")
	}
	if strings.Contains(out, "=== RUN") || strings.Contains(out, "--- PASS") {
		t.Fatalf("passing scaffolding not stripped: %q", out)
	}

	// An uncovered command is passed through untouched.
	out2, changed2 := fs.Apply("cat README.md", in)
	if changed2 || out2 != in {
		t.Fatalf("uncovered command should pass through unchanged")
	}
}

func TestFilterOverrideByName(t *testing.T) {
	fs := NewFilterSet()
	a := &Filter{Name: "x", MatchCommand: "foo", OnEmpty: "first"}
	if err := a.compile(); err != nil {
		t.Fatal(err)
	}
	b := &Filter{Name: "x", MatchCommand: "foo", OnEmpty: "second"}
	if err := b.compile(); err != nil {
		t.Fatal(err)
	}
	fs.Add(a)
	fs.Add(b)
	if got := len(fs.Names()); got != 1 {
		t.Fatalf("override-by-name should keep 1 filter, got %d", got)
	}
	out, _ := fs.Apply("foo", "")
	if out != "second" {
		t.Fatalf("expected override to win, got %q", out)
	}
}

func TestFilterPriority(t *testing.T) {
	// Two distinctly-named filters match the same command. Default priority
	// (0) means load order wins; a higher priority overrides that regardless
	// of load order.
	mk := func(name, onEmpty string, prio int) *Filter {
		f := &Filter{Name: name, MatchCommand: "foo", OnEmpty: onEmpty, Priority: prio}
		if err := f.compile(); err != nil {
			t.Fatal(err)
		}
		return f
	}

	// Equal priority: first loaded wins (stable tie-break preserves the old
	// first-match-wins behaviour).
	tie := NewFilterSet()
	tie.Add(mk("a", "first", 0))
	tie.Add(mk("b", "second", 0))
	if out, _ := tie.Apply("foo", ""); out != "first" {
		t.Fatalf("equal priority should keep load order, got %q", out)
	}

	// Higher priority wins even though it is loaded last.
	pr := NewFilterSet()
	pr.Add(mk("a", "low", 0))
	pr.Add(mk("b", "high", 5))
	if out, _ := pr.Apply("foo", ""); out != "high" {
		t.Fatalf("higher priority should win, got %q", out)
	}

	// Higher priority wins even when loaded first.
	pr2 := NewFilterSet()
	pr2.Add(mk("a", "high", 5))
	pr2.Add(mk("b", "low", 0))
	if out, _ := pr2.Apply("foo", ""); out != "high" {
		t.Fatalf("higher priority should win regardless of order, got %q", out)
	}
}

func TestFilterStripANSI(t *testing.T) {
	f := &Filter{Name: "x", MatchCommand: ".", StripANSI: true}
	if err := f.compile(); err != nil {
		t.Fatal(err)
	}
	out, _ := f.Apply("\x1b[31mred\x1b[0m text")
	if out != "red text" {
		t.Fatalf("ansi not stripped: %q", out)
	}
}

func TestFilterCovers(t *testing.T) {
	fs := LoadFilterSet("", nil)
	if !fs.Covers("git status") {
		t.Fatal("git status should be covered")
	}
	if fs.Covers("some-random-binary --flag") {
		t.Fatal("random command should not be covered")
	}
}
