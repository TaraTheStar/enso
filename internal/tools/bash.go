// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/TaraTheStar/enso/internal/bus"
)

// BashTool executes a shell command and captures output.
type BashTool struct{}

func (t BashTool) Name() string { return "bash" }
func (t BashTool) Description() string {
	return "Execute a shell command. Args: cmd (string). Output is truncated for LLM but stored fully in session."
}
func (t BashTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"cmd": map[string]interface{}{"type": "string", "description": "Shell command to execute"},
		},
		"required": []string{"cmd"},
	}
}

func (t BashTool) Run(ctx context.Context, args map[string]interface{}, ac *AgentContext) (Result, error) {
	cmdStr, _ := args["cmd"].(string)
	if cmdStr == "" {
		return Result{}, fmt.Errorf("bash: cmd required")
	}

	if ac.Sandbox != nil {
		return runBashSandboxed(ctx, cmdStr, ac)
	}
	return runBashHost(ctx, cmdStr, ac)
}

// runBashHost is the original direct-exec path: `sh -c <cmd>` with cwd
// set to the project root. Used when no sandbox is configured.
func runBashHost(ctx context.Context, cmdStr string, ac *AgentContext) (Result, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Dir = ac.Cwd
	// Put the shell into its own process group so cancel kills the
	// whole pipeline, not just `sh` (children like `long_thing | foo`
	// would otherwise survive as orphans).
	setProcessGroup(cmd)
	cmd.Cancel = func() error {
		return killProcessGroup(cmd.Process)
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = newProgressWriter(&stdoutBuf, ac.Bus, ac.CurrentToolID, "stdout")
	cmd.Stderr = newProgressWriter(&stderrBuf, ac.Bus, ac.CurrentToolID, "stderr")

	runErr := cmd.Run()

	cap := ac.OutputCaps.CapFor("bash")
	cacheKey := bashCacheKey(cmdStr)

	if runErr != nil {
		output := stderrBuf.String()
		if output == "" {
			output = runErr.Error()
		}
		truncated, full := capTruncate(output, cap, ac.RecentUserHint)
		return Result{
			LLMOutput:  fmt.Sprintf("exit %d\n%s", cmd.ProcessState.ExitCode(), truncated),
			FullOutput: fmt.Sprintf("exit %d\nstdout: %s\nstderr: %s", cmd.ProcessState.ExitCode(), stdoutBuf.String(), full),
			Meta:       ResultMeta{CacheKey: cacheKey},
		}, nil
	}

	output := stdoutBuf.String()
	truncated, full := capTruncate(output, cap, ac.RecentUserHint)
	llm := truncated
	if llm == "" {
		// A model can't interpret an empty tool message, and some
		// OpenAI-compatible servers reject non-assistant messages with
		// no `content` field outright. Make success-with-no-stdout
		// explicit.
		llm = "(exit 0, no output)"
	}

	return Result{
		LLMOutput:  llm,
		FullOutput: full,
		Display:    strings.TrimSpace(truncated),
		Meta:       ResultMeta{CacheKey: cacheKey},
	}, nil
}

// runBashSandboxed routes through ac.Sandbox.Exec. The sandbox
// implementation streams stdout+stderr through a single io.Writer (the
// `<runtime> exec` CLI doesn't separate them via SSH-style stream
// IDs), so we capture everything as combined output. That's a small
// regression vs. the host path's separate buffers, but the truncation
// + display logic doesn't actually use the split.
func runBashSandboxed(ctx context.Context, cmdStr string, ac *AgentContext) (Result, error) {
	var combined bytes.Buffer
	w := newProgressWriter(&combined, ac.Bus, ac.CurrentToolID, "stdout")
	runErr := ac.Sandbox.Exec(ctx, w, cmdStr)

	output := combined.String()
	cap := ac.OutputCaps.CapFor("bash")
	truncated, full := capTruncate(output, cap, ac.RecentUserHint)
	cacheKey := bashCacheKey(cmdStr)

	if runErr != nil {
		exit := -1
		if ee, ok := runErr.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		}
		return Result{
			LLMOutput:  fmt.Sprintf("exit %d (sandbox: %s)\n%s", exit, ac.Sandbox.Runtime(), truncated),
			FullOutput: fmt.Sprintf("exit %d (sandbox: %s/%s)\n%s", exit, ac.Sandbox.Runtime(), ac.Sandbox.ContainerName(), full),
			Meta:       ResultMeta{CacheKey: cacheKey},
		}, nil
	}

	llm := truncated
	if llm == "" {
		llm = "(exit 0, no output)"
	}
	return Result{
		LLMOutput:  llm,
		FullOutput: full,
		Display:    strings.TrimSpace(truncated),
		Meta:       ResultMeta{CacheKey: cacheKey},
	}, nil
}

// bashCacheKey normalises a shell command into a dedup key. Long
// commands are clipped to keep the key sane; cosmetic whitespace is
// collapsed so `git status` and `git  status` dedup correctly.
func bashCacheKey(cmd string) string {
	const max = 200
	c := strings.Join(strings.Fields(cmd), " ")
	if len(c) > max {
		c = c[:max]
	}
	return "bash:" + c
}

// progressWriter is an io.Writer that mirrors writes into a backing buffer
// (so the final Result still has the full output) AND publishes a
// ToolCallProgress bus event per Write so the TUI can stream the output
// live as the command runs.
type progressWriter struct {
	buf    io.Writer
	bus    *bus.Bus
	id     string
	stream string // "stdout" / "stderr"
}

func newProgressWriter(buf io.Writer, b *bus.Bus, id, stream string) *progressWriter {
	return &progressWriter{buf: buf, bus: b, id: id, stream: stream}
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if err != nil {
		return n, err
	}
	if w.bus != nil && n > 0 {
		w.bus.Publish(bus.Event{
			Type: bus.EventToolCallProgress,
			Payload: map[string]any{
				"id":     w.id,
				"stream": w.stream,
				"text":   string(p[:n]),
			},
		})
	}
	return n, err
}
