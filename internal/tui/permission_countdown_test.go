// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"strings"
	"testing"
	"time"
)

func TestFmtCountdown_DimAboveThirty(t *testing.T) {
	got := fmtCountdown(45 * time.Second)
	if !strings.Contains(got, "[gray]") {
		t.Errorf("expected gray dim color above 30s, got %q", got)
	}
	if !strings.Contains(got, "auto-deny in 45s") {
		t.Errorf("expected '45s' label, got %q", got)
	}
}

func TestFmtCountdown_YellowAtThirty(t *testing.T) {
	got := fmtCountdown(25 * time.Second)
	if !strings.Contains(got, "[yellow]") {
		t.Errorf("expected yellow at 25s, got %q", got)
	}
}

func TestFmtCountdown_RedUnderTen(t *testing.T) {
	got := fmtCountdown(7 * time.Second)
	if !strings.Contains(got, "[red]") {
		t.Errorf("expected red at 7s, got %q", got)
	}
}

func TestFmtCountdown_FloorsToOneSecond(t *testing.T) {
	// Sub-second remaining (e.g., 400ms) should not display "0s" — the
	// expiry tick fires almost immediately after, but the user
	// shouldn't see a spurious "0s" frame and assume we hung.
	got := fmtCountdown(400 * time.Millisecond)
	if !strings.Contains(got, "1s") {
		t.Errorf("sub-second remaining should floor to 1s, got %q", got)
	}
	if strings.Contains(got, "0s") {
		t.Errorf("must not display 0s while time remains: %q", got)
	}
}

func TestFmtCountdown_BoundaryThirtyExactly(t *testing.T) {
	// At exactly 30s the threshold goes yellow, not gray. Documents
	// the inclusive boundary so a future tweak doesn't accidentally
	// flip the rule.
	got := fmtCountdown(30 * time.Second)
	if !strings.Contains(got, "[yellow]") {
		t.Errorf("30s should be yellow, got %q", got)
	}
}

func TestFmtCountdown_BoundaryTenExactly(t *testing.T) {
	got := fmtCountdown(10 * time.Second)
	if !strings.Contains(got, "[red]") {
		t.Errorf("10s should be red, got %q", got)
	}
}
