// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
)

func TestSlugifyLabel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"   ", ""},
		{"hello", "hello"},
		{"Hello World", "hello-world"},
		{"  Leading and trailing  ", "leading-and-trailing"},
		{"Why is `Foo()` broken?", "why-is-foo-broken"},
		{"Multiple   spaces---and___punct!!", "multiple-spaces-and-punct"},
		{"unicode café résumé", "unicode-caf-r-sum"},
		// Cap is 30 chars; trailing dash trimmed after the cut.
		{"summarize-the-readme-and-tell-me-what-it-does", "summarize-the-readme-and-tell"},
		{"a", "a"},
		{"!!!only-punct-around!!!", "only-punct-around"},
		{"123 numbers ok", "123-numbers-ok"},
	}
	for _, tc := range cases {
		got := SlugifyLabel(tc.in)
		if got != tc.want {
			t.Errorf("SlugifyLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if len(got) > labelMaxLen {
			t.Errorf("SlugifyLabel(%q) = %q exceeds cap %d", tc.in, got, labelMaxLen)
		}
		if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
			t.Errorf("SlugifyLabel(%q) = %q has dangling hyphen", tc.in, got)
		}
	}
}

func TestAutoLabel_FirstUserMessage(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w, err := NewSession(s, "m", "p", "/c")
	if err != nil {
		t.Fatal(err)
	}
	// Fresh session: no label.
	if got, err := w.Label(); err != nil || got != "" {
		t.Fatalf("fresh label: got %q, err %v; want empty", got, err)
	}

	// First user message auto-derives the label.
	if err := w.AppendMessage(llm.Message{Role: "user", Content: "Fix the flaky auth test"}, ""); err != nil {
		t.Fatalf("append: %v", err)
	}
	want := "fix-the-flaky-auth-test"
	got, err := w.Label()
	if err != nil || got != want {
		t.Errorf("after first user: got %q (%v), want %q", got, err, want)
	}

	// Subsequent user messages don't overwrite.
	if err := w.AppendMessage(llm.Message{Role: "user", Content: "Now do something else"}, ""); err != nil {
		t.Fatalf("append 2: %v", err)
	}
	got, _ = w.Label()
	if got != want {
		t.Errorf("after second user: got %q, want %q (must not overwrite)", got, want)
	}
}

func TestAutoLabel_SkipsNonUserAndSubagent(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w, err := NewSession(s, "m", "p", "/c")
	if err != nil {
		t.Fatal(err)
	}

	// Assistant + tool messages don't trigger auto-label.
	_ = w.AppendMessage(llm.Message{Role: "assistant", Content: "I'll start by reading"}, "")
	_ = w.AppendMessage(llm.Message{Role: "tool", Content: "ok", ToolCallID: "x"}, "")
	if got, _ := w.Label(); got != "" {
		t.Errorf("after non-user messages: got %q, want empty", got)
	}

	// Sub-agent user messages don't trigger top-level auto-label.
	_ = w.AppendMessage(llm.Message{Role: "user", Content: "sub-agent prompt"}, "sub-1")
	if got, _ := w.Label(); got != "" {
		t.Errorf("after sub-agent user: got %q, want empty (must only fire on top-level)", got)
	}

	// Top-level user message finally triggers it.
	_ = w.AppendMessage(llm.Message{Role: "user", Content: "actual top-level prompt"}, "")
	if got, _ := w.Label(); got != "actual-top-level-prompt" {
		t.Errorf("after top-level user: got %q", got)
	}
}

func TestAutoLabel_EmptyContentLeavesUnset(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w, err := NewSession(s, "m", "p", "/c")
	if err != nil {
		t.Fatal(err)
	}
	// Pure-punctuation content slugifies to empty; label stays unset so
	// the *next* substantive message wins.
	_ = w.AppendMessage(llm.Message{Role: "user", Content: "???"}, "")
	if got, _ := w.Label(); got != "" {
		t.Errorf("after punct-only: got %q, want empty", got)
	}
	_ = w.AppendMessage(llm.Message{Role: "user", Content: "now a real one"}, "")
	if got, _ := w.Label(); got != "now-a-real-one" {
		t.Errorf("after real msg: got %q", got)
	}
}

func TestSetLabel_OverridesAndNormalizes(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w, err := NewSession(s, "m", "p", "/c")
	if err != nil {
		t.Fatal(err)
	}
	// Auto-label first.
	_ = w.AppendMessage(llm.Message{Role: "user", Content: "investigate the build"}, "")

	// /rename overrides regardless of existing label and applies the
	// same slug normalisation as auto-derivation.
	if err := w.SetLabel("Refactor: Auth Flow!"); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, _ := w.Label()
	if got != "refactor-auth-flow" {
		t.Errorf("after /rename: got %q, want refactor-auth-flow", got)
	}

	// Empty input clears.
	if err := w.SetLabel(""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ = w.Label()
	if got != "" {
		t.Errorf("after clear: got %q, want empty", got)
	}
	// After clearing, the *next* user message re-arms auto-derivation.
	_ = w.AppendMessage(llm.Message{Role: "user", Content: "round two"}, "")
	got, _ = w.Label()
	if got != "round-two" {
		t.Errorf("after re-arm: got %q, want round-two", got)
	}
}

func TestLoadAndListRecent_RoundtripLabel(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w, err := NewSession(s, "m", "p", "/c")
	if err != nil {
		t.Fatal(err)
	}
	_ = w.AppendMessage(llm.Message{Role: "user", Content: "Run the deploy script"}, "")

	state, err := Load(s, w.SessionID())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if state.Info.Label != "run-the-deploy-script" {
		t.Errorf("Load: label = %q", state.Info.Label)
	}

	infos, err := ListRecent(s, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(infos) == 0 || infos[0].Label != "run-the-deploy-script" {
		t.Errorf("ListRecent: label = %q (infos=%+v)", func() string {
			if len(infos) > 0 {
				return infos[0].Label
			}
			return "(no rows)"
		}(), infos)
	}

	withStats, err := ListRecentWithStats(s, 10)
	if err != nil {
		t.Fatalf("list with stats: %v", err)
	}
	if len(withStats) == 0 || withStats[0].Label != "run-the-deploy-script" {
		t.Errorf("ListRecentWithStats: label = %q", func() string {
			if len(withStats) > 0 {
				return withStats[0].Label
			}
			return "(no rows)"
		}())
	}
}
