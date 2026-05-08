// SPDX-License-Identifier: AGPL-3.0-or-later

package permissions

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
)

// Decision is the result of a permission check.
type Decision int

const (
	Allow  Decision = iota // Allow the tool call
	Deny                   // Deny the tool call
	Prompt                 // Ask the user
)

// PromptRequest is the bus payload for EventPermissionRequest. Subscribers
// (typically the TUI) read the fields, ask the user, and send the result on
// Respond. The agent blocks on Respond until a value arrives.
//
// AgentID and AgentRole identify the requesting agent — empty for the
// top-level agent, populated for spawn_agent children and workflow
// roles so the prompt UI can show "[reviewer abc123]" instead of a
// faceless tool call the user can't trace.
type PromptRequest struct {
	ToolName  string
	ArgString string
	Args      map[string]interface{}
	Diff      string // optional unified diff for edit-like calls
	AgentID   string
	AgentRole string
	Respond   chan Decision
	// Deadline is the wall-clock time at which the request will be
	// auto-denied if no decision arrives. Set by attach mode (the
	// daemon enforces a per-request timeout); zero in standalone mode
	// where there's no auto-deny. The modal renders a countdown only
	// when this is non-zero.
	Deadline time.Time
}

// Checker evaluates tool calls against allowlist and config mode.
type Checker struct {
	allowlist *Allowlist
	mode      string // "prompt", "allow", "deny"
	yolo      bool

	// turnAllow holds patterns the user granted via the modal's
	// "Allow Turn" button — auto-allow matching calls until the next
	// real user message resets it. Lazily allocated so an unused
	// checker keeps zero overhead. Reset via ResetTurnAllows from
	// the agent's Run loop right before EventUserMessage publish; see
	// the security caveat on TODO P2 #13 for why turn boundary alone
	// is wrong (chained sub-agent calls would extend the grant
	// indefinitely).
	turnAllow *Allowlist
}

// NewChecker creates a permission checker. `ask` rules always cause a
// prompt regardless of mode; `deny` rules always reject; `allow` rules
// auto-allow. Unmatched calls fall back to `mode`.
func NewChecker(allow, ask, deny []string, mode string) *Checker {
	if mode == "" {
		mode = "prompt"
	}
	return &Checker{
		allowlist: NewAllowlist(allow, ask, deny),
		mode:      mode,
	}
}

// SetYolo toggles yolo mode (auto-allow all).
func (c *Checker) SetYolo(yolo bool) {
	c.yolo = yolo
}

// Yolo reports whether yolo mode is currently active.
func (c *Checker) Yolo() bool {
	return c.yolo
}

// AddAllow appends a pattern to the live allowlist. Used when the user
// chooses "Allow + Remember" so subsequent calls in this session don't
// re-prompt. The caller is also responsible for persisting the rule to
// disk via config.AppendAllow.
func (c *Checker) AddAllow(pattern string) error {
	p, err := ParsePattern(pattern)
	if err != nil {
		return fmt.Errorf("parse pattern %q: %w", pattern, err)
	}
	if p == nil {
		return fmt.Errorf("invalid pattern %q (expected `tool(arg)` form)", pattern)
	}
	p.Kind = KindAllow
	c.allowlist.AppendPattern(p)
	return nil
}

// AddTurnAllow appends a pattern to the turn-scoped allowlist. The
// grant survives only until ResetTurnAllows fires (next real user
// message). Used by the modal's "Allow Turn" button — same pattern
// derivation as "Allow + Remember" but transient.
func (c *Checker) AddTurnAllow(pattern string) error {
	p, err := ParsePattern(pattern)
	if err != nil {
		return fmt.Errorf("parse pattern %q: %w", pattern, err)
	}
	if p == nil {
		return fmt.Errorf("invalid pattern %q (expected `tool(arg)` form)", pattern)
	}
	p.Kind = KindAllow
	if c.turnAllow == nil {
		c.turnAllow = NewAllowlist(nil, nil, nil)
	}
	c.turnAllow.AppendPattern(p)
	return nil
}

// ResetTurnAllows clears every turn-scoped grant so the next call
// matching one of those patterns prompts again. Called by the agent
// loop right before each new user message starts processing — that's
// the only safe boundary, since sub-agent fan-out and tool-call
// chains all run within one user-driven turn.
func (c *Checker) ResetTurnAllows() {
	c.turnAllow = nil
}

// HasTurnAllows reports whether any turn-scoped grants are active.
// Useful for /permissions debugging and tests.
func (c *Checker) HasTurnAllows() bool {
	return c.turnAllow != nil && len(c.turnAllow.patterns) > 0
}

// RemoveRule drops the matching tool(arg) entry of the given kind from
// the live allowlist. Used by the /permissions overlay so deleting a
// rule from disk also takes effect in the running session. Returns
// false if nothing matched. Caller persists the deletion separately via
// config.RemoveRule.
func (c *Checker) RemoveRule(pattern string, kind Kind) (bool, error) {
	p, err := ParsePattern(pattern)
	if err != nil {
		return false, fmt.Errorf("parse pattern %q: %w", pattern, err)
	}
	if p == nil {
		return false, fmt.Errorf("invalid pattern %q (expected `tool(arg)` form)", pattern)
	}
	return c.allowlist.Remove(p.Tool, p.Arg, kind), nil
}

// Patterns returns a copy of the live allowlist. Used by the
// /permissions overlay for rendering.
func (c *Checker) Patterns() []Pattern { return c.allowlist.Patterns() }

// DerivePattern returns a sensible allowlist pattern for the given tool
// call, suitable as the default rule the user accepts via "Allow +
// Remember". Conventions:
//
//	bash      → "bash(<first-word> *)"  (generalises to every invocation
//	                                    of that command)
//	read/grep/glob → "<tool>(**)"       (read-only, broadly safe)
//	write/edit/web_fetch → "<tool>(<exact-arg>)"  (conservative — exact
//	                                              path / url only)
//	anything else → "<tool>(*)"
func DerivePattern(toolName string, args map[string]any) string {
	switch toolName {
	case "bash":
		cmd, _ := args["cmd"].(string)
		cmd = strings.TrimSpace(cmd)
		if cmd == "" {
			return "bash(*)"
		}
		if idx := strings.IndexByte(cmd, ' '); idx > 0 {
			return "bash(" + cmd[:idx] + " *)"
		}
		return "bash(" + cmd + ")"

	case "read", "grep", "glob":
		return toolName + "(**)"

	case "write", "edit":
		if path, _ := args["path"].(string); path != "" {
			return fmt.Sprintf("%s(%s)", toolName, path)
		}

	case "web_fetch":
		if url, _ := args["url"].(string); url != "" {
			return "web_fetch(" + url + ")"
		}
	}
	return toolName + "(*)"
}

// Check evaluates a tool call and returns the decision.
//
// Order of precedence: yolo bypass > deny pattern > turn allow > ask
// pattern > persistent allow pattern > config mode default. Turn-allow
// sits above ask deliberately: if the user explicitly clicked "Allow
// Turn" on a pattern, an `ask` rule that would otherwise re-prompt
// for that pattern is what they're trying to silence. Deny rules
// still win, so a user grant can never override a hard "never".
func (c *Checker) Check(toolName string, args map[string]interface{}, busInst *bus.Bus) (Decision, error) {
	if c.yolo {
		emitAutoAllow(busInst, toolName, args, "yolo")
		return Allow, nil
	}

	argStr := buildArgString(args)
	matchArg := extractArg(toolName, args)

	matched, kind := c.allowlist.Match(toolName, matchArg)
	if matched && kind == KindDeny {
		return Deny, nil
	}

	if c.turnAllow != nil {
		if turnMatched, turnKind := c.turnAllow.Match(toolName, matchArg); turnMatched && turnKind == KindAllow {
			emitAutoAllow(busInst, toolName, args, "allow-turn")
			return Allow, nil
		}
	}

	if matched {
		switch kind {
		case KindAsk:
			return Prompt, nil
		case KindAllow:
			emitAutoAllow(busInst, toolName, args, "allowlist")
			return Allow, nil
		}
	}

	switch c.mode {
	case "allow":
		return Allow, nil
	case "deny":
		return Deny, fmt.Errorf("permission denied: %s(%s)", toolName, argStr)
	default:
		return Prompt, nil
	}
}

// extractArg returns the per-tool argument string used by allowlist
// matching: bash uses the raw command, file tools use the path arg,
// web_fetch uses the URL, MCP tools fall back to the generic
// key=value form. New tools can be added here without touching the
// allowlist code.
func extractArg(tool string, args map[string]any) string {
	switch tool {
	case "bash":
		if v, ok := args["cmd"].(string); ok {
			return v
		}
	case "read", "write", "edit", "grep":
		if v, ok := args["path"].(string); ok {
			return v
		}
	case "glob":
		if v, ok := args["pattern"].(string); ok {
			return v
		}
	case "web_fetch":
		if v, ok := args["url"].(string); ok {
			return v
		}
	case "web_search":
		if v, ok := args["query"].(string); ok {
			return v
		}
	case "spawn_agent":
		if v, ok := args["role"].(string); ok {
			return v
		}
	}
	return buildArgString(args)
}

// emitAutoAllow records an auto-allow decision (yolo bypass or allowlist
// match). Goes to slog for the audit trail and to the bus so future UI
// could surface it.
func emitAutoAllow(b *bus.Bus, tool string, args map[string]interface{}, reason string) {
	slog.Info("permission auto-allow", "tool", tool, "reason", reason)
	if b == nil {
		return
	}
	b.Publish(bus.Event{
		Type: bus.EventPermissionAuto,
		Payload: map[string]any{
			"tool":   tool,
			"args":   args,
			"reason": reason,
		},
	})
}

func buildArgString(args map[string]interface{}) string {
	var parts []string
	for k, v := range args {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return joinStr(parts, " ")
}

func joinStr(s []string, sep string) string {
	if len(s) == 0 {
		return ""
	}
	r := s[0]
	for _, v := range s[1:] {
		r += sep + v
	}
	return r
}
