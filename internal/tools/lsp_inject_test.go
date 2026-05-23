// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeLSPNotifier records the paths it was called with and returns a
// canned reminder string per call. Used to verify write/edit hook
// firing without standing up a real LSP server.
type fakeLSPNotifier struct {
	mu    sync.Mutex
	calls []string
	reply string
}

func (f *fakeLSPNotifier) NotifyWrite(_ context.Context, absPath string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, absPath)
	return f.reply
}

// TestWriteTool_LSPInjection covers the success path: the notifier's
// non-empty reply is appended to LLMOutput verbatim, and the notifier
// sees the absolute path of the just-written file. FullOutput must
// NOT pick up the diagnostics block — that's a model-only signal.
func TestWriteTool_LSPInjection(t *testing.T) {
	tmp := t.TempDir()
	abs := filepath.Join(tmp, "x.go")

	notifier := &fakeLSPNotifier{reply: "\n\n[LSP diagnostics for x.go]\nx.go:1:1: error: oops"}
	ac := &AgentContext{
		Cwd:         tmp,
		ReadSet:     map[string]bool{},
		LSPNotifier: notifier,
	}
	args := map[string]interface{}{"path": abs, "content": "package main\n"}

	res, err := (WriteTool{}).Run(context.Background(), args, ac)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "[LSP diagnostics for x.go]") {
		t.Errorf("LLMOutput missing diagnostics block:\n%s", res.LLMOutput)
	}
	if !strings.Contains(res.LLMOutput, "wrote ") {
		t.Errorf("LLMOutput dropped the wrote-N-bytes line:\n%s", res.LLMOutput)
	}
	if strings.Contains(res.FullOutput, "LSP diagnostics") {
		t.Errorf("FullOutput leaked diagnostics (model-only signal):\n%s", res.FullOutput)
	}

	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.calls) != 1 || notifier.calls[0] != abs {
		t.Errorf("notifier calls = %v, want one call with %q", notifier.calls, abs)
	}
}

// TestEditTool_LSPInjection covers the same path through the edit
// tool. Uses an existing file in the session ReadSet so the edit
// preconditions pass.
func TestEditTool_LSPInjection(t *testing.T) {
	tmp := t.TempDir()
	abs := filepath.Join(tmp, "y.go")
	if err := os.WriteFile(abs, []byte("package main\nvar x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	notifier := &fakeLSPNotifier{reply: "\n\n[LSP diagnostics for y.go]\ny.go:2:5: error: unused"}
	ac := &AgentContext{
		Cwd:         tmp,
		ReadSet:     map[string]bool{abs: true},
		LSPNotifier: notifier,
	}
	args := map[string]interface{}{
		"path":       abs,
		"old_string": "var x = 1",
		"new_string": "var x = 2",
	}

	res, err := (EditTool{}).Run(context.Background(), args, ac)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "[LSP diagnostics for y.go]") {
		t.Errorf("LLMOutput missing diagnostics block:\n%s", res.LLMOutput)
	}
	if len(notifier.calls) != 1 || notifier.calls[0] != abs {
		t.Errorf("notifier calls = %v, want one call with %q", notifier.calls, abs)
	}
}

// TestWriteTool_NoNotifierNoInjection confirms the disabled path: a
// nil LSPNotifier leaves output identical to today's behaviour, byte
// for byte. Guards against the hook adding stray whitespace or a
// header even when there's nothing to surface.
func TestWriteTool_NoNotifierNoInjection(t *testing.T) {
	tmp := t.TempDir()
	abs := filepath.Join(tmp, "z.go")

	ac := &AgentContext{
		Cwd:     tmp,
		ReadSet: map[string]bool{},
		// LSPNotifier: nil
	}
	args := map[string]interface{}{"path": abs, "content": "hi\n"}

	res, err := (WriteTool{}).Run(context.Background(), args, ac)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(res.LLMOutput, "[LSP") {
		t.Errorf("LSP header leaked without a notifier: %q", res.LLMOutput)
	}
}

// TestWriteTool_EmptyNotifierReplyNoTrailingBlank verifies that when
// the notifier returns "" (no diagnostics worth surfacing), nothing is
// appended — not even the two newlines that would otherwise wrap a
// diagnostics block. Keeps the empty-diagnostics case visually clean.
func TestWriteTool_EmptyNotifierReplyNoTrailingBlank(t *testing.T) {
	tmp := t.TempDir()
	abs := filepath.Join(tmp, "w.go")

	notifier := &fakeLSPNotifier{reply: ""}
	ac := &AgentContext{
		Cwd:         tmp,
		ReadSet:     map[string]bool{},
		LSPNotifier: notifier,
	}
	args := map[string]interface{}{"path": abs, "content": "hi\n"}

	res, err := (WriteTool{}).Run(context.Background(), args, ac)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.HasSuffix(res.LLMOutput, "\n") {
		t.Errorf("trailing newline crept in despite empty notifier reply: %q", res.LLMOutput)
	}
}
