// SPDX-License-Identifier: AGPL-3.0-or-later

package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/config"
)

// TestGoplsHoverIntegration is an opt-in smoke test that drives a real
// gopls subprocess. Skipped when gopls isn't on PATH; when enabled, it
// asserts that a hover request on a known symbol returns non-empty
// content. This is the test that proves the framing + handshake survive
// contact with a real LSP server.
func TestGoplsHoverIntegration(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH; skipping LSP integration test")
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := `package main

import "fmt"

func main() {
	fmt.Println("hi")
}
`
	srcPath := filepath.Join(root, "main.go")
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(root, map[string]config.LSPConfig{
		"go": {
			Command:     "gopls",
			Extensions:  []string{".go"},
			RootMarkers: []string{"go.mod"},
		},
	})
	defer mgr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, _, err := mgr.ClientFor(ctx, srcPath)
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if err := client.DidOpen(pathToURI(srcPath), "go", src); err != nil {
		t.Fatalf("DidOpen: %v", err)
	}

	// Hover over `Println` on line 6 (1-based) col 6 (start of "Println"). The
	// converter in the tools layer subtracts 1; this test calls the client
	// directly, so use 0-based: line=5 ("\tfmt.Println(...)"), char ~6.
	hover, err := client.Hover(ctx, pathToURI(srcPath), 5, 6)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if !strings.Contains(strings.ToLower(hover), "println") {
		t.Errorf("expected hover to mention Println, got: %q", hover)
	}
}
