// SPDX-License-Identifier: AGPL-3.0-or-later

package permissions

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
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
	ToolName  string                 `json:"tool_name"`
	ArgString string                 `json:"arg_string,omitempty"`
	Args      map[string]interface{} `json:"args,omitempty"`
	Diff      string                 `json:"diff,omitempty"` // optional unified diff for edit-like calls
	AgentID   string                 `json:"agent_id,omitempty"`
	AgentRole string                 `json:"agent_role,omitempty"`
	// Respond is the in-process channel the agent loop blocks on for the
	// user's decision. Host-local only — never serialized. Daemon-socket
	// subscribers see PermissionRequest events without it (and so cannot
	// answer; that flow goes through PermissionResponseReq).
	Respond chan Decision `json:"-"`
	// Deadline is the wall-clock time at which the request will be
	// auto-denied if no decision arrives. Set by attach mode (the
	// daemon enforces a per-request timeout); zero in standalone mode
	// where there's no auto-deny. The modal renders a countdown only
	// when this is non-zero. Not serialized — the per-request timeout
	// lives in the daemon and would only be confusing on the wire.
	Deadline time.Time `json:"-"`
}

// Checker evaluates tool calls against allowlist and config mode.
//
// mu guards every mutable field — yolo, the turnAllow pointer + its
// patterns, and mutations of the persistent allowlist (AddAllow /
// RemoveRule) against concurrent reads in Check. This matters now that
// grants can arrive off the agent goroutine: the worker's serveControl
// handles CtrlAddAllow / CtrlSetYolo on a separate goroutine from the
// agent loop that calls Check, and the modal's "Allow Turn" / agent's
// ResetTurnAllows likewise race a concurrent Check. mode is set once at
// construction and never mutated, so reading it under the lock is just
// for uniformity.
type Checker struct {
	mu sync.Mutex

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
	c.mu.Lock()
	c.yolo = yolo
	c.mu.Unlock()
}

// Yolo reports whether yolo mode is currently active.
func (c *Checker) Yolo() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
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
	c.mu.Lock()
	c.allowlist.AppendPattern(p)
	c.mu.Unlock()
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
	c.mu.Lock()
	if c.turnAllow == nil {
		c.turnAllow = NewAllowlist(nil, nil, nil)
	}
	c.turnAllow.AppendPattern(p)
	c.mu.Unlock()
	return nil
}

// ResetTurnAllows clears every turn-scoped grant so the next call
// matching one of those patterns prompts again. Called by the agent
// loop right before each new user message starts processing — that's
// the only safe boundary, since sub-agent fan-out and tool-call
// chains all run within one user-driven turn.
func (c *Checker) ResetTurnAllows() {
	c.mu.Lock()
	c.turnAllow = nil
	c.mu.Unlock()
}

// HasTurnAllows reports whether any turn-scoped grants are active.
// Useful for /permissions debugging and tests.
func (c *Checker) HasTurnAllows() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
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
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.allowlist.Remove(p.Tool, p.Arg, kind), nil
}

// Patterns returns a copy of the live allowlist. Used by the
// /permissions overlay for rendering.
func (c *Checker) Patterns() []Pattern {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.allowlist.Patterns()
}

// DerivePattern returns a sensible allowlist pattern for the given tool
// call, suitable as the default rule the user accepts via "Allow +
// Remember". `cwd` is the session working directory, used to path-scope
// read/grep grants. Conventions:
//
//	bash      → "bash(<first-word> *)"  (generalises to every invocation
//	                                    of that command)
//	read/grep → "<tool>(<cwd>/**)" when the path is inside cwd, else
//	            "<tool>(<exact-clean-path>)"  (project-scoped, never `**`)
//	glob      → "glob(<exact-pattern>)"  (the glob the user just ran)
//	write/edit/web_fetch → "<tool>(<exact-arg>)"  (conservative — exact
//	                                              path / url only)
//	anything else → "<tool>(*)"
//
// read/grep are deliberately NOT `**`: a bare `read(**)` matches
// /etc/passwd, ~/.ssh, and enso's own API-key config, so "remember read"
// would silently grant whole-filesystem read. Scoping to cwd keeps the
// rule useful within the project while keeping those out of reach.
func DerivePattern(toolName string, args map[string]any, cwd string) string {
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

	case "read", "grep":
		if path, _ := args["path"].(string); path != "" {
			return derivePathPattern(toolName, path, cwd)
		}

	case "glob":
		// glob's arg is itself a pattern, matched lexically (not as a
		// path), so the conservative "remember" is the exact pattern the
		// user just ran — never a broadened `**`.
		if pat, _ := args["pattern"].(string); pat != "" {
			return "glob(" + pat + ")"
		}

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

// derivePathPattern scopes a read/grep "remember" rule. A path inside the
// session cwd becomes a project-subtree rule (`read(<cwd>/**)`) — broad
// enough to be useful but unable to reach /etc, ~/.ssh, or enso's own key
// store outside the project. Anything outside cwd (additional dirs,
// absolute reads) gets an EXACT cleaned-path rule so "remember" never
// widens to the whole filesystem. An empty cwd falls back to exact too.
func derivePathPattern(tool, path, cwd string) string {
	clean := filepath.Clean(path)
	if cwd != "" {
		c := filepath.Clean(cwd)
		if clean == c || strings.HasPrefix(clean, c+string(filepath.Separator)) {
			return fmt.Sprintf("%s(%s/**)", tool, c)
		}
	}
	return fmt.Sprintf("%s(%s)", tool, clean)
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
	matchArg := extractArg(toolName, args)

	// Evaluate all mutable state under the lock; defer the bus emission
	// and error formatting until after unlock so a slow bus subscriber
	// can't pin the checker (and to keep the critical section pure).
	c.mu.Lock()
	decision, autoReason, modeDeny := c.evalLocked(toolName, matchArg)
	c.mu.Unlock()

	if autoReason != "" {
		emitAutoAllow(busInst, toolName, args, autoReason)
	}
	if modeDeny {
		return Deny, fmt.Errorf("permission denied: %s(%s)", toolName, buildArgString(args))
	}
	return decision, nil
}

// evalLocked computes the decision against the checker's mutable state.
// Caller must hold c.mu. It returns the decision, a non-empty
// autoReason when the call was auto-allowed (so the caller emits the
// audit event after releasing the lock), and modeDeny=true when the
// denial came from the "deny" mode default (which carries an error)
// rather than a deny rule (which doesn't). See Check's doc comment for
// the precedence rules.
func (c *Checker) evalLocked(toolName, matchArg string) (decision Decision, autoReason string, modeDeny bool) {
	if c.yolo {
		return Allow, "yolo", false
	}

	matched, kind := c.allowlist.Match(toolName, matchArg)
	if matched && kind == KindDeny {
		return Deny, "", false
	}

	if c.turnAllow != nil {
		if turnMatched, turnKind := c.turnAllow.Match(toolName, matchArg); turnMatched && turnKind == KindAllow {
			return Allow, "allow-turn", false
		}
	}

	if matched {
		switch kind {
		case KindAsk:
			return Prompt, "", false
		case KindAllow:
			return Allow, "allowlist", false
		}
	}

	switch c.mode {
	case "allow":
		return Allow, "", false
	case "deny":
		return Deny, "", true
	default:
		return Prompt, "", false
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
