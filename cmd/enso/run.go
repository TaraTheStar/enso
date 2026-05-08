// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"sync/atomic"

	"github.com/TaraTheStar/enso/internal/daemon"

	"github.com/TaraTheStar/enso/internal/agent"
	"github.com/TaraTheStar/enso/internal/agents"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/hooks"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/lsp"
	"github.com/TaraTheStar/enso/internal/mcp"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/sandbox"
	"github.com/TaraTheStar/enso/internal/session"
	"github.com/TaraTheStar/enso/internal/tools"
	"github.com/TaraTheStar/enso/internal/workflow"
)

// runOnce is the implementation behind `enso run "<prompt>"`. It executes one
// user message non-interactively, streaming assistant text to stdout, running
// tool calls under the configured permission policy, and exiting with a
// non-zero code if any tool errored or the loop was cancelled.
func runOnce(promptArgs []string) error {
	if flagFormat != "text" && flagFormat != "json" {
		return fmt.Errorf("--format: want \"text\" or \"json\", got %q", flagFormat)
	}

	prompt, err := resolvePrompt(promptArgs)
	if err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get cwd: %w", err)
	}

	cfg, err := loadOrWelcome(cwd)
	if err != nil {
		if errors.Is(err, errFirstRunWelcome) {
			return err
		}
		return fmt.Errorf("load config: %w", err)
	}

	providers, err := llm.BuildProviders(cfg.Providers)
	if err != nil {
		return err
	}
	requested := cfg.DefaultProvider
	if flagProvider != "" {
		requested = flagProvider
	}
	defaultName, err := pickDefaultProviderName(providers, requested)
	if err != nil {
		return err
	}
	provider := providers[defaultName]

	denies := append([]string{}, cfg.Permissions.Deny...)
	if ignore, err := permissions.LoadIgnoreFile(filepath.Join(cwd, ".ensoignore")); err == nil {
		denies = append(denies, permissions.IgnoreToDenyPatterns(ignore)...)
	}
	checker := permissions.NewChecker(cfg.Permissions.Allow, cfg.Permissions.Ask, denies, cfg.Permissions.Mode)
	if flagYolo {
		checker.SetYolo(true)
	}

	registry := tools.BuildDefault()
	agent.RegisterSpawn(registry)
	tools.RegisterSearch(registry, cfg.Search)

	mcpMgr := mcp.NewManager()
	if len(cfg.MCP) > 0 {
		mcpMgr.Start(context.Background(), cfg.MCP)
		mcpMgr.RegisterAll(registry)
	}
	defer mcpMgr.Close()

	lspMgr := lsp.NewManager(cwd, cfg.LSP)
	tools.RegisterLSP(registry, lspMgr)
	defer lspMgr.Close()

	var sandboxMgr *sandbox.Manager
	if sbCfg, on := sandbox.FromConfig(cfg); on {
		sandboxMgr, err = sandbox.NewManager(cwd, sbCfg)
		if err != nil {
			return fmt.Errorf("sandbox: %w", err)
		}
		ensureCtx, ensureCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ensureCancel()
		if err := sandboxMgr.Ensure(ensureCtx, os.Stderr); err != nil {
			return fmt.Errorf("sandbox: ensure %s: %w", sandboxMgr.ContainerName(), err)
		}
	}
	var restrictedRoots []string
	if !cfg.Permissions.DisableFileConfinement {
		restrictedRoots = append([]string{cwd}, cfg.Permissions.AdditionalDirectories...)
	}

	spec, err := agents.Find(cwd, flagAgent)
	if err != nil {
		return err
	}
	applied := agents.Apply(spec, provider, registry)
	provider = applied.Provider
	registry = applied.Registry

	busInst := bus.New()

	var (
		store   *session.Store
		writer  *session.Writer
		resumed *session.State
	)
	if !flagEphemeral {
		store, err = session.Open()
		if err != nil {
			return fmt.Errorf("open session store: %w", err)
		}
		defer store.Close()
		if flagSession != "" {
			resumed, err = session.Load(store, flagSession)
			if err != nil {
				return fmt.Errorf("resume %s: %w", flagSession, err)
			}
			writer, err = session.AttachWriter(store, flagSession)
			if err != nil {
				return fmt.Errorf("attach writer: %w", err)
			}
		} else {
			writer, err = session.NewSession(store, provider.Model, provider.Name, cwd)
			if err != nil {
				return fmt.Errorf("create session: %w", err)
			}
		}
	}

	maxTurns := flagMaxTurns
	if applied.MaxTurns > 0 {
		maxTurns = applied.MaxTurns
	}
	acfg := agent.Config{
		Providers:             providers,
		DefaultProvider:       defaultName,
		Bus:                   busInst,
		Registry:              registry,
		Perms:                 checker,
		Writer:                writer,
		Cwd:                   cwd,
		MaxTurns:              maxTurns,
		GitAttribution:        cfg.Git.Attribution,
		GitAttributionName:    cfg.Git.AttributionName,
		ExtraSystemPrompt:     applied.PromptAppend,
		AdditionalDirectories: cfg.Permissions.AdditionalDirectories,
		RestrictedRoots:       restrictedRoots,
		Hooks:                 hooks.New(cfg.Hooks.OnFileEdit, cfg.Hooks.OnSessionEnd),
		WebFetchAllowHosts:    cfg.WebFetch.AllowHosts,
	}
	// Avoid the typed-nil-into-interface trap: assign Sandbox only when
	// a manager actually exists. Otherwise the interface is non-nil but
	// holds a nil *sandbox.Manager and bash.go's `ac.Sandbox != nil`
	// check passes — then dispatch panics on first method call.
	if sandboxMgr != nil {
		acfg.Sandbox = sandboxMgr
	}
	if writer != nil {
		acfg.SessionID = writer.SessionID()
	}
	if resumed != nil {
		acfg.History = resumed.History
	}

	agt, err := agent.New(acfg)
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}

	// In non-interactive mode, a permission prompt has no UI. Auto-deny so
	// the model gets a "permission denied" tool result and can react. Run with
	// --yolo to bypass.
	permCh := busInst.Subscribe(8)
	go func() {
		for evt := range permCh {
			if evt.Type != bus.EventPermissionRequest {
				continue
			}
			req, ok := evt.Payload.(*permissions.PromptRequest)
			if !ok {
				continue
			}
			if flagFormat == "json" {
				emitJSON(map[string]any{"type": "permission_auto_deny", "tool": req.ToolName})
			} else {
				fmt.Fprintf(os.Stderr, "[denied] %s (run with --yolo to auto-allow)\n", req.ToolName)
			}
			req.Respond <- permissions.Deny
		}
	}()

	// Track whether any tool errored so we can exit non-zero.
	var sawToolError bool
	statusCh := busInst.Subscribe(64)
	if flagFormat == "json" {
		sid := ""
		if writer != nil {
			sid = writer.SessionID()
		}
		emitJSON(map[string]any{
			"type":    "session_start",
			"id":      sid,
			"model":   provider.Model,
			"cwd":     cwd,
			"resumed": resumed != nil,
		})
		go renderJSON(statusCh, &sawToolError)
	} else {
		go renderText(statusCh, &sawToolError)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Wire SIGINT/SIGTERM to cancel the in-flight turn cleanly.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		agt.Cancel()
	}()

	inputCh := make(chan string, 1)
	doneCh := make(chan error, 1)
	go func() { doneCh <- agt.Run(ctx, inputCh) }()

	// Send the single prompt and close the input channel so agent.Run will
	// exit after the loop quiesces.
	inputCh <- prompt
	close(inputCh)

	if err := <-doneCh; err != nil && !errors.Is(err, context.Canceled) {
		if flagFormat == "json" {
			emitJSON(map[string]any{"type": "session_end", "tool_errors": sawToolError, "error": err.Error()})
		}
		return err
	}

	if flagFormat == "json" {
		emitJSON(map[string]any{"type": "session_end", "tool_errors": sawToolError})
	}
	if sawToolError {
		return errors.New("one or more tool calls errored or were denied")
	}
	return nil
}

// writeJSON writes one JSON object plus a newline to w. We swallow encode
// errors silently — stdout closure / disk full is the only realistic cause
// and there's nowhere useful to report it.
func writeJSON(w io.Writer, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	b = append(b, '\n')
	_, _ = w.Write(b)
}

// emitJSON is the production wrapper that targets stdout.
func emitJSON(v any) { writeJSON(os.Stdout, v) }

// renderText is the human-readable bus consumer used by `enso run` (default).
// Mirrors the original inline goroutine: assistant deltas to stdout, tool
// activity / errors / compaction notes to stderr.
func renderText(ch <-chan bus.Event, sawToolError *bool) {
	for evt := range ch {
		switch evt.Type {
		case bus.EventAssistantDelta:
			if s, ok := evt.Payload.(string); ok {
				fmt.Print(s)
			}
		case bus.EventAssistantDone:
			fmt.Println()
		case bus.EventToolCallStart:
			if m, ok := evt.Payload.(map[string]any); ok {
				fmt.Fprintf(os.Stderr, "→ %v\n", m["name"])
			}
		case bus.EventToolCallEnd:
			if m, ok := evt.Payload.(map[string]any); ok {
				if e, _ := m["error"].(error); e != nil {
					*sawToolError = true
					fmt.Fprintf(os.Stderr, "  ✘ %v\n", e)
				}
				if denied, _ := m["denied"].(bool); denied {
					*sawToolError = true
				}
			}
		case bus.EventCompacted:
			fmt.Fprintln(os.Stderr, "(context compacted)")
		case bus.EventError:
			fmt.Fprintf(os.Stderr, "error: %v\n", evt.Payload)
		}
	}
}

// renderJSON emits one JSON object per bus event to stdout.
func renderJSON(ch <-chan bus.Event, sawToolError *bool) {
	renderJSONTo(os.Stdout, ch, sawToolError)
}

// renderJSONTo writes one JSON object per bus event to w. Schema is
// {"type": "<snake_case>", ...fields...}. Internal-only events
// (PermissionRequest/Response — they carry pointers to live channels) are
// skipped.
func renderJSONTo(w io.Writer, ch <-chan bus.Event, sawToolError *bool) {
	for evt := range ch {
		switch evt.Type {
		case bus.EventUserMessage:
			writeJSON(w, map[string]any{"type": "user_message", "content": evt.Payload})
		case bus.EventAssistantDelta:
			writeJSON(w, map[string]any{"type": "assistant_delta", "text": evt.Payload})
		case bus.EventReasoningDelta:
			writeJSON(w, map[string]any{"type": "reasoning_delta", "text": evt.Payload})
		case bus.EventAssistantDone:
			writeJSON(w, map[string]any{"type": "assistant_done"})
		case bus.EventToolCallStart:
			m, _ := evt.Payload.(map[string]any)
			out := map[string]any{"type": "tool_call_start"}
			for k, v := range m {
				out[k] = v
			}
			writeJSON(w, out)
		case bus.EventToolCallEnd:
			m, _ := evt.Payload.(map[string]any)
			out := map[string]any{"type": "tool_call_end"}
			for k, v := range m {
				if k == "error" {
					if e, ok := v.(error); ok && e != nil {
						out["error"] = e.Error()
						*sawToolError = true
						continue
					}
					out["error"] = nil
					continue
				}
				out[k] = v
			}
			if denied, _ := out["denied"].(bool); denied {
				*sawToolError = true
			}
			writeJSON(w, out)
		case bus.EventCompacted:
			m, _ := evt.Payload.(map[string]any)
			out := map[string]any{"type": "compacted"}
			for k, v := range m {
				out[k] = v
			}
			writeJSON(w, out)
		case bus.EventAgentStart:
			m, _ := evt.Payload.(map[string]any)
			out := map[string]any{"type": "agent_start"}
			for k, v := range m {
				out[k] = v
			}
			writeJSON(w, out)
		case bus.EventAgentEnd:
			m, _ := evt.Payload.(map[string]any)
			out := map[string]any{"type": "agent_end"}
			for k, v := range m {
				out[k] = v
			}
			writeJSON(w, out)
		case bus.EventCancelled:
			writeJSON(w, map[string]any{"type": "cancelled"})
		case bus.EventError:
			msg := fmt.Sprintf("%v", evt.Payload)
			writeJSON(w, map[string]any{"type": "error", "message": msg})
		}
	}
}

// runDetached connects to the daemon, submits the prompt, prints the session
// id, and exits. The daemon keeps the agent running; attach with
// `enso attach <id>`.
func runDetached(argParts []string) error {
	prompt, err := resolvePrompt(argParts)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	c, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("daemon not reachable (start with `enso daemon`): %w", err)
	}
	defer c.Close()

	// Default to yolo for fire-and-forget submissions: with --detach there's
	// no attached UI to prompt by default, so without yolo the daemon would
	// deny every tool call after a 60s timeout. Users who want gated tools
	// should `enso daemon` + `enso attach <id>` instead.
	info, err := c.CreateSession(daemon.CreateSessionReq{
		Prompt:   prompt,
		Cwd:      cwd,
		Yolo:     true,
		MaxTurns: flagMaxTurns,
	})
	if err != nil {
		return err
	}
	fmt.Println(info.ID)
	return nil
}

// runWorkflow loads a named workflow and runs it non-interactively, streaming
// each role's output to stdout.
func runWorkflow(name string, argParts []string) error {
	args := strings.Join(argParts, " ")

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get cwd: %w", err)
	}
	cfg, err := loadOrWelcome(cwd)
	if err != nil {
		if errors.Is(err, errFirstRunWelcome) {
			return err
		}
		return fmt.Errorf("load config: %w", err)
	}
	wfProviders, err := llm.BuildProviders(cfg.Providers)
	if err != nil {
		return err
	}
	wfDefault, err := pickDefaultProviderName(wfProviders, cfg.DefaultProvider)
	if err != nil {
		return err
	}

	wf, err := workflow.LoadByName(cwd, name)
	if err != nil {
		return err
	}

	checker := permissions.NewChecker(cfg.Permissions.Allow, cfg.Permissions.Ask, cfg.Permissions.Deny, cfg.Permissions.Mode)
	if flagYolo {
		checker.SetYolo(true)
	}
	var wfRestrictedRoots []string
	if !cfg.Permissions.DisableFileConfinement {
		wfRestrictedRoots = append([]string{cwd}, cfg.Permissions.AdditionalDirectories...)
	}
	registry := tools.BuildDefault()
	agent.RegisterSpawn(registry)
	tools.RegisterSearch(registry, cfg.Search)

	mcpMgr := mcp.NewManager()
	if len(cfg.MCP) > 0 {
		mcpMgr.Start(context.Background(), cfg.MCP)
		mcpMgr.RegisterAll(registry)
	}
	defer mcpMgr.Close()

	busInst := bus.New()
	// Stream assistant deltas + agent transitions to stderr/stdout.
	stream := busInst.Subscribe(64)
	go func() {
		for evt := range stream {
			switch evt.Type {
			case bus.EventAssistantDelta:
				if s, ok := evt.Payload.(string); ok {
					fmt.Print(s)
				}
			case bus.EventAssistantDone:
				fmt.Println()
			case bus.EventAgentStart:
				if m, ok := evt.Payload.(map[string]any); ok {
					fmt.Fprintf(os.Stderr, "▶ %v\n", m["role"])
				}
			case bus.EventAgentEnd:
				if m, ok := evt.Payload.(map[string]any); ok {
					if e, _ := m["error"].(string); e != "" {
						fmt.Fprintf(os.Stderr, "✘ %v: %s\n", m["role"], e)
					} else {
						fmt.Fprintf(os.Stderr, "✓ %v\n", m["role"])
					}
				}
			}
		}
	}()

	// Auto-deny permission prompts in non-interactive mode (use --yolo).
	permCh := busInst.Subscribe(8)
	go func() {
		for evt := range permCh {
			if evt.Type != bus.EventPermissionRequest {
				continue
			}
			if req, ok := evt.Payload.(*permissions.PromptRequest); ok {
				fmt.Fprintf(os.Stderr, "[denied] %s (use --yolo)\n", req.ToolName)
				req.Respond <- permissions.Deny
			}
		}
	}()

	deps := workflow.RunDeps{
		Providers:          wfProviders,
		DefaultProvider:    wfDefault,
		Bus:                busInst,
		Registry:           registry,
		Perms:              checker,
		Cwd:                cwd,
		MaxTurns:           flagMaxTurns,
		GlobalAgents:       &atomic.Int64{},
		GitAttribution:     cfg.Git.Attribution,
		GitAttributionName: cfg.Git.AttributionName,
		WebFetchAllowHosts: cfg.WebFetch.AllowHosts,
		RestrictedRoots:    wfRestrictedRoots,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	res, err := workflow.Run(ctx, wf, args, deps)
	if err != nil {
		return err
	}
	// Final summary on stdout (deltas already flushed inline).
	if res.Last != "" && len(wf.RoleOrder) > 0 {
		// Re-emit the last role's output as a single block so callers can
		// pipe it cleanly even if intermediate roles printed to stderr.
		_ = res.Last
	}
	return nil
}

// resolvePrompt returns the prompt from CLI args (joined) or stdin if no args
// were given.
func resolvePrompt(args []string) (string, error) {
	if len(args) > 0 {
		return strings.Join(args, " "), nil
	}
	stat, err := os.Stdin.Stat()
	if err != nil {
		return "", fmt.Errorf("stat stdin: %w", err)
	}
	if (stat.Mode() & os.ModeCharDevice) != 0 {
		return "", errors.New("usage: enso run \"<prompt>\" (or pipe a prompt on stdin)")
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return "", errors.New("stdin was empty")
	}
	return prompt, nil
}
