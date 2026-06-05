// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeBashCommand(t *testing.T) {
	cases := map[string]string{
		"go test ./...":          "go test",
		"git status -s":          "git status",
		"ls -la":                 "ls",
		"npm install foo":        "npm install",
		"git diff | head -n 20":  "git diff",
		"FOO=bar go build ./...": "go build",
		"./scripts/run.sh arg":   "./scripts/run.sh",
		"cat /etc/hosts":         "cat", // second arg has a slash → not a subcommand
		"":                       "",
	}
	for in, want := range cases {
		if got := NormalizeBashCommand(in); got != want {
			t.Errorf("NormalizeBashCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestComputeBashOutputStats(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w, err := NewSession(s, "qwen3.6", "local", "/p1")
	if err != nil {
		t.Fatal(err)
	}

	big := strings.Repeat("noisy build output line\n", 500) // lots of tokens
	if err := w.AppendToolCall("c1", "bash", map[string]any{"cmd": "go test ./..."}, "short", big, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := w.AppendToolCall("c2", "bash", map[string]any{"cmd": "go test ./pkg"}, "short", big, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := w.AppendToolCall("c3", "bash", map[string]any{"cmd": "ls -la"}, "a\nb\nc", "", "ok"); err != nil {
		t.Fatal(err)
	}
	// A non-bash call must be ignored.
	if err := w.AppendToolCall("c4", "read", map[string]any{}, "x", "x", "ok"); err != nil {
		t.Fatal(err)
	}

	stats, err := ComputeBashOutputStats(s, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected 2 distinct bash commands, got %d: %+v", len(stats), stats)
	}
	// Highest RawTokens first → "go test" (2 big runs).
	if stats[0].Command != "go test" {
		t.Errorf("expected go test ranked first, got %q", stats[0].Command)
	}
	if stats[0].Runs != 2 {
		t.Errorf("go test runs = %d, want 2", stats[0].Runs)
	}
	if stats[0].RawTokens <= stats[1].RawTokens {
		t.Errorf("ranking wrong: %+v", stats)
	}
	// "ls" used llm_output as raw fallback (full_output empty).
	if stats[1].Command != "ls" || stats[1].RawTokens == 0 {
		t.Errorf("ls stat wrong: %+v", stats[1])
	}
}
