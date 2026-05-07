// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditTool_ExactReplace(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "f.txt"), "alpha beta gamma\n")
	ac := newToolAC(tmp)

	res, err := EditTool{}.Run(context.Background(), map[string]any{
		"path":       "f.txt",
		"old_string": "beta",
		"new_string": "BETA",
	}, ac)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "edited") || !strings.Contains(res.LLMOutput, "1 replacement") {
		t.Errorf("output = %q", res.LLMOutput)
	}
	got, _ := os.ReadFile(filepath.Join(tmp, "f.txt"))
	if string(got) != "alpha BETA gamma\n" {
		t.Errorf("contents = %q", got)
	}
	// Display should carry the unified diff for the modal.
	if diff, ok := res.Display.(string); !ok || !strings.Contains(diff, "+alpha BETA gamma") {
		t.Errorf("Display = %v, want unified diff", res.Display)
	}
}

func TestEditTool_AmbiguousMatchRefused(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "f.txt"), "x x x\n")
	ac := newToolAC(tmp)

	res, err := EditTool{}.Run(context.Background(), map[string]any{
		"path":       "f.txt",
		"old_string": "x",
		"new_string": "Y",
	}, ac)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "appears") {
		t.Errorf("expected ambiguity refusal, got: %q", res.LLMOutput)
	}
	// File must be unchanged.
	got, _ := os.ReadFile(filepath.Join(tmp, "f.txt"))
	if string(got) != "x x x\n" {
		t.Errorf("file modified despite refusal: %q", got)
	}
}

func TestEditTool_ReplaceAll(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "f.txt"), "x x x\n")
	ac := newToolAC(tmp)

	if _, err := (EditTool{}).Run(context.Background(), map[string]any{
		"path":        "f.txt",
		"old_string":  "x",
		"new_string":  "Y",
		"replace_all": true,
	}, ac); err != nil {
		t.Fatalf("edit: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(tmp, "f.txt"))
	if string(got) != "Y Y Y\n" {
		t.Errorf("contents = %q, want all-replaced", got)
	}
}

func TestEditTool_NotFound(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "f.txt"), "alpha\n")
	ac := newToolAC(tmp)

	res, _ := EditTool{}.Run(context.Background(), map[string]any{
		"path":       "f.txt",
		"old_string": "ZZZ",
		"new_string": "WWW",
	}, ac)
	if !strings.Contains(res.LLMOutput, "not found") {
		t.Errorf("output = %q", res.LLMOutput)
	}
}
