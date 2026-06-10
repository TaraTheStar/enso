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

func TestWriteTool_NewFile(t *testing.T) {
	tmp := t.TempDir()
	ac := newToolAC(tmp)

	res, err := WriteTool{}.Run(context.Background(),
		map[string]any{"path": "new.txt", "content": "hello\nworld\n"}, ac)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "wrote") {
		t.Errorf("output = %q", res.LLMOutput)
	}
	got, err := os.ReadFile(filepath.Join(tmp, "new.txt"))
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(got) != "hello\nworld\n" {
		t.Errorf("contents = %q", got)
	}
}

func TestWriteTool_ExistingFileRequiresPriorRead(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "existing.txt"), "original\n")
	ac := newToolAC(tmp)

	res, err := WriteTool{}.Run(context.Background(),
		map[string]any{"path": "existing.txt", "content": "replacement\n"}, ac)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "not read in this session") {
		t.Errorf("expected refusal, got: %q", res.LLMOutput)
	}
	// File must be unchanged.
	got, _ := os.ReadFile(filepath.Join(tmp, "existing.txt"))
	if string(got) != "original\n" {
		t.Errorf("file was modified despite refusal: %q", got)
	}
}

func TestWriteTool_AfterReadAllowed(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "f.txt"), "v1\n")
	ac := newToolAC(tmp)

	if _, err := (ReadTool{}).Run(context.Background(),
		map[string]any{"path": "f.txt"}, ac); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := (WriteTool{}).Run(context.Background(),
		map[string]any{"path": "f.txt", "content": "v2\n"}, ac); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(tmp, "f.txt"))
	if string(got) != "v2\n" {
		t.Errorf("contents = %q, want v2", got)
	}
}

// TestWriteTool_PreservesExistingFileMode guards M11: overwriting an
// existing 0755 script must keep its permission bits, while a brand-new
// file still gets the 0o644 default (umask-independent via the explicit
// chmod in atomicWriteFile).
func TestWriteTool_PreservesExistingFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	tmp := t.TempDir()
	script := filepath.Join(tmp, "run.sh")
	mustWriteFile(t, script, "#!/bin/sh\necho v1\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	ac := newToolAC(tmp)

	if _, err := (ReadTool{}).Run(context.Background(),
		map[string]any{"path": "run.sh"}, ac); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := (WriteTool{}).Run(context.Background(),
		map[string]any{"path": "run.sh", "content": "#!/bin/sh\necho v2\n"}, ac); err != nil {
		t.Fatalf("write: %v", err)
	}
	fi, err := os.Stat(script)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %o, want 755 preserved", fi.Mode().Perm())
	}

	// New file: default 0o644 regardless of umask.
	if _, err := (WriteTool{}).Run(context.Background(),
		map[string]any{"path": "fresh.txt", "content": "hi"}, ac); err != nil {
		t.Fatalf("write new: %v", err)
	}
	fi, err = os.Stat(filepath.Join(tmp, "fresh.txt"))
	if err != nil {
		t.Fatalf("stat new: %v", err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("new-file mode = %o, want 644", fi.Mode().Perm())
	}
}

func TestWriteTool_CreatesParentDirs(t *testing.T) {
	tmp := t.TempDir()
	ac := newToolAC(tmp)

	if _, err := (WriteTool{}).Run(context.Background(),
		map[string]any{"path": "nested/deep/file.txt", "content": "ok"}, ac); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "nested", "deep", "file.txt")); err != nil {
		t.Errorf("nested file not created: %v", err)
	}
}
