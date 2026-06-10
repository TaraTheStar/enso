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

// fakeResolver is a minimal InstructionResolver stand-in for read-tool
// tests. Records every absPath it was asked about and replies with a
// canned reminder the first time per path; subsequent reads of the
// same path return "" to mimic the real per-session dedup.
type fakeResolver struct {
	mu      sync.Mutex
	seen    map[string]bool
	calls   []string
	respond func(absPath string) string
}

func newFakeResolver(respond func(string) string) *fakeResolver {
	return &fakeResolver{
		seen:    map[string]bool{},
		respond: respond,
	}
}

func (f *fakeResolver) ResolveOnRead(absPath string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, absPath)
	if f.seen[absPath] {
		return ""
	}
	f.seen[absPath] = true
	return f.respond(absPath)
}

// TestReadTool_AppendsInstructionReminder verifies the post-read hook:
// when InstructionResolver returns non-empty text, it's appended to
// LLMOutput. FullOutput stays untouched (instructions are for the
// model, not the persisted record).
func TestReadTool_AppendsInstructionReminder(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "main.go")
	if err := writeTmpFile(target, "package main\nfunc main() {}\n"); err != nil {
		t.Fatal(err)
	}

	resolver := newFakeResolver(func(p string) string {
		return "<system-reminder>\nrules for " + p + "\n</system-reminder>"
	})
	ac := &AgentContext{
		Cwd:                 tmp,
		ReadSet:             map[string]bool{},
		InstructionResolver: resolver,
		OutputCaps:          DefaultOutputCaps{Default: 2000},
	}

	res, err := (ReadTool{}).Run(context.Background(), map[string]any{"path": target}, ac)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "<system-reminder>") {
		t.Errorf("LLMOutput missing reminder block:\n%s", res.LLMOutput)
	}
	if strings.Contains(res.FullOutput, "<system-reminder>") {
		t.Errorf("FullOutput must not include the reminder (it's not real file content)")
	}
}

// TestReadTool_NoResolverNoReminder confirms the read tool is a no-op
// w.r.t. instruction injection when ac.InstructionResolver is nil
// (tests, sub-agents without a cwd, etc.).
func TestReadTool_NoResolverNoReminder(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "x.go")
	if err := writeTmpFile(target, "// hi\n"); err != nil {
		t.Fatal(err)
	}
	ac := &AgentContext{
		Cwd:        tmp,
		ReadSet:    map[string]bool{},
		OutputCaps: DefaultOutputCaps{Default: 2000},
	}
	res, err := (ReadTool{}).Run(context.Background(), map[string]any{"path": target}, ac)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.LLMOutput, "<system-reminder>") {
		t.Errorf("reminder appeared without a resolver: %s", res.LLMOutput)
	}
}

// TestReadTool_ResolverDedupsAcrossReads pins the per-session "already
// injected" contract: the resolver may opt to return "" on a repeat
// read of the same file. The read tool must surface that as a clean
// LLMOutput (no trailing blank lines, no stray <system-reminder>).
func TestReadTool_ResolverDedupsAcrossReads(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "y.go")
	if err := writeTmpFile(target, "// hi\n"); err != nil {
		t.Fatal(err)
	}
	resolver := newFakeResolver(func(p string) string {
		return "<system-reminder>\nfirst time only\n</system-reminder>"
	})
	ac := &AgentContext{
		Cwd:                 tmp,
		ReadSet:             map[string]bool{},
		InstructionResolver: resolver,
		OutputCaps:          DefaultOutputCaps{Default: 2000},
	}

	r1, err := (ReadTool{}).Run(context.Background(), map[string]any{"path": target}, ac)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r1.LLMOutput, "first time only") {
		t.Errorf("first read missing reminder:\n%s", r1.LLMOutput)
	}

	r2, err := (ReadTool{}).Run(context.Background(), map[string]any{"path": target}, ac)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r2.LLMOutput, "first time only") {
		t.Errorf("second read re-emitted reminder (dedup broken):\n%s", r2.LLMOutput)
	}
}

func writeTmpFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
