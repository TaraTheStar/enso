// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobTool_DoublestarPattern(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "src", "a.go"), "")
	mustWriteFile(t, filepath.Join(tmp, "src", "deep", "b.go"), "")
	mustWriteFile(t, filepath.Join(tmp, "src", "ignore.txt"), "")
	mustWriteFile(t, filepath.Join(tmp, "README.md"), "")

	ac := newToolAC(tmp)
	res, err := GlobTool{}.Run(context.Background(),
		map[string]any{"pattern": "**/*.go", "path": tmp}, ac)
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "a.go") || !strings.Contains(res.LLMOutput, "b.go") {
		t.Errorf("missing .go hits in:\n%s", res.LLMOutput)
	}
	if strings.Contains(res.LLMOutput, "ignore.txt") || strings.Contains(res.LLMOutput, "README.md") {
		t.Errorf("non-Go files leaked into result:\n%s", res.LLMOutput)
	}
	if !strings.Contains(res.LLMOutput, "found 2 match") {
		t.Errorf("expected `found 2 match`, got:\n%s", res.LLMOutput)
	}
}

func TestGlobTool_NoMatches(t *testing.T) {
	tmp := t.TempDir()
	ac := newToolAC(tmp)
	res, err := GlobTool{}.Run(context.Background(),
		map[string]any{"pattern": "**/*.nonsense", "path": tmp}, ac)
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "no matches") {
		t.Errorf("output = %q", res.LLMOutput)
	}
}

func TestGlobTool_DefaultsToCwd(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "x.md"), "")
	ac := newToolAC(tmp)

	res, err := GlobTool{}.Run(context.Background(),
		map[string]any{"pattern": "*.md"}, ac)
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "x.md") {
		t.Errorf("expected x.md when path defaults to Cwd:\n%s", res.LLMOutput)
	}
}
