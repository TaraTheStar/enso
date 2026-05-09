// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestGrepTool_FindsMatches validates that grep returns hits regardless of
// whether `rg` is on PATH (the tool prefers ripgrep but falls back to a
// regex walker; both code paths should produce hits for the same regex).
func TestGrepTool_FindsMatches(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "a.txt"), "hello world\nbeta\n")
	mustWriteFile(t, filepath.Join(tmp, "b.txt"), "alpha\nworld peace\n")
	ac := newToolAC(tmp)

	res, err := GrepTool{}.Run(context.Background(),
		map[string]any{"pattern": "world", "path": tmp}, ac)
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "a.txt") || !strings.Contains(res.LLMOutput, "b.txt") {
		t.Errorf("missing expected hits in:\n%s", res.LLMOutput)
	}
	if !strings.Contains(res.LLMOutput, "hello world") {
		t.Errorf("missing match line in:\n%s", res.LLMOutput)
	}
}

func TestGrepTool_DisplayOutput(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "a.txt"), "world\nworld\n")
	mustWriteFile(t, filepath.Join(tmp, "b.txt"), "world\n")
	mustWriteFile(t, filepath.Join(tmp, "skip.txt"), "nope\n")
	ac := newToolAC(tmp)

	res, _ := GrepTool{}.Run(context.Background(),
		map[string]any{"pattern": "world", "path": tmp}, ac)
	// Three matches across two files, regardless of rg-vs-walker code path.
	if res.DisplayOutput != "3 matches in 2 files" {
		t.Errorf("display = %q, want `3 matches in 2 files`", res.DisplayOutput)
	}
}

func TestGrepDisplay(t *testing.T) {
	cases := map[string]string{
		"/a.txt:1:hello":                       "1 match in 1 file",
		"/a.txt:1:hello\n/a.txt:5:world":       "2 matches in 1 file",
		"/a.txt:1:hello\n/b.txt:1:hello":       "2 matches in 2 files",
		"/a.txt:1:x\n/a.txt:2:y\n/b.txt:1:z\n": "3 matches in 2 files",
	}
	for in, want := range cases {
		if got := grepDisplay(in); got != want {
			t.Errorf("grepDisplay(%q) = %q want %q", in, got, want)
		}
	}
}

func TestGrepTool_NoMatches(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "a.txt"), "alpha\n")
	ac := newToolAC(tmp)

	res, err := GrepTool{}.Run(context.Background(),
		map[string]any{"pattern": "ZZZNEVERFOUND", "path": tmp}, ac)
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "no matches") {
		t.Errorf("output = %q", res.LLMOutput)
	}
}
