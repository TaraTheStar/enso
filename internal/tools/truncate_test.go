// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"strings"
	"testing"
)

// TestCapTruncate_ByteCapFiresOnOneLineGiant pins the acceptance case
// from the plan: a single-line 100 KB blob (think a minified JS dump
// from a bash pipeline) hits the byte cap, not the line cap. Without
// the byte pass this would slip through — one line is well under any
// reasonable maxLines budget — and the model would see the full blob.
func TestCapTruncate_ByteCapFiresOnOneLineGiant(t *testing.T) {
	input := strings.Repeat("x", 100*1024) // 100 KB, one line
	truncated, full := capTruncate(input, 2000, 50*1024, 2000, "")

	if len(truncated) >= len(input) {
		t.Fatalf("byte cap did not fire: truncated %d bytes, input %d", len(truncated), len(input))
	}
	if !strings.Contains(truncated, "bytes truncated") {
		t.Errorf("byte cap banner missing in output: %q", truncated[:min(200, len(truncated))])
	}
	if full != input {
		t.Errorf("full output should be untouched (len=%d, want %d)", len(full), len(input))
	}
}

// TestCapTruncate_LineCapStillFiresUnderByteCap confirms a many-line
// but small-byte output still trips the line cap (the legacy path).
func TestCapTruncate_LineCapStillFiresUnderByteCap(t *testing.T) {
	// 5000 short lines, well under any byte cap.
	lines := make([]string, 5000)
	for i := range lines {
		lines[i] = "line"
	}
	input := strings.Join(lines, "\n")
	truncated, _ := capTruncate(input, 200, 50*1024, 2000, "")

	got := strings.Count(truncated, "\n")
	if got >= 5000 {
		t.Fatalf("line cap did not fire: got %d newlines, want <5000", got)
	}
	if !strings.Contains(truncated, "lines truncated") {
		t.Errorf("line-cap banner missing in output")
	}
}

// TestCapTruncate_PerLineCapElidesMiddle confirms a line longer than
// maxLineLen gets its middle elided while shorter lines are untouched.
func TestCapTruncate_PerLineCapElidesMiddle(t *testing.T) {
	short := "ok"
	long := strings.Repeat("Z", 6000) // 6 KB on one line
	input := short + "\n" + long + "\n" + short

	truncated, _ := capTruncate(input, 2000, 0 /*disable byte cap*/, 1000, "")

	got := strings.Split(truncated, "\n")
	if len(got) != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", len(got), truncated)
	}
	if got[0] != short || got[2] != short {
		t.Errorf("short lines mutated: %q / %q", got[0], got[2])
	}
	if len(got[1]) >= len(long) {
		t.Errorf("long line not shortened: %d bytes (was %d)", len(got[1]), len(long))
	}
	if !strings.Contains(got[1], "bytes elided") {
		t.Errorf("per-line elision banner missing: %q", got[1][:min(200, len(got[1]))])
	}
}

// TestCapTruncate_ZeroDisablesEachCap verifies that zero (or negative)
// values disable that pass — important so callers can disable
// individual caps without having to know an "infinity" sentinel.
func TestCapTruncate_ZeroDisablesEachCap(t *testing.T) {
	input := strings.Repeat("x", 10*1024) // 10 KB
	// All caps zero (line cap falls through to defaultLineCap=2000,
	// but with only 1 line it's a no-op anyway).
	truncated, full := capTruncate(input, 0, 0, 0, "")
	if truncated != input || full != input {
		t.Errorf("zero caps should pass input through unchanged")
	}
}

// TestCapTruncate_AllThreeCompose feeds an input that trips all three
// caps and asserts the order: byte cap → line cap → per-line cap.
// The byte cap should fire first (output ≤ maxBytes), then per-line
// trimming may further shorten any surviving long line.
func TestCapTruncate_AllThreeCompose(t *testing.T) {
	// 2 lines: a 200 KB single line, then 5000 short lines.
	huge := strings.Repeat("A", 200*1024)
	tail := strings.Repeat("ok\n", 5000)
	input := huge + "\n" + tail

	truncated, _ := capTruncate(input, 1000, 50*1024, 1000, "")

	if len(truncated) > 60*1024 {
		t.Errorf("byte cap not honoured: result %d bytes, want ≤ ~50 KB", len(truncated))
	}
	for _, ln := range strings.Split(truncated, "\n") {
		if len(ln) > 1500 { // 1000 + banner slack
			t.Errorf("per-line cap not honoured: line %d bytes", len(ln))
		}
	}
}

// TestHeadTailBytes_PreferNewlineBoundary verifies the split tries to
// land on a newline boundary near the budget midpoint, so the elision
// banner doesn't crash into the middle of a line.
func TestHeadTailBytes_PreferNewlineBoundary(t *testing.T) {
	// Build an input with many short lines so newlines are dense in
	// the boundary region.
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		b.WriteString("short line of text\n")
	}
	input := b.String()
	out := HeadTailBytes(input, 1024)
	// The banner should appear surrounded by complete lines.
	idx := strings.Index(out, "bytes truncated")
	if idx < 0 {
		t.Fatalf("banner missing: %q", out[:min(200, len(out))])
	}
	// Character immediately before the banner block should be a
	// newline (the head ended cleanly on \n).
	bannerStart := strings.LastIndex(out[:idx], "\n... ")
	if bannerStart < 0 {
		t.Fatalf("could not locate banner prefix")
	}
	if bannerStart > 0 && out[bannerStart-1] != '\n' {
		t.Errorf("banner did not land on newline boundary: %q",
			out[max(0, bannerStart-20):min(len(out), bannerStart+40)])
	}
}

// TestDefaultOutputCaps_LookupFallback exercises the BytesFor /
// LineLengthFor helpers' fallback chain: per-tool override → global
// → in-tree default.
func TestDefaultOutputCaps_LookupFallback(t *testing.T) {
	caps := DefaultOutputCaps{
		MaxBytes:          70 * 1024,
		PerToolBytes:      map[string]int{"bash": 10 * 1024},
		MaxLineLength:     1500,
		PerToolLineLength: map[string]int{"read": 4000},
	}
	if got := caps.BytesFor("bash"); got != 10*1024 {
		t.Errorf("bash BytesFor: got %d, want 10240 (per-tool override)", got)
	}
	if got := caps.BytesFor("grep"); got != 70*1024 {
		t.Errorf("grep BytesFor: got %d, want 71680 (global)", got)
	}
	if got := (DefaultOutputCaps{}).BytesFor("anything"); got != DefaultMaxBytes {
		t.Errorf("zero-value BytesFor: got %d, want %d (default)", got, DefaultMaxBytes)
	}
	if got := caps.LineLengthFor("read"); got != 4000 {
		t.Errorf("read LineLengthFor: got %d, want 4000 (per-tool override)", got)
	}
	if got := caps.LineLengthFor("bash"); got != 1500 {
		t.Errorf("bash LineLengthFor: got %d, want 1500 (global)", got)
	}
	if got := (DefaultOutputCaps{}).LineLengthFor("anything"); got != DefaultMaxLineLength {
		t.Errorf("zero-value LineLengthFor: got %d, want %d (default)",
			got, DefaultMaxLineLength)
	}
}

// TestPriorityTruncate_KeepsErrorsOverNoise is the H6 check: with no hint,
// truncation past the line cap should preserve the error line and drop the
// surrounding INFO/debug noise.
func TestPriorityTruncate_KeepsErrorsOverNoise(t *testing.T) {
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, "INFO routine log line some detail here")
	}
	lines = append(lines, "ERROR database connection refused")
	for i := 0; i < 100; i++ {
		lines = append(lines, "DEBUG verbose trace chatter")
	}
	input := strings.Join(lines, "\n")

	truncated, _ := capTruncate(input, 20, 0, 0, "")
	if !strings.Contains(truncated, "ERROR database connection refused") {
		t.Fatalf("priority truncation dropped the error line:\n%s", truncated)
	}
}

func TestLinePriority(t *testing.T) {
	cases := map[string]int{
		"ERROR something exploded":     3,
		"panic: nil deref":             3,
		"security vulnerability found": 3,
		"WARNING deprecated API":       2,
		"DEBUG handler entered":        -1,
		"just a normal line of output": 0,
	}
	for line, want := range cases {
		if got := linePriority(line); got != want {
			t.Errorf("linePriority(%q) = %d, want %d", line, got, want)
		}
	}
}
