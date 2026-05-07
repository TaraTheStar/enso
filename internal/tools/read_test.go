// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadTool_FullFile(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "hello.txt"), "alpha\nbeta\ngamma\n")

	ac := newToolAC(tmp)
	res, err := ReadTool{}.Run(context.Background(),
		map[string]any{"path": "hello.txt"}, ac)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(res.LLMOutput, want) {
			t.Errorf("missing %q in:\n%s", want, res.LLMOutput)
		}
	}
	// Output is line-numbered like `cat -n`.
	if !strings.Contains(res.LLMOutput, "     1  alpha") {
		t.Errorf("expected `     1  alpha` line-numbered prefix in:\n%s", res.LLMOutput)
	}
	abs, _ := filepath.Abs(filepath.Join(tmp, "hello.txt"))
	if !ac.ReadSet[abs] {
		t.Errorf("ReadSet missing entry for %s", abs)
	}
}

func TestReadTool_LineRange(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "many.txt"), "one\ntwo\nthree\nfour\nfive\n")

	ac := newToolAC(tmp)
	res, err := ReadTool{}.Run(context.Background(),
		map[string]any{
			"path":       "many.txt",
			"first_line": float64(2),
			"last_line":  float64(4),
		}, ac)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"two", "three", "four"} {
		if !strings.Contains(res.LLMOutput, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, dontWant := range []string{"one\n", "five\n"} {
		if strings.Contains(res.LLMOutput, dontWant) {
			t.Errorf("unexpected line %q in range output", dontWant)
		}
	}
}

func TestReadTool_MissingFile(t *testing.T) {
	tmp := t.TempDir()
	ac := newToolAC(tmp)
	_, err := ReadTool{}.Run(context.Background(),
		map[string]any{"path": "nope.txt"}, ac)
	if err == nil {
		t.Errorf("missing file: want error")
	}
}

func TestReadTool_RequiresPath(t *testing.T) {
	ac := newToolAC(t.TempDir())
	_, err := ReadTool{}.Run(context.Background(), map[string]any{}, ac)
	if err == nil {
		t.Errorf("empty path: want error")
	}
}

// helpers shared by all tools_*_test.go files

func newToolAC(cwd string) *AgentContext {
	return &AgentContext{Cwd: cwd, ReadSet: map[string]bool{}}
}

func mustWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
