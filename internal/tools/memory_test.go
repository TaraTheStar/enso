// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"db test policy", "db-test-policy"},
		{"DB Test Policy!!!", "db-test-policy"},
		{"  trailing  spaces  ", "trailing-spaces"},
		{"weird/chars\\..with-dashes", "weird-chars-with-dashes"},
		{"---only-dashes---", "only-dashes"},
		{"", ""},
		{"!!!", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := slugify(tc.in); got != tc.want {
				t.Errorf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestMemoryToolWritesProjectFile(t *testing.T) {
	cwd := t.TempDir()
	tool := MemoryTool{}
	ac := &AgentContext{Cwd: cwd}

	out, err := tool.Run(context.Background(), map[string]any{
		"name":    "Db Test Policy",
		"content": "Integration tests must hit a real database.\n",
	}, ac)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.LLMOutput, "memory saved") {
		t.Errorf("LLMOutput = %q", out.LLMOutput)
	}

	path := filepath.Join(cwd, ".enso", "memory", "db-test-policy.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("memory file not at expected path %s: %v", path, err)
	}
	if !strings.Contains(string(body), "real database") {
		t.Errorf("body = %q", body)
	}
}

func TestMemoryToolRejectsEmptyContent(t *testing.T) {
	tool := MemoryTool{}
	ac := &AgentContext{Cwd: t.TempDir()}
	_, err := tool.Run(context.Background(), map[string]any{"name": "x", "content": "   "}, ac)
	if err == nil {
		t.Errorf("expected error for empty content")
	}
}

func TestMemoryToolRejectsUnnamed(t *testing.T) {
	tool := MemoryTool{}
	ac := &AgentContext{Cwd: t.TempDir()}
	_, err := tool.Run(context.Background(), map[string]any{"name": "!!!", "content": "hi"}, ac)
	if err == nil {
		t.Errorf("expected error when name slugifies to empty")
	}
}

func TestMemoryToolOverwrites(t *testing.T) {
	cwd := t.TempDir()
	tool := MemoryTool{}
	ac := &AgentContext{Cwd: cwd}

	if _, err := tool.Run(context.Background(), map[string]any{"name": "x", "content": "first"}, ac); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Run(context.Background(), map[string]any{"name": "x", "content": "second"}, ac); err != nil {
		t.Fatal(err)
	}

	body, _ := os.ReadFile(filepath.Join(cwd, ".enso", "memory", "x.md"))
	if !strings.Contains(string(body), "second") || strings.Contains(string(body), "first") {
		t.Errorf("expected overwrite, got %q", body)
	}
}
