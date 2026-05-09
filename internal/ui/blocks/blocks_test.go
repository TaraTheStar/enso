// SPDX-License-Identifier: AGPL-3.0-or-later

package blocks

import (
	"testing"
	"time"
)

func TestTool_RunningWhenStartedAndNoDuration(t *testing.T) {
	tool := &Tool{StartedAt: time.Now()}
	if !tool.Running() {
		t.Errorf("Running()=false, want true (StartedAt set, Duration zero)")
	}
}

func TestTool_NotRunningOnReplayBlock(t *testing.T) {
	// Replayed blocks leave StartedAt zero — they must not look "live"
	// even though Duration is also zero.
	tool := &Tool{Name: "Bash"}
	if tool.Running() {
		t.Errorf("Running()=true on replay block (StartedAt zero)")
	}
}

func TestTool_NotRunningOnceDurationSet(t *testing.T) {
	tool := &Tool{StartedAt: time.Now(), Duration: 2 * time.Second}
	if tool.Running() {
		t.Errorf("Running()=true after Duration set")
	}
}

func TestTool_ElapsedZeroOnReplayBlock(t *testing.T) {
	tool := &Tool{Name: "Bash"}
	if got := tool.Elapsed(); got != 0 {
		t.Errorf("Elapsed()=%v, want 0 on replay block", got)
	}
}

func TestTool_ElapsedReturnsRecordedDuration(t *testing.T) {
	tool := &Tool{
		StartedAt: time.Now().Add(-5 * time.Second),
		Duration:  3 * time.Second,
	}
	if got := tool.Elapsed(); got != 3*time.Second {
		t.Errorf("Elapsed()=%v, want 3s (recorded duration wins over wall-clock)", got)
	}
}

func TestTool_ElapsedComputesLiveTime(t *testing.T) {
	tool := &Tool{StartedAt: time.Now().Add(-100 * time.Millisecond)}
	got := tool.Elapsed()
	if got < 100*time.Millisecond || got > time.Second {
		t.Errorf("Elapsed()=%v, want ~100ms for running block", got)
	}
}

func TestBlockMarkerInterface(t *testing.T) {
	// Sanity check that every block type satisfies the marker interface.
	// If a new block type is added without registering isBlock(), this
	// fails to compile — that's the intent.
	all := []Block{
		&User{},
		&Assistant{},
		&Tool{},
		&Reasoning{},
		&Error{},
		&Cancelled{},
		&InputDiscarded{},
		&Compacted{},
	}
	if len(all) != 8 {
		t.Errorf("expected 8 block types, got %d", len(all))
	}
}
