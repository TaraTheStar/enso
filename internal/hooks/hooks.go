// SPDX-License-Identifier: AGPL-3.0-or-later

// Package hooks runs user-configured shell commands at well-known
// lifecycle moments. Two events are wired: on_file_edit (after the
// edit/write tools succeed) and on_session_end (when the top-level
// agent.Run loop returns).
//
// Surfacing failures is intentionally narrow per project convention:
// timeouts and template errors call Warn; non-zero exit codes from
// the user's command are silently swallowed (the user wrote the
// command, they own its semantics — `gofmt` returning 1 on a parse
// error is normal, not an enso problem).
//
// `sh -c` is used to invoke the rendered command, so users can pipe,
// quote, redirect, etc. On Windows this will fail with a clear
// "sh: not found"; cross-platform shell selection is out of scope.
//
// Auto-quoting: every interpolated value (`.Path`, `.Tool`, `.Cwd`,
// `.SessionID`) is POSIX-shell-single-quoted before substitution. So
// `gofmt -w {{.Path}}` is safe even when Path is a model-controlled
// filename like `foo;rm -rf ~`. Do NOT wrap interpolated values in
// `'...'` or `"..."` in your template — the auto-quoting handles it,
// and your wrapper would either double-quote (ugly) or break (literal
// quotes leaking into the arg). For the rare case where you actually
// want raw substitution, use `{{.Raw.Path}}` (etc.) — explicit opt-out.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
)

// DefaultTimeout caps how long a single hook command may run before
// it's killed. Auto-format on a single file should take <1s; 10s is
// generous and matches Claude Code's default.
const DefaultTimeout = 10 * time.Second

// Hooks holds user-configured shell commands and the callback used to
// surface real failures. Construct via New; a nil *Hooks is safe to
// call methods on (every method is a no-op).
type Hooks struct {
	OnFileEditCmd   string
	OnSessionEndCmd string
	// OnEventCmd fires for every wire-safe bus event (UserMessage,
	// AssistantDelta-class, ToolCallStart/End, AgentIdle/End, …) and
	// receives the event as JSON on stdin rather than via template
	// vars. Designed for observers that translate per-event into
	// another protocol (status boards, audit pipelines).
	OnEventCmd string
	// OnEvents, when non-nil and non-empty, filters which event types
	// fire OnEventCmd — list the wire type strings ("UserMessage",
	// "ToolCallStart", …). Empty/nil uses DefaultEventFilter to skip
	// the chatty per-token deltas that would otherwise spawn a process
	// per token.
	OnEvents []string
	// Env are extra environment variables merged onto each hook
	// subprocess's environment. Inherits os.Environ() by default; Env
	// values override matching keys. Lets users keep secrets (e.g.
	// WATCHOURAI_TOKEN) in enso config instead of shell rc files.
	Env map[string]string

	Timeout time.Duration

	// Warn fires only for genuine misconfigurations: template render
	// errors and command timeouts. Default routes to slog.Warn — the
	// host (TUI) overrides this to also surface the message in chat.
	Warn func(format string, args ...any)
}

// Config carries every user-configurable hook setting in one struct
// so New's signature doesn't grow per added hook.
type Config struct {
	OnFileEdit   string
	OnSessionEnd string
	OnEvent      string
	OnEvents     []string
	Env          map[string]string
}

// New builds a Hooks from the loaded config. Empty/nil fields disable
// the corresponding behaviour.
func New(c Config) *Hooks {
	return &Hooks{
		OnFileEditCmd:   c.OnFileEdit,
		OnSessionEndCmd: c.OnSessionEnd,
		OnEventCmd:      c.OnEvent,
		OnEvents:        c.OnEvents,
		Env:             c.Env,
		Timeout:         DefaultTimeout,
		Warn: func(format string, args ...any) {
			slog.Warn(fmt.Sprintf(format, args...))
		},
	}
}

// DefaultEventFilter is the set of event types OnEventCmd fires for
// when the user hasn't supplied OnEvents. Deltas are excluded — at
// per-token frequency they would spawn one subprocess per token and
// melt the box. Observers that want deltas opt in explicitly.
var DefaultEventFilter = []string{
	"UserMessage",
	"AgentStart",
	"AgentIdle",
	"AgentEnd",
	"AssistantDone",
	"Cancelled",
	"Error",
	"ToolCallStart",
	"ToolCallEnd",
	"PermissionRequest",
	"Compacted",
}

// OnFileEdit fires the configured command (if any) after a successful
// edit/write tool run. Vars: .Path, .Tool.
func (h *Hooks) OnFileEdit(cwd, path, tool string) {
	if h == nil || h.OnFileEditCmd == "" {
		return
	}
	h.run("on_file_edit", h.OnFileEditCmd, cwd, map[string]any{
		"Path": path,
		"Tool": tool,
	})
}

// OnEvent fires the configured command (if any) for one bus event,
// piping the event as JSON to the command's stdin. Filtered to the
// user's OnEvents list, falling back to DefaultEventFilter when unset.
// Unserializable events (those bus.Event.WireForm rejects) are
// silently skipped — the host-only permission-response feedback loop,
// for instance.
//
// JSON shape on stdin:
//
//	{
//	  "session_id": "...",
//	  "cwd":        "...",
//	  "type":       "ToolCallStart",
//	  "payload":    { ... }    // event-type specific
//	}
//
// Fires synchronously to the caller's goroutine but bounded by Timeout;
// callers are expected to call from a fanout goroutine, not the agent's
// hot path.
func (h *Hooks) OnEvent(cwd, sessionID string, evt bus.Event) {
	if h == nil || h.OnEventCmd == "" {
		return
	}
	typ, payload, ok := evt.WireForm()
	if !ok {
		return
	}
	if !h.eventAllowed(typ) {
		return
	}
	body, err := json.Marshal(struct {
		SessionID string          `json:"session_id"`
		Cwd       string          `json:"cwd"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload,omitempty"`
	}{
		SessionID: sessionID,
		Cwd:       cwd,
		Type:      typ,
		Payload:   payload,
	})
	if err != nil {
		h.Warn("hooks: on_event marshal failed: %v", err)
		return
	}
	h.runWithStdin("on_event", h.OnEventCmd, cwd, body)
}

// eventAllowed reports whether the wire type passes the configured
// filter. Empty OnEvents → DefaultEventFilter; explicit OnEvents wins
// entirely (no merge with defaults — listing one is "I want only these").
func (h *Hooks) eventAllowed(typ string) bool {
	list := h.OnEvents
	if len(list) == 0 {
		list = DefaultEventFilter
	}
	return slices.Contains(list, typ)
}

// OnSessionEnd fires when the top-level agent.Run loop returns. Vars:
// .SessionID, .Cwd. Subagent / RunOneShot exits do NOT fire — those
// aren't user-visible session ends.
func (h *Hooks) OnSessionEnd(cwd, sessionID string) {
	if h == nil || h.OnSessionEndCmd == "" {
		return
	}
	h.run("on_session_end", h.OnSessionEndCmd, cwd, map[string]any{
		"SessionID": sessionID,
		"Cwd":       cwd,
	})
}

// runWithStdin invokes the user's command with `body` piped to stdin.
// Unlike `run`, the command is NOT template-rendered — on_event-style
// hooks consume structured JSON, not interpolated strings. Same
// best-effort posture: timeouts log via Warn, non-zero exits stay silent.
func (h *Hooks) runWithStdin(label, cmdStr, cwd string, body []byte) {
	if strings.TrimSpace(cmdStr) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Dir = cwd
	cmd.Stdin = bytes.NewReader(body)
	cmd.Env = h.mergedEnv()
	cmd.WaitDelay = time.Second

	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		if snippet := firstNonEmptyLine(out); snippet != "" {
			h.Warn("hooks: %s timed out after %s: %s", label, h.Timeout, snippet)
		} else {
			h.Warn("hooks: %s timed out after %s", label, h.Timeout)
		}
		return
	}
	_ = err
}

// mergedEnv returns os.Environ() with h.Env merged in (Env values
// override matching keys). Returns nil when Env is empty so the
// stdlib uses os.Environ() directly without an alloc.
func (h *Hooks) mergedEnv() []string {
	if len(h.Env) == 0 {
		return nil
	}
	base := os.Environ()
	// Build a key→index map for in-place override.
	idx := make(map[string]int, len(base))
	for i, kv := range base {
		if j := strings.IndexByte(kv, '='); j > 0 {
			idx[kv[:j]] = i
		}
	}
	for k, v := range h.Env {
		entry := k + "=" + v
		if i, ok := idx[k]; ok {
			base[i] = entry
		} else {
			base = append(base, entry)
		}
	}
	return base
}

func (h *Hooks) run(label, tmpl, cwd string, vars map[string]any) {
	rendered, err := renderTemplate(tmpl, prepareVars(vars))
	if err != nil {
		h.Warn("hooks: %s template error: %v", label, err)
		return
	}
	if strings.TrimSpace(rendered) == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), h.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", rendered)
	cmd.Dir = cwd
	// WaitDelay matters because we capture output below: when the ctx
	// fires, sh gets SIGKILL but any grandchildren (e.g., `sleep`
	// under `sh -c`) inherit the stdout/stderr fds, so Wait would
	// otherwise block on the pipe drain forever. WaitDelay tells the
	// stdlib to forcibly close the I/O after the grace window so the
	// hook caller is never stuck.
	cmd.WaitDelay = time.Second

	// CombinedOutput so the timeout warning can include a snippet of
	// what the hook was doing when it died. Capturing stdout+stderr
	// adds a bounded buffer (limited by hook lifetime + Timeout) and
	// is much cheaper than asking users to tail enso.log to figure
	// out which gofmt/prettier/etc. is hung.
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		if snippet := firstNonEmptyLine(out); snippet != "" {
			h.Warn("hooks: %s timed out after %s: %s", label, h.Timeout, snippet)
		} else {
			h.Warn("hooks: %s timed out after %s", label, h.Timeout)
		}
		return
	}
	// Non-zero exit, exec failures (e.g. sh missing): silent. The
	// user's command is responsible for its own semantics.
	_ = err
}

// firstNonEmptyLine returns the first non-blank line of `b` trimmed
// and capped at 120 chars. Used to summarise hook output in warning
// messages — we want enough to identify the failure mode without
// flooding the chat with a multi-line stack trace.
func firstNonEmptyLine(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		if len(s) > 120 {
			s = s[:120] + "…"
		}
		return s
	}
	return ""
}

func renderTemplate(tmpl string, vars map[string]any) (string, error) {
	t, err := template.New("hook").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, vars); err != nil {
		return "", fmt.Errorf("execute: %w", err)
	}
	return buf.String(), nil
}

// prepareVars returns a map mirroring `in` where every leaf value has
// been POSIX-shell-single-quoted, plus a "Raw" key holding the
// originals so templates can opt into raw substitution via
// `{{.Raw.Field}}`. This is the single chokepoint that makes hook
// commands safe against shell-metachar injection from model-controlled
// values like file paths.
func prepareVars(in map[string]any) map[string]any {
	outCap := len(in)
	if len(in) < math.MaxInt {
		outCap = len(in) + 1
	}
	out := make(map[string]any, outCap)
	raw := make(map[string]any, len(in))
	for k, v := range in {
		s := fmt.Sprint(v)
		out[k] = shellQuote(s)
		raw[k] = s
	}
	out["Raw"] = raw
	return out
}

// shellQuote wraps `s` in POSIX-safe single quotes. Embedded singles
// are encoded as the standard `'\”` (close-quote, escape, reopen).
// The empty string becomes `”` (an explicit empty arg, not nothing).
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
