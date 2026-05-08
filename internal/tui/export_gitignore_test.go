// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureEnsoGitignore_CreatesFile(t *testing.T) {
	cwd := t.TempDir()
	if err := ensureEnsoGitignore(cwd); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(cwd, ".enso", ".gitignore"))
	if err != nil {
		t.Fatalf("expected gitignore created: %v", err)
	}
	if !strings.Contains(string(got), "exports/") {
		t.Errorf(".gitignore missing exports/: %q", got)
	}
}

func TestEnsureEnsoGitignore_IdempotentWhenRuleExists(t *testing.T) {
	cwd := t.TempDir()
	ensoDir := filepath.Join(cwd, ".enso")
	if err := os.MkdirAll(ensoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	original := "# project notes\nexports/\n"
	path := filepath.Join(ensoDir, ".gitignore")
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureEnsoGitignore(cwd); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("file changed when rule already present\noriginal: %q\nafter: %q",
			original, string(got))
	}
}

func TestEnsureEnsoGitignore_AppendsToExistingFile(t *testing.T) {
	cwd := t.TempDir()
	ensoDir := filepath.Join(cwd, ".enso")
	if err := os.MkdirAll(ensoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ensoDir, ".gitignore")
	if err := os.WriteFile(path, []byte("trust.json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureEnsoGitignore(cwd); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "trust.json") {
		t.Errorf("existing line lost: %q", got)
	}
	if !strings.Contains(string(got), "exports/") {
		t.Errorf("exports/ line not appended: %q", got)
	}
}

func TestEnsureEnsoGitignore_AppendsNewlineIfMissing(t *testing.T) {
	// File missing trailing newline must still get a clean append
	// rather than 'foo bar exports/' on one line.
	cwd := t.TempDir()
	ensoDir := filepath.Join(cwd, ".enso")
	if err := os.MkdirAll(ensoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ensoDir, ".gitignore")
	if err := os.WriteFile(path, []byte("trust.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureEnsoGitignore(cwd); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(got), "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 lines, got %d: %q", len(lines), got)
	}
	if lines[1] != "exports/" {
		t.Errorf("line 2=%q, want 'exports/'", lines[1])
	}
}

func TestFormatThousandsTUI(t *testing.T) {
	cases := map[int]string{
		0:        "0",
		999:      "999",
		1000:     "1,000",
		12345:    "12,345",
		1234567:  "1,234,567",
		-1234567: "-1,234,567",
	}
	for in, want := range cases {
		if got := formatThousands(in); got != want {
			t.Errorf("formatThousands(%d)=%q, want %q", in, got, want)
		}
	}
}
