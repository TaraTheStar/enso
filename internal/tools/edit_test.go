// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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

// TestEditTool_EmptyOldString guards C1: an empty old_string used to slip
// past the not-found guard (strings.Count(s, "") == len+1) and, with
// replace_all, splice new_string between every byte. It must be rejected and
// the file left untouched.
func TestEditTool_EmptyOldString(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "f.txt"), "abc\n")
	ac := newToolAC(tmp)

	res, err := EditTool{}.Run(context.Background(), map[string]any{
		"path":        "f.txt",
		"old_string":  "",
		"new_string":  "X",
		"replace_all": true,
	}, ac)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "must not be empty") {
		t.Errorf("output = %q, want empty-old_string rejection", res.LLMOutput)
	}
	got, _ := os.ReadFile(filepath.Join(tmp, "f.txt"))
	if string(got) != "abc\n" {
		t.Errorf("file corrupted despite rejection: %q", got)
	}
}

// TestEditTool_PreservesFileMode guards M11: the atomic rewrite must keep
// the original file's permission bits — editing a 0755 script used to
// silently strip the exec bit via a hardcoded 0o644.
func TestEditTool_PreservesFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	tmp := t.TempDir()
	script := filepath.Join(tmp, "run.sh")
	mustWriteFile(t, script, "#!/bin/sh\necho old\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	ac := newToolAC(tmp)

	if _, err := (EditTool{}).Run(context.Background(), map[string]any{
		"path":       "run.sh",
		"old_string": "old",
		"new_string": "new",
	}, ac); err != nil {
		t.Fatalf("edit: %v", err)
	}
	fi, err := os.Stat(script)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %o, want 755 preserved", fi.Mode().Perm())
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
