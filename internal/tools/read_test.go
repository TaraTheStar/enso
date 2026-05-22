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

func TestReadTool_DisplayOutput_FullFile(t *testing.T) {
	tmp := t.TempDir()
	// Trailing newline → strings.Split yields 4 elements ("a","b","c","").
	mustWriteFile(t, filepath.Join(tmp, "f.txt"), "a\nb\nc\n")
	ac := newToolAC(tmp)
	res, _ := ReadTool{}.Run(context.Background(), map[string]any{"path": "f.txt"}, ac)
	if res.DisplayOutput != "4 lines" {
		t.Errorf("display = %q, want `4 lines`", res.DisplayOutput)
	}
}

func TestReadTool_DisplayOutput_SingleLineFile(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "f.txt"), "only")
	ac := newToolAC(tmp)
	res, _ := ReadTool{}.Run(context.Background(), map[string]any{"path": "f.txt"}, ac)
	// Singular form for a one-line file.
	if res.DisplayOutput != "1 line" {
		t.Errorf("display = %q, want `1 line`", res.DisplayOutput)
	}
}

func TestReadTool_DisplayOutput_LineRange(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "f.txt"), "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n")
	ac := newToolAC(tmp)
	res, _ := ReadTool{}.Run(context.Background(), map[string]any{
		"path":       "f.txt",
		"first_line": float64(3),
		"last_line":  float64(7),
	}, ac)
	// File has 11 lines (10 + trailing-newline empty); range form shows
	// "lines A-B (of TOTAL)".
	if !strings.Contains(res.DisplayOutput, "lines 3-7") {
		t.Errorf("display = %q, want `lines 3-7 …`", res.DisplayOutput)
	}
	if !strings.Contains(res.DisplayOutput, "(of 11)") {
		t.Errorf("display = %q, want `(of 11)`", res.DisplayOutput)
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

// TestReadTool_PNGEmitsImagePart confirms the image short-circuit:
// reading a recognised image file returns a Result with a single
// image MessagePart instead of binary-as-numbered-text. The textual
// LLMOutput becomes a one-line summary the model can still read on
// adapters that don't support images yet.
func TestReadTool_PNGEmitsImagePart(t *testing.T) {
	tmp := t.TempDir()
	// 8-byte PNG magic (89 50 4E 47 0D 0A 1A 0A) — enough for
	// http.DetectContentType to recognise the format if extension
	// detection somehow missed.
	pngMagic := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	mustWriteFile(t, filepath.Join(tmp, "tiny.png"), string(pngMagic))

	ac := newToolAC(tmp)
	res, err := ReadTool{}.Run(context.Background(),
		map[string]any{"path": "tiny.png"}, ac)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Parts) != 1 {
		t.Fatalf("Parts: want 1, got %d", len(res.Parts))
	}
	if res.Parts[0].Type != "image" || res.Parts[0].MIMEType != "image/png" {
		t.Fatalf("part: type=%q mime=%q", res.Parts[0].Type, res.Parts[0].MIMEType)
	}
	if len(res.Parts[0].Data) != len(pngMagic) {
		t.Fatalf("Data len=%d, want %d", len(res.Parts[0].Data), len(pngMagic))
	}
	// Textual summary still set — adapters that don't speak images
	// (or persistence) get a useful fallback.
	if !strings.Contains(res.LLMOutput, "[image:") {
		t.Fatalf("LLMOutput summary missing: %q", res.LLMOutput)
	}
	if !strings.Contains(res.DisplayOutput, "image,") {
		t.Fatalf("DisplayOutput missing image prefix: %q", res.DisplayOutput)
	}
}

// TestReadTool_NonImageBinaryFallsThrough verifies that a .bin file
// the sniffer can't classify routes through the normal text-read
// path (no Parts). Better to show the user the symptom (garbled
// text) than pretend we have a multimodal answer.
func TestReadTool_NonImageBinaryFallsThrough(t *testing.T) {
	tmp := t.TempDir()
	mustWriteFile(t, filepath.Join(tmp, "blob.bin"), "\x00\x01\x02arbitrary")

	ac := newToolAC(tmp)
	res, err := ReadTool{}.Run(context.Background(),
		map[string]any{"path": "blob.bin"}, ac)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Parts) != 0 {
		t.Fatalf("non-image must not produce Parts: %+v", res.Parts)
	}
}

// TestImageMIME_ExtensionAndSniff covers both detection paths.
// Extension is authoritative (user named it); sniff catches files
// renamed to ".dat" but still PNG.
func TestImageMIME_ExtensionAndSniff(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		data     []byte
		wantMime string
		wantOK   bool
	}{
		{"png by extension", "x.png", []byte("anything"), "image/png", true},
		{"jpeg by extension", "x.JPG", []byte("anything"), "image/jpeg", true},
		{"png by sniff", "x.dat", []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, "image/png", true},
		{"unknown extension+content", "x.dat", []byte("hello"), "", false},
		{"heic (intentionally unsupported)", "x.heic", []byte("fakeheic"), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := imageMIME(tc.path, tc.data)
			if ok != tc.wantOK || got != tc.wantMime {
				t.Fatalf("got=%q ok=%v, want %q %v", got, ok, tc.wantMime, tc.wantOK)
			}
		})
	}
}
