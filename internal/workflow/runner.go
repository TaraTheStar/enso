// SPDX-License-Identifier: AGPL-3.0-or-later

package workflow

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"text/template"

	"github.com/TaraTheStar/enso/internal/agent"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/session"
	"github.com/TaraTheStar/enso/internal/tools"
)

// RunDeps is what the runner needs from the surrounding agent stack.
type RunDeps struct {
	// Providers is the full set of configured LLM endpoints, keyed by
	// the user-facing label (e.g. "qwen-fast"). Per-role `model:` in
	// the YAML names a key here. Must be non-empty.
	Providers map[string]*llm.Provider
	// DefaultProvider is the provider name a role falls back to when
	// its `model:` field is empty. Empty string = alphabetical-first.
	DefaultProvider string

	Bus                *bus.Bus
	Registry           *tools.Registry
	Perms              *permissions.Checker
	Cwd                string
	MaxTurns           int
	GlobalAgents       *atomic.Int64
	MaxAgents          int
	MaxDepth           int
	Depth              int
	Transcripts        *tools.Transcripts // optional; captures per-role histories
	Writer             *session.Writer    // optional; persists per-role messages with role's agent id
	GitAttribution     string
	GitAttributionName string
	WebFetchAllowHosts []string
	RestrictedRoots    []string
}

// Result is the outcome of a workflow run.
type Result struct {
	Outputs map[string]string // role name → final assistant text
	Last    string            // output of the last role in topological order
}

// Run executes the workflow with sibling parallelism. Roles whose
// dependencies have all completed launch concurrently; their child agents
// share the parent Provider's Pool, which is what actually serialises
// inflight LLM calls when the configured concurrency is 1.
//
// On the first role error the run-context is cancelled and the runner
// drains. The first error is returned; later errors are logged.
func Run(ctx context.Context, wf *Workflow, args string, deps RunDeps) (*Result, error) {
	if wf == nil {
		return nil, fmt.Errorf("nil workflow")
	}
	if len(wf.Roles) == 0 {
		return &Result{Outputs: map[string]string{}}, nil
	}

	// Build dependency graph state. parse.go has already rejected cycles.
	indeg := map[string]int{}
	for name := range wf.Roles {
		indeg[name] = 0
	}
	deps2 := map[string][]string{} // role -> roles that depend on it
	for _, e := range wf.Edges {
		deps2[e.From] = append(deps2[e.From], e.To)
		indeg[e.To]++
	}

	outputs := map[string]string{}
	var outMu sync.Mutex

	type roleDone struct {
		role string
		err  error
	}
	doneCh := make(chan roleDone, len(wf.Roles))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	launch := func(name string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := runRole(runCtx, name, wf.Roles[name], args, &outMu, outputs, deps)
			doneCh <- roleDone{role: name, err: err}
		}()
	}

	// Initial ready set: roles with no incoming edge. Sort for stable
	// ordering of bus events when concurrency=1.
	ready := []string{}
	for name, d := range indeg {
		if d == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)
	launched := 0
	for _, name := range ready {
		launch(name)
		launched++
	}

	// Loop drains completions until every launched role has finished.
	// We do NOT wait for `len(wf.Roles)`: when a role fails, its
	// dependents are never launched (cancel() prevents activation), so
	// they never publish a doneCh entry — counting against `total`
	// would deadlock. Counting against `launched` lets the runner
	// drain whatever's actually in flight and then return.
	var firstErr error
	completed := 0
	for completed < launched {
		d := <-doneCh
		completed++
		if d.err != nil {
			if firstErr == nil {
				firstErr = d.err
				cancel() // signal still-running roles to abort
			}
			continue
		}
		// Activate dependents whose in-degree just hit zero. Sort the
		// list so launches are deterministic.
		dependents := append([]string(nil), deps2[d.role]...)
		sort.Strings(dependents)
		for _, dep := range dependents {
			indeg[dep]--
			if indeg[dep] == 0 {
				launch(dep)
				launched++
			}
		}
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}

	res := &Result{Outputs: outputs}
	if len(wf.RoleOrder) > 0 {
		last := wf.RoleOrder[len(wf.RoleOrder)-1]
		res.Last = outputs[last]
	}
	return res, nil
}

// runRole executes a single role end-to-end: render prompt against the
// current outputs snapshot, build a child agent, run it to quiescence,
// publish AgentStart/End, and store its output. Returns a wrapped error
// on any failure.
func runRole(
	ctx context.Context,
	name string,
	role Role,
	args string,
	outMu *sync.Mutex,
	outputs map[string]string,
	deps RunDeps,
) error {
	// Snapshot outputs for prompt rendering. By construction the role
	// can only launch after every dependency has populated outputs, so
	// this snapshot has everything the template needs.
	outMu.Lock()
	snapshot := make(map[string]string, len(outputs))
	for k, v := range outputs {
		snapshot[k] = v
	}
	outMu.Unlock()

	prompt, err := renderPrompt(role.PromptTemplate, args, snapshot)
	if err != nil {
		return fmt.Errorf("role %q: render: %w", name, err)
	}

	childRegistry := deps.Registry
	if len(role.AllowedTools) > 0 {
		childRegistry = deps.Registry.Filter(role.AllowedTools)
	}

	agentID := name + ":" + shortID()

	// Per-role provider: the YAML's `model:` field is interpreted as a
	// provider-name lookup against deps.Providers. Empty falls back
	// to deps.DefaultProvider. Unknown is an error before any work
	// starts so the user sees the typo cleanly.
	roleDefault := deps.DefaultProvider
	if role.Model != "" {
		if _, ok := deps.Providers[role.Model]; !ok {
			return fmt.Errorf("role %q: model %q not in [providers] (configured: %v)",
				name, role.Model, providerNames(deps.Providers))
		}
		roleDefault = role.Model
	}

	child, err := agent.New(agent.Config{
		Providers:          deps.Providers,
		DefaultProvider:    roleDefault,
		Bus:                deps.Bus,
		Registry:           childRegistry,
		Perms:              deps.Perms,
		Cwd:                deps.Cwd,
		MaxTurns:           deps.MaxTurns,
		Depth:              deps.Depth + 1,
		MaxDepth:           deps.MaxDepth,
		MaxAgents:          deps.MaxAgents,
		GlobalAgents:       deps.GlobalAgents,
		AgentID:            agentID,
		AgentRole:          name,
		Transcripts:        deps.Transcripts,
		Writer:             deps.Writer,
		GitAttribution:     deps.GitAttribution,
		GitAttributionName: deps.GitAttributionName,
		WebFetchAllowHosts: deps.WebFetchAllowHosts,
		RestrictedRoots:    deps.RestrictedRoots,
	})
	if err != nil {
		return fmt.Errorf("role %q: build: %w", name, err)
	}

	// Workflow roles have no parent agent — they're invoked from the slash
	// command, so parent_id stays empty (root-level in the agents tree).
	deps.Bus.Publish(bus.Event{
		Type: bus.EventAgentStart,
		Payload: map[string]any{
			"id":        agentID,
			"parent_id": "",
			"depth":     deps.Depth + 1,
			"prompt":    truncate(prompt, 80),
			"role":      name,
		},
	})

	text, runErr := child.RunOneShot(ctx, prompt)

	// Capture transcript for click-to-expand in agents pane.
	deps.Transcripts.Store(agentID, child.History)

	endPayload := map[string]any{"id": agentID, "parent_id": "", "role": name}
	if runErr != nil {
		endPayload["error"] = runErr.Error()
	}
	deps.Bus.Publish(bus.Event{Type: bus.EventAgentEnd, Payload: endPayload})

	if runErr != nil {
		return fmt.Errorf("role %q: run: %w", name, runErr)
	}

	outMu.Lock()
	outputs[name] = text
	outMu.Unlock()
	return nil
}

// renderPrompt executes the role's template against `{ Args, <role>: {output} }`
// for every role that has already produced an output.
func renderPrompt(tmpl *template.Template, args string, outputs map[string]string) (string, error) {
	data := map[string]any{"Args": args}
	for name, text := range outputs {
		data[name] = map[string]any{"output": text}
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// providerNames returns the configured provider keys in stable order
// for inclusion in error messages.
func providerNames(providers map[string]*llm.Provider) []string {
	out := make([]string, 0, len(providers))
	for name := range providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// shortID returns an 8-char id derived from a process-wide counter. Collisions
// across separate workflow runs are unlikely in practice and inconsequential.
func shortID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	n := idCounter.Add(1)
	var b [8]byte
	for i := range b {
		b[i] = alphabet[int(n>>(uint(i)*4))&0x1f%len(alphabet)]
	}
	return string(b[:])
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

var idCounter atomic.Int64
