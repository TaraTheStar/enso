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
	"strings"
	"syscall"

	"sync/atomic"

	"github.com/google/uuid"

	"github.com/TaraTheStar/enso/internal/daemon"

	"github.com/TaraTheStar/enso/internal/agent"
	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/host"
	"github.com/TaraTheStar/enso/internal/backend/workspace"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/mcp"
	"github.com/TaraTheStar/enso/internal/permissions"
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

	providers, err := llm.BuildProviders(cfg.Providers, cfg.ResolvePools())
	if err != nil {
		return err
	}
	for _, p := range providers {
		p.IncludeProviders = cfg.Instructions.ProvidersIncluded()
	}
	requested := cfg.DefaultProvider
	if flagProvider != "" {
		requested = flagProvider
	}
	defaultName, err := pickDefaultProviderName(providers, requested)
	if err != nil {
		return err
	}
	// Exactly one execution path: the agent core always runs as a
	// Worker behind a Backend (LocalBackend = sandbox off,
	// PodmanBackend = sandbox on). No in-process branch — that is the
	// structural fix the whole effort exists for.
	b, isol, bopts := host.SelectBackend(cfg)
	return runViaBackend(b, isol, bopts, prompt, cwd, cfg, providers, defaultName)
}

// runViaBackend is the Backend-seam implementation of `enso run`
// for the default (sandbox-off) path. The host stays thin: it owns the
// REAL providers (endpoints/keys/pools) and the rendering/permission/
// signal wiring; the agent core (model loop + tools + session store +
// mcp/lsp/agents recipe) runs in the worker child process. The host
// adapter republishes the worker's bus events onto busInst, so the
// existing renderText/renderJSON/permission goroutines work unchanged.
//
// Behavior parity with the old in-process path: same stdout/stderr,
// same exit code (non-zero on tool error / cancel), same --format json
// envelope. The only host-visible shift is that the session id is now
// minted host-side (so the json header can name the run before the
// worker is ready); the worker, which owns the store, inserts the row
// under that id.
func runViaBackend(b backend.Backend, isol backend.IsolationSpec, bopts []host.Option, prompt, cwd string, cfg *config.Config, providers map[string]*llm.Provider, defaultName string) error {
	// Workspace overlay: run the agent against a throwaway copy
	// bind-mounted at the real path inside the box. `enso run` is
	// non-interactive, so at task end Resolve keeps the diverged copy
	// and prints how to apply it — never silently commits or destroys
	// the agent's work. Backend-specific wiring lives in one helper.
	if ov, err := host.SetupWorkspaceOverlay(context.Background(), b, cfg, cwd, os.Stderr); err != nil {
		return fmt.Errorf("workspace: %w", err)
	} else if ov != nil {
		defer func() { _ = workspace.Resolve(context.Background(), ov, false, nil, os.Stderr) }()
	}

	resuming := flagSession != ""
	sessionID := ""
	if resuming {
		sessionID = flagSession
	} else if !flagEphemeral {
		sessionID = uuid.NewString()
	}

	spec := backend.TaskSpec{
		TaskID:          uuid.NewString(),
		Cwd:             cwd,
		Prompt:          prompt,
		Interactive:     false,
		Ephemeral:       flagEphemeral,
		MaxTurns:        flagMaxTurns,
		Yolo:            flagYolo,
		AgentProfile:    flagAgent,
		Providers:       host.ProviderCatalog(providers),
		DefaultProvider: defaultName,
		Isolation:       isol,
	}
	if resuming {
		spec.ResumeSessionID = flagSession
	} else {
		spec.SessionID = sessionID // empty when --ephemeral; worker skips the store
	}
	// Credential-scrub invariant: the SCRUBBED config crosses the seam,
	// never the raw one. The worker rebuilds providers from
	// spec.Providers + the host-proxied client; it gets no endpoint/key.
	rc, err := json.Marshal(cfg.ScrubbedForWorker())
	if err != nil {
		return fmt.Errorf("serialize config: %w", err)
	}
	spec.ResolvedConfig = rc

	// Host owns session-row creation (mirrors the legacy in-process
	// runOnce, which created the row host-side): the worker only
	// attaches a message writer to it. This keeps the row visible
	// host-side immediately and avoids racing the worker's startup.
	if !flagEphemeral && !resuming {
		store, err := session.Open()
		if err != nil {
			return fmt.Errorf("open session store: %w", err)
		}
		if _, err := session.NewSessionWithID(store, sessionID, providers[defaultName].Model, defaultName, cwd); err != nil {
			store.Close()
			return fmt.Errorf("create session: %w", err)
		}
		store.Close()
	}

	busInst := bus.New()

	// In non-interactive mode a permission prompt has no UI. Auto-deny
	// (unchanged from the in-process path: it subscribes to the same
	// bus the host adapter republishes the worker's prompts onto). Run
	// with --yolo to bypass.
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

	var sawToolError bool
	statusCh := busInst.Subscribe(64)
	if flagFormat == "json" {
		go renderJSON(statusCh, &sawToolError)
	} else {
		go renderText(statusCh, &sawToolError)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, err := host.Start(ctx, b, spec, providers, busInst, bopts...)
	if err != nil {
		if flagFormat == "json" {
			emitJSON(map[string]any{"type": "session_end", "tool_errors": sawToolError, "error": err.Error()})
		}
		return err
	}
	defer sess.Close()

	// session_start mirrors the old emit (after the agent is
	// constructed, before the first turn). host.Start has returned, so
	// the worker is ready but no bus event has flowed yet — this stays
	// the first stdout line. Model is the host-side default-provider
	// model (identical to the old path for the common no-profile case;
	// an --agent profile that overrides the model is resolved
	// worker-side and only this informational header would differ).
	if flagFormat == "json" {
		emitJSON(map[string]any{
			"type":    "session_start",
			"id":      sessionID,
			"model":   providers[defaultName].Model,
			"cwd":     cwd,
			"resumed": resuming,
		})
	}

	// SIGINT/SIGTERM cancels the in-flight turn cleanly (Ctrl-C
	// equivalent), now routed over the Channel as MsgCancel.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		sess.Cancel()
	}()

	werr := sess.Wait()
	cancel()
	busInst.Close() // ends the render goroutine's range, bounded

	if werr != nil && !errors.Is(werr, context.Canceled) {
		if flagFormat == "json" {
			emitJSON(map[string]any{"type": "session_end", "tool_errors": sawToolError, "error": werr.Error()})
		}
		return werr
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
	wfProviders, err := llm.BuildProviders(cfg.Providers, cfg.ResolvePools())
	if err != nil {
		return err
	}
	for _, p := range wfProviders {
		p.IncludeProviders = cfg.Instructions.ProvidersIncluded()
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
