// SPDX-License-Identifier: AGPL-3.0-or-later

package lsp

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/config"
)

func TestManagerMatchByExtension(t *testing.T) {
	cfgs := map[string]config.LSPConfig{
		"go":         {Command: "gopls", Extensions: []string{".go"}},
		"typescript": {Command: "tsserver", Extensions: []string{".ts", ".tsx"}},
	}
	m := NewManager("/tmp/proj", cfgs)
	cases := []struct {
		path string
		want string
	}{
		{"/tmp/proj/main.go", "go"},
		{"/tmp/proj/app.ts", "typescript"},
		{"/tmp/proj/COMP.TSX", "typescript"}, // case-insensitive
		{"/tmp/proj/README.md", ""},
		{"/tmp/proj/Makefile", ""},
	}
	for _, tc := range cases {
		name, _ := m.matchByExtension(tc.path)
		if name != tc.want {
			t.Errorf("matchByExtension(%q) = %q, want %q", tc.path, name, tc.want)
		}
	}
}

func TestFindRoot(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "x.go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got := findRoot(filepath.Join(deep, "x.go"), []string{"go.mod"}, "/fallback")
	if got != root {
		t.Errorf("findRoot found %q, want %q", got, root)
	}

	// No marker → fallback.
	got = findRoot(filepath.Join(deep, "x.go"), []string{"nope.txt"}, "/fallback")
	if got != "/fallback" {
		t.Errorf("findRoot fallback = %q, want /fallback", got)
	}
}

func TestPathToURIRoundtrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("URI normalisation differs on Windows; smoke-test on POSIX")
	}
	cases := []string{
		"/tmp/proj/main.go",
		"/home/user/work/file with spaces.go",
		"/etc/hosts",
	}
	for _, p := range cases {
		uri := pathToURI(p)
		if !strings.HasPrefix(uri, "file://") {
			t.Errorf("URI %q lacks file:// prefix", uri)
		}
		got := URIToPath(uri)
		if got != p {
			t.Errorf("roundtrip: %q → %q → %q", p, uri, got)
		}
	}
}

func TestHasServers(t *testing.T) {
	if (*Manager)(nil).HasServers() {
		t.Errorf("nil manager should report HasServers=false")
	}
	if NewManager("/", map[string]config.LSPConfig{}).HasServers() {
		t.Errorf("empty config should report HasServers=false")
	}
	if !NewManager("/", map[string]config.LSPConfig{"go": {Command: "gopls"}}).HasServers() {
		t.Errorf("non-empty config should report HasServers=true")
	}
}
