// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
)

// TestBashTool_ForegroundTimeout asserts that a command exceeding its
// timeout is killed and reported via a NORMAL result (not an error) so the
// turn keeps going. A short per-call `timeout` keeps the test fast.
func TestBashTool_ForegroundTimeout(t *testing.T) {
	ac := &AgentContext{Cwd: t.TempDir(), Bus: bus.New()}

	start := time.Now()
	res, err := BashTool{}.Run(
		context.Background(),
		map[string]any{"cmd": "echo before; sleep 30", "timeout": 1},
		ac,
	)
	if err != nil {
		t.Fatalf("timeout must surface as a normal result, got error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("command was not killed promptly: ran %s", elapsed)
	}
	if !strings.Contains(res.LLMOutput, "timed out") {
		t.Errorf("LLMOutput should mention the timeout, got %q", res.LLMOutput)
	}
	if !strings.Contains(res.LLMOutput, "run_in_background") {
		t.Errorf("LLMOutput should hint at run_in_background, got %q", res.LLMOutput)
	}
	if !strings.Contains(res.LLMOutput, "before") {
		t.Errorf("LLMOutput should include partial output captured before the kill, got %q", res.LLMOutput)
	}
}

// TestBashTool_UserInterruptNotTimeout verifies a cancelled parent context
// (user Ctrl-C) is returned as ctx.Err()-style failure, not mislabelled as
// our timeout.
func TestBashTool_UserInterruptNotTimeout(t *testing.T) {
	ac := &AgentContext{Cwd: t.TempDir(), Bus: bus.New()}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()

	res, _ := runBashHost(ctx, "sleep 30", 30*time.Second, ac)
	if strings.Contains(res.LLMOutput, "timed out") {
		t.Errorf("user interrupt must not be reported as a timeout, got %q", res.LLMOutput)
	}
}

// TestBashJobs_Lifecycle exercises run_in_background → bash_output →
// bash_kill, plus KillAll on teardown.
func TestBashJobs_Lifecycle(t *testing.T) {
	ac := &AgentContext{Cwd: t.TempDir(), Bus: bus.New(), BashJobs: NewBashJobs()}

	// Launch a job that emits a line then sleeps so it stays running.
	res, err := BashTool{}.Run(
		context.Background(),
		map[string]any{"cmd": "echo started; sleep 30", "run_in_background": true},
		ac,
	)
	if err != nil {
		t.Fatalf("background launch: %v", err)
	}
	id := parseJobID(t, res.LLMOutput)

	// Poll bash_output until the first line shows up.
	var out Result
	deadline := time.After(3 * time.Second)
	for {
		out, err = BashOutputTool{}.Run(context.Background(), map[string]any{"id": id}, ac)
		if err != nil {
			t.Fatalf("bash_output: %v", err)
		}
		if strings.Contains(out.LLMOutput, "started") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("background job output never appeared, last: %q", out.LLMOutput)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !strings.Contains(out.LLMOutput, "running") {
		t.Errorf("status should be running, got %q", out.LLMOutput)
	}

	// A second read returns no NEW output (cursor advanced).
	again, _ := BashOutputTool{}.Run(context.Background(), map[string]any{"id": id}, ac)
	if !strings.Contains(again.LLMOutput, "no new output") {
		t.Errorf("second read should report no new output, got %q", again.LLMOutput)
	}

	// Kill it.
	killed, err := BashKillTool{}.Run(context.Background(), map[string]any{"id": id}, ac)
	if err != nil {
		t.Fatalf("bash_kill: %v", err)
	}
	if !strings.Contains(killed.LLMOutput, "killed") {
		t.Errorf("kill result = %q", killed.LLMOutput)
	}

	// KillAll is safe to call afterwards (and on a nil registry).
	ac.BashJobs.KillAll()
	var nilJobs *BashJobs
	nilJobs.KillAll()
}

// TestBashJobs_UnknownID confirms output/kill on a missing id stay
// non-error so the model can recover.
func TestBashJobs_UnknownID(t *testing.T) {
	ac := &AgentContext{Cwd: t.TempDir(), Bus: bus.New(), BashJobs: NewBashJobs()}
	out, err := BashOutputTool{}.Run(context.Background(), map[string]any{"id": "bg_999"}, ac)
	if err != nil {
		t.Fatalf("unknown id should not error: %v", err)
	}
	if !strings.Contains(out.LLMOutput, "no background job") {
		t.Errorf("got %q", out.LLMOutput)
	}
}

// TestBashTool_BackgroundUnavailable covers the nil-registry guard.
func TestBashTool_BackgroundUnavailable(t *testing.T) {
	ac := &AgentContext{Cwd: t.TempDir(), Bus: bus.New()} // BashJobs nil
	res, err := BashTool{}.Run(
		context.Background(),
		map[string]any{"cmd": "echo hi", "run_in_background": true},
		ac,
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "not available") {
		t.Errorf("got %q", res.LLMOutput)
	}
}

// parseJobID pulls the "bg_N" id out of the launch result text.
func parseJobID(t *testing.T, s string) string {
	t.Helper()
	i := strings.Index(s, "bg_")
	if i < 0 {
		t.Fatalf("no job id in %q", s)
	}
	id := s[i:]
	if j := strings.IndexAny(id, " \n"); j >= 0 {
		id = id[:j]
	}
	return id
}

func TestToolTimeouts_EffectiveBash(t *testing.T) {
	cases := []struct {
		name      string
		tt        ToolTimeouts
		requested int
		want      time.Duration
	}{
		{"zero value uses default", ToolTimeouts{}, 0, 120 * time.Second},
		{"configured default", ToolTimeouts{BashDefault: 45 * time.Second}, 0, 45 * time.Second},
		{"disabled default", ToolTimeouts{BashDefault: -1}, 0, 0},
		{"override honoured", ToolTimeouts{}, 30, 30 * time.Second},
		{"slow-but-finite honoured under the ceiling", ToolTimeouts{}, 1500, 1500 * time.Second}, // 25min < 1h
		{"absurd explicit clamped to default 1h ceiling", ToolTimeouts{}, 9999999, time.Hour},
		{"explicit clamped to configured ceiling", ToolTimeouts{BashMax: 90 * time.Second}, 9999, 90 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tt.EffectiveBash(tc.requested); got != tc.want {
				t.Errorf("EffectiveBash(%d) = %s, want %s", tc.requested, got, tc.want)
			}
		})
	}
}
