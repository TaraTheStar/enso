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
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"text/template"
	"time"
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
	Timeout         time.Duration

	// Warn fires only for genuine misconfigurations: template render
	// errors and command timeouts. Default routes to slog.Warn — the
	// host (TUI) overrides this to also surface the message in chat.
	Warn func(format string, args ...any)
}

// New builds a Hooks from the loaded config strings. Empty strings
// disable the corresponding event.
func New(onFileEdit, onSessionEnd string) *Hooks {
	return &Hooks{
		OnFileEditCmd:   onFileEdit,
		OnSessionEndCmd: onSessionEnd,
		Timeout:         DefaultTimeout,
		Warn: func(format string, args ...any) {
			slog.Warn(fmt.Sprintf(format, args...))
		},
	}
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
	out := make(map[string]any, len(in)+1)
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
