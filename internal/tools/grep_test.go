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
