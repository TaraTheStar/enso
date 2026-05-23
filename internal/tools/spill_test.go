// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFileSpill_WritesAndDedupes confirms a spill round-trip and that
// identical content reuses the same path (the sha256-based name makes
// repeats free).
func TestFileSpill_WritesAndDedupes(t *testing.T) {
	root := t.TempDir()
	fs := &FileSpill{Root: root, SessionID: "sess-1"}

	content := "hello\nworld\n"
	p1, err := fs.Spill(content)
	if err != nil {
		t.Fatalf("first Spill: %v", err)
	}
	if !strings.HasPrefix(p1, filepath.Join(root, "sess-1")) {
		t.Errorf("path not under session subdir: %s", p1)
	}
	got, err := os.ReadFile(p1)
	if err != nil {
		t.Fatalf("read spilled file: %v", err)
	}
	if string(got) != content {
		t.Errorf("spilled content mismatch: got %q, want %q", got, content)
	}

	// Same content → same path; second call must not error or rewrite.
	p2, err := fs.Spill(content)
	if err != nil {
		t.Fatalf("second Spill: %v", err)
	}
	if p2 != p1 {
		t.Errorf("dedup failed: got %q vs %q for identical content", p1, p2)
	}
}

// TestFileSpill_DistinctContentDistinctPath confirms different inputs
// don't collide on the 16-hex truncated hash. Birthday-paradox at 64
// bits is fine for a single session's worth of outputs.
func TestFileSpill_DistinctContentDistinctPath(t *testing.T) {
	root := t.TempDir()
	fs := &FileSpill{Root: root, SessionID: "sess"}
	p1, _ := fs.Spill("alpha")
	p2, _ := fs.Spill("beta")
	if p1 == p2 {
		t.Errorf("distinct content shared a path: %s", p1)
	}
}

// TestFileSpill_Misconfigured surfaces a clear error rather than
// silently writing to cwd or panicking when fields are zero.
func TestFileSpill_Misconfigured(t *testing.T) {
	if _, err := (&FileSpill{}).Spill("anything"); err == nil {
		t.Error("zero-value FileSpill should error")
	}
	if _, err := (&FileSpill{Root: "/x"}).Spill("anything"); err == nil {
		t.Error("missing SessionID should error")
	}
}

// TestSweepSpills_RemovesExpired confirms TTL cleanup: a file with an
// mtime older than maxAge gets removed; a fresh one survives. The
// empty per-session subdir is also tidied so accumulation is bounded.
func TestSweepSpills_RemovesExpired(t *testing.T) {
	root := t.TempDir()
	// One stale session: 1 expired file.
	stale := filepath.Join(root, "old-session")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	staleFile := filepath.Join(stale, "abc.txt")
	if err := os.WriteFile(staleFile, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(staleFile, old, old); err != nil {
		t.Fatal(err)
	}
	// One live session: 1 fresh file.
	live := filepath.Join(root, "new-session")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	freshFile := filepath.Join(live, "def.txt")
	if err := os.WriteFile(freshFile, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := SweepSpills(root, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("SweepSpills: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed count: got %d, want 1", removed)
	}
	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Errorf("stale file not removed (err=%v)", err)
	}
	if _, err := os.Stat(freshFile); err != nil {
		t.Errorf("fresh file unexpectedly removed: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("empty stale subdir not tidied (err=%v)", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("live subdir unexpectedly removed: %v", err)
	}
}

// TestSweepSpills_MissingRootNoError covers the cold-start case: a
// host that's never spilled has no truncated dir, and the sweep must
// no-op without surfacing an error that would scare agent startup.
func TestSweepSpills_MissingRootNoError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")
	removed, err := SweepSpills(root, time.Hour)
	if err != nil {
		t.Errorf("missing root should not error: %v", err)
	}
	if removed != 0 {
		t.Errorf("missing root removed count: got %d, want 0", removed)
	}
}

// TestTruncateWithRecovery_SpillsAndAppendsFooter exercises the full
// path used by the 5 tool callers: oversize content triggers spill
// AND the LLMOutput gets the recovery footer with the path embedded.
func TestTruncateWithRecovery_SpillsAndAppendsFooter(t *testing.T) {
	root := t.TempDir()
	ac := &AgentContext{
		OutputCaps: DefaultOutputCaps{
			Default:  500,
			MaxBytes: 50 * 1024,
		},
		Spill: &FileSpill{Root: root, SessionID: "sess"},
	}
	// 3000 short lines → trips line cap (500), well under byte cap.
	lines := make([]string, 3000)
	for i := range lines {
		lines[i] = "filler line"
	}
	input := strings.Join(lines, "\n")

	model, full := truncateWithRecovery(ac, "bash", input)

	if full != input {
		t.Errorf("FullOutput must be untouched")
	}
	if model == full {
		t.Errorf("LLMOutput should be truncated, not equal to FullOutput")
	}
	if !strings.Contains(model, "[full output:") {
		t.Errorf("recovery footer missing in LLMOutput:\n%s", model[len(model)-300:])
	}
	if !strings.Contains(model, "use `read`") {
		t.Errorf("footer should mention `read` recovery hint")
	}
	// The footer path must point at a file that actually exists.
	idx := strings.Index(model, "[full output: ")
	if idx < 0 {
		t.Fatal("footer prefix not found")
	}
	rest := model[idx+len("[full output: "):]
	end := strings.Index(rest, " ")
	if end < 0 {
		t.Fatalf("could not extract path from footer: %q", model[idx:])
	}
	path := rest[:end]
	if _, err := os.Stat(path); err != nil {
		t.Errorf("spill file at footer path missing: %v (path=%s)", err, path)
	}
}

// TestTruncateWithRecovery_NoSpillNoFooter covers the degradation
// path: when ac.Spill is nil, output is truncated but the footer
// must NOT appear (otherwise the model would be pointed at a
// nonexistent file).
func TestTruncateWithRecovery_NoSpillNoFooter(t *testing.T) {
	ac := &AgentContext{
		OutputCaps: DefaultOutputCaps{Default: 100},
		// Spill: nil
	}
	input := strings.Repeat("line\n", 500)
	model, full := truncateWithRecovery(ac, "bash", input)
	if model == full {
		t.Fatal("expected truncation")
	}
	if strings.Contains(model, "[full output:") {
		t.Error("footer should NOT appear without a spill writer")
	}
}

// TestTruncateWithRecovery_NoTruncationNoFooter pins the non-trip
// path: small inputs return untouched and no footer is appended.
func TestTruncateWithRecovery_NoTruncationNoFooter(t *testing.T) {
	root := t.TempDir()
	ac := &AgentContext{
		OutputCaps: DefaultOutputCaps{Default: 2000},
		Spill:      &FileSpill{Root: root, SessionID: "sess"},
	}
	model, full := truncateWithRecovery(ac, "bash", "tiny output")
	if model != "tiny output" || full != "tiny output" {
		t.Errorf("small input mutated: model=%q full=%q", model, full)
	}
	// And no spill file was written.
	dir := filepath.Join(root, "sess")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("spill dir created for sub-cap input (err=%v)", err)
	}
}
