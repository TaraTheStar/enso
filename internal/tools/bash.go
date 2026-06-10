// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
)

// BashTool executes a shell command and captures output.
type BashTool struct{}

func (t BashTool) Name() string { return "bash" }
func (t BashTool) Description() string {
	return "Execute a shell command. Args: cmd (string), optional timeout (int seconds, default 120), optional run_in_background (bool). Two ways to handle long-running work: (1) a command that FINISHES on its own but is slow (a big test suite, a long build) — run it in the foreground and raise `timeout`; you want the result; (2) a command that NEVER returns on its own (a dev server, file watcher, `tail -f`) — pass run_in_background:true, then read it with bash_output and stop it with bash_kill. A foreground command is killed when it exceeds its timeout. Output is truncated for the model but stored fully in the session."
}
func (t BashTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"cmd": map[string]interface{}{"type": "string", "description": "Shell command to execute"},
			"timeout": map[string]interface{}{
				"type":        "integer",
				"description": "Seconds to wait before a foreground command is killed (default 120). Raise it for a command that finishes on its own but is slow, like a big test suite — honoured as given, up to a safety cap (1 hour by default). Ignored when run_in_background is true.",
			},
			"run_in_background": map[string]interface{}{
				"type":        "boolean",
				"description": "Start the command detached and return immediately with a job id; read its output with bash_output and stop it with bash_kill. Use for a command that never returns on its own — a dev server, a file watcher, tail -f.",
			},
		},
		"required": []string{"cmd"},
	}
}

func (t BashTool) Run(ctx context.Context, args map[string]interface{}, ac *AgentContext) (Result, error) {
	cmdStr, _ := args["cmd"].(string)
	if cmdStr == "" {
		return Result{}, fmt.Errorf("bash: cmd required")
	}

	if bg, _ := args["run_in_background"].(bool); bg {
		if ac.BashJobs == nil {
			return Result{LLMOutput: "background jobs are not available in this context"}, nil
		}
		return ac.BashJobs.Start(cmdStr, ac)
	}

	reqTimeout := optIntArg(args["timeout"])
	timeout := ac.ToolTimeouts.EffectiveBash(reqTimeout)

	// Pre-run nudge: a command that can't return on its own would only
	// block until the timeout. Steer it toward run_in_background instead
	// of burning the budget — unless the model explicitly bounded it with
	// a `timeout` arg (it accepts the time limit) or the command already
	// bounds/detaches itself. The timeout backstop still covers anything
	// the heuristic misses.
	if reqTimeout == 0 {
		if reason, ok := looksNonTerminating(cmdStr); ok {
			bound := "would block indefinitely (the bash timeout is disabled)"
			if timeout > 0 {
				bound = fmt.Sprintf("would block until the %ds timeout", int(timeout/time.Second))
			}
			return Result{
				LLMOutput: fmt.Sprintf("not run — %s, so in the foreground it %s. "+
					"Start it with run_in_background:true and read it with bash_output / stop with bash_kill; "+
					"for a one-shot peek use a bounded form (e.g. `tail -n 100` instead of `tail -f`); "+
					"or pass an explicit `timeout` to run it time-bounded anyway.", reason, bound),
				Meta: ResultMeta{CacheKey: bashCacheKey(cmdStr)},
			}, nil
		}
		// A foreground sleep / poll-loop only burns the turn budget waiting.
		// Steer the model to run the real work in the background and poll
		// THAT, rather than sleeping in the foreground.
		if reason, ok := looksLikePolling(cmdStr); ok {
			return Result{
				LLMOutput: fmt.Sprintf("not run — %s. "+
					"If you're waiting on a background job, poll it with bash_output (and stop it with bash_kill) instead of sleeping; "+
					"if you're waiting on an external state change, run a single bounded check rather than chaining sleeps. "+
					"To run the sleep anyway, pass an explicit `timeout`.", reason),
				Meta: ResultMeta{CacheKey: bashCacheKey(cmdStr)},
			}, nil
		}
	}

	return runBashHost(ctx, cmdStr, timeout, ac)
}

// optIntArg coerces an optional JSON-decoded numeric tool arg to int.
// Tool args arrive as float64 from encoding/json, but tests and some
// adapters may pass a plain int; both are accepted. Missing or non-numeric
// yields 0 ("unset"). Distinct from lsp.go's argInt, which is required and
// 1-based.
func optIntArg(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}

// secretEnvSubstrings are the (upper-cased) name fragments that mark an
// environment variable as a credential. Any var whose NAME contains one
// is dropped from the bash child env. Matching on the name (not the
// value) keeps it cheap and avoids false positives on innocent values
// that happen to look key-ish.
var secretEnvSubstrings = []string{
	"API_KEY", "APIKEY", "SECRET", "TOKEN",
	"PASSWORD", "PASSWD", "CREDENTIAL", "PRIVATE_KEY", "ACCESS_KEY",
}

// scrubbedBashEnv returns env (KEY=VALUE entries, e.g. from os.Environ)
// with credential-shaped variables removed (S9). It drops:
//
//   - every ENSO_* var — these exist specifically to feed enso secrets
//     via the `api_key = "$ENSO_OPENAI_KEY"` indirection, so the model
//     should never see them; and
//   - any var whose name contains a secretEnvSubstrings fragment
//     (OPENAI_API_KEY, AWS_SECRET_ACCESS_KEY, GITHUB_TOKEN, …).
//
// This is a name-pattern denylist, not an allowlist, so the child shell
// keeps PATH/HOME/LANG/toolchain vars a build legitimately needs.
// Trade-off: it also hides genuine tokens like GITHUB_TOKEN from the
// model — intended, since the bash tool is driven by an untrusted model;
// legitimate auth should come from a credential helper or an isolated
// backend, not an inherited secret. Documented in AGENTS.md.
func scrubbedBashEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if isSecretEnvName(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// isSecretEnvName reports whether an env var name should be scrubbed.
func isSecretEnvName(name string) bool {
	up := strings.ToUpper(name)
	if strings.HasPrefix(up, "ENSO_") {
		return true
	}
	for _, frag := range secretEnvSubstrings {
		if strings.Contains(up, frag) {
			return true
		}
	}
	return false
}

// runBashHost runs `sh -c <cmd>` with cwd at the project root.
// Isolation (container/VM) is the Backend the whole worker runs in —
// there is no separate in-process sandbox path.
//
// timeout > 0 bounds the command's wall-clock runtime; on expiry the whole
// process group is killed and a normal (non-error) Result is returned with
// the partial output and a hint, so the turn continues rather than the
// agent hanging. timeout <= 0 disables the bound (legacy behaviour). A
// user interrupt (parent ctx cancelled) is distinguished from our timeout
// so it isn't mislabelled.
func runBashHost(ctx context.Context, cmdStr string, timeout time.Duration, ac *AgentContext) (Result, error) {
	runCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, "sh", "-c", cmdStr)
	cmd.Dir = ac.Cwd
	// Scrub secret-shaped vars from the child env (S9). On the local
	// backend the worker IS the host enso process, so an unscrubbed
	// child shell would let the model `echo $OPENAI_API_KEY` (or any
	// provider key resolved from $ENSO_*) and exfiltrate it. On isolated
	// backends the guest env shouldn't carry these anyway, so the scrub
	// is a harmless defense-in-depth there.
	cmd.Env = scrubbedBashEnv(os.Environ())
	// Put the shell into its own process group so cancel kills the
	// whole pipeline, not just `sh` (children like `long_thing | foo`
	// would otherwise survive as orphans).
	setProcessGroup(cmd)
	cmd.Cancel = func() error {
		return killProcessGroup(cmd.Process)
	}
	// Stdout/Stderr below are in-process writers, so Run blocks until the
	// pipe copy goroutines see EOF. A grandchild that escaped the process
	// group (setsid, nohup &) keeps the write end open past the group kill,
	// which would hang Run — and the agent's turn — forever. WaitDelay tells
	// the stdlib to force-close the pipes after the grace window.
	cmd.WaitDelay = 5 * time.Second

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = newProgressWriter(&stdoutBuf, ac.Bus, ac.CurrentToolID, "stdout")
	cmd.Stderr = newProgressWriter(&stderrBuf, ac.Bus, ac.CurrentToolID, "stderr")

	runErr := cmd.Run()

	cacheKey := bashCacheKey(cmdStr)

	// Our timeout fired (not a user Ctrl-C, which cancels the parent ctx).
	if timeout > 0 && runCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		stdout, stderr := stdoutBuf.String(), stderrBuf.String()
		combined := stdout
		if stderr != "" {
			if combined != "" {
				combined += "\n"
			}
			combined += stderr
		}
		truncated, _ := truncateWithRecoverySplit(ac, "bash", compressBash(ac, cmdStr, combined), combined)
		if truncated == "" {
			truncated = "(no output before timeout)"
		}
		secs := int(timeout / time.Second)
		maxSecs := int(ac.ToolTimeouts.bashMax() / time.Second)
		return Result{
			LLMOutput: fmt.Sprintf("command timed out after %ds and was killed. "+
				"If it never returns on its own (server, watcher, tail -f), re-run with run_in_background:true and manage it via bash_output / bash_kill; "+
				"otherwise it finishes on its own but is slow — re-run with a larger `timeout` (max %ds).\nPartial output:\n%s", secs, maxSecs, truncated),
			FullOutput: fmt.Sprintf("timed out after %ds\nstdout: %s\nstderr: %s", secs, stdout, stderr),
			Meta:       ResultMeta{CacheKey: cacheKey},
		}, nil
	}

	if runErr != nil {
		output := stderrBuf.String()
		if output == "" {
			output = runErr.Error()
		}
		truncated, full := truncateWithRecoverySplit(ac, "bash", compressBash(ac, cmdStr, output), output)
		return Result{
			LLMOutput:  fmt.Sprintf("exit %d\n%s", cmd.ProcessState.ExitCode(), truncated),
			FullOutput: fmt.Sprintf("exit %d\nstdout: %s\nstderr: %s", cmd.ProcessState.ExitCode(), stdoutBuf.String(), full),
			Meta:       ResultMeta{CacheKey: cacheKey},
		}, nil
	}

	output := stdoutBuf.String()
	truncated, full := truncateWithRecoverySplit(ac, "bash", compressBash(ac, cmdStr, output), output)
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

// compressBash applies the command-keyed declarative filter and the
// content-shape structural compressors to raw command output (R1/H5). The
// result is what the model sees (after the output caps); the raw output is
// still spilled so the model can recover anything dropped. A nil filter set
// or no match falls through to structural compression, then to raw.
func compressBash(ac *AgentContext, cmd, raw string) string {
	// A nil FilterSet means the compression feature is disabled wholesale
	// (config `compress = false`); fall back to plain truncation.
	if raw == "" || ac.Filters == nil {
		return raw
	}
	out, _ := compressOutput(ac.Filters, cmd, raw)
	return out
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
