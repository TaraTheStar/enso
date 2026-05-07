// SPDX-License-Identifier: AGPL-3.0-or-later

package picker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWalkSkipsNoiseDirs(t *testing.T) {
	root := t.TempDir()

	files := []string{
		"main.go",
		"README.md",
		"sub/thing.txt",
		".git/HEAD",                     // hidden — skip
		".enso/config.toml",             // hidden — skip
		"node_modules/leftpad/index.js", // hard skip
		"vendor/foo/bar.go",             // hard skip
		"bin/enso",                      // hard skip
		"target/debug/x",                // hard skip
		"src/lib.rs",
		"docs/foo.md",
	}
	for _, f := range files {
		path := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"README.md":     true,
		"docs/foo.md":   true,
		"main.go":       true,
		"src/lib.rs":    true,
		"sub/thing.txt": true,
	}
	if len(got) != len(want) {
		t.Errorf("Walk returned %d files, want %d: %v", len(got), len(want), got)
	}
	for _, f := range got {
		if !want[f] {
			t.Errorf("Walk surfaced unexpected file %q", f)
		}
	}
}

func TestRankExactBasenameWinsOverContains(t *testing.T) {
	files := []string{
		"deep/nested/path/agent.go",
		"agent.go",
		"agents/agent.go",
		"docs/agents.md",
		"agentcontext.go",
	}
	got := Rank(files, "agent.go", 5)
	if len(got) == 0 || got[0] != "agent.go" {
		t.Fatalf("expected exact match 'agent.go' first, got %v", got)
	}
}

func TestRankShorterPathsBeatDeepOnes(t *testing.T) {
	files := []string{
		"a/b/c/d/foo.go",
		"foo.go",
		"a/foo.go",
	}
	got := Rank(files, "foo", 5)
	if got[0] != "foo.go" {
		t.Errorf("shortest path should win in same tier, got %v", got)
	}
}

func TestRankPathContainsTier(t *testing.T) {
	files := []string{
		"unrelated/x.go",
		"src/agent/handler.go", // path contains "agent"
	}
	got := Rank(files, "agent", 5)
	if len(got) != 1 || got[0] != "src/agent/handler.go" {
		t.Errorf("path-contains tier should still rank, got %v", got)
	}
}

func TestRankEmptyQueryReturnsCappedSlice(t *testing.T) {
	files := []string{"a", "b", "c", "d", "e"}
	got := Rank(files, "", 3)
	if len(got) != 3 {
		t.Errorf("empty query with limit=3 should return 3, got %d", len(got))
	}
}

func TestRankNoMatch(t *testing.T) {
	got := Rank([]string{"a.go", "b.go"}, "ZZZ", 5)
	if len(got) != 0 {
		t.Errorf("no match → empty slice, got %v", got)
	}
}

func TestWalkAllExtras(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extra, "notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := WalkAll(root, []string{extra}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Project files come back relative; extras come back absolute.
	hasMain, hasNotes := false, false
	for _, p := range got {
		if p == "main.go" {
			hasMain = true
		}
		if filepath.Base(p) == "notes.md" && filepath.IsAbs(p) {
			hasNotes = true
		}
	}
	if !hasMain || !hasNotes {
		t.Errorf("WalkAll should include both project file (relative) and extra dir file (absolute); got %v", got)
	}
}

func TestWalkAllRespectsIgnore(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"main.go", ".env", "secrets/key.pem", "src/x.go"} {
		path := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// `.env` is hidden, so the walker already skips it via the dot rule.
	// Use `secrets/**` to test the ignore path.
	got, err := WalkAll(root, nil, []string{"secrets/**", "*.pem"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		if filepath.Base(p) == "key.pem" || (len(p) > 7 && p[:7] == "secrets") {
			t.Errorf("ignore-pattern violation: %q in result", p)
		}
	}
}
