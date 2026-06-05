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
