// SPDX-License-Identifier: AGPL-3.0-or-later

package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"

	"github.com/TaraTheStar/enso/internal/agent"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/provider"
	"github.com/TaraTheStar/enso/internal/tools"
)

// RunDeps is what the runner needs from the surrounding agent stack.
type RunDeps struct {
	// Providers is the full set of configured LLM endpoints, keyed by
	// the user-facing label (e.g. "qwen-fast"). Per-role `model:` in
	// the YAML names a key here. Must be non-empty.
	Providers map[string]*provider.Provider
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
	Writer             tools.SessionWriter // optional; persists per-role messages with role's agent id
	GitAttribution     string
	GitAttributionName string
	WebFetchAllowHosts []string
	ToolTimeouts       tools.ToolTimeouts // bash execution budget, forwarded to each role agent
	RestrictedRoots    []string

	// Capabilities is the tier-3 broker handle. Non-nil only when the
	// engine runs inside a network-sealed worker, so role agents broker
	// web egress exactly like the interactive agent (nil keeps today's
	// direct-dial behavior on the local backend).
	Capabilities tools.CapabilityRequester
	// IsolationNote is the honest # Environment sentence describing the
	// box the roles run in, forwarded verbatim to each role agent.
	IsolationNote string
}

// Result is the outcome of a workflow run.
type Result struct {
	Outputs map[string]string            // role name → final assistant text ("" for skipped)
	Fields  map[string]map[string]string // role name → parsed structured fields
	Skipped map[string]bool              // role name → true if a conditional edge skipped it
	Last    string                       // output of the last non-skipped role in topological order
}

// roleOutput is the per-role record stored during a run. Fields is built once
// by parseStructured when the role completes and is never mutated afterwards,
// so a shallow struct copy in the snapshot path is safe.
type roleOutput struct {
	Text    string            // raw final assistant text -> .<role>.output
	Fields  map[string]string // parsed structured fields  -> .<role>.<field>
	Skipped bool              // true if a conditional edge skipped it -> .<role>.skipped
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
	// Each node tracks how many incoming edges are still unresolved
	// (remaining) and how many resolved "dead" — i.e. did not fire, either
	// because their `if` guard was false or because the source role was
	// itself skipped. With strict-AND join (no join modes), a node runs iff
	// every incoming edge fired (dead==0); any dead edge skips it.
	type nodeState struct {
		remaining int
		dead      int
	}
	state := map[string]*nodeState{}
	for name := range wf.Roles {
		state[name] = &nodeState{}
	}
	outEdges := map[string][]Edge{} // role -> its outgoing edges (Cond preserved)
	for _, e := range wf.Edges {
		outEdges[e.From] = append(outEdges[e.From], e)
		state[e.To].remaining++
	}

	outputs := map[string]roleOutput{}
	skipped := map[string]bool{}
	var outMu sync.Mutex

	type roleDone struct {
		role string
		err  error
	}
	doneCh := make(chan roleDone, len(wf.Roles))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	launched := 0
	launch := func(name string) {
		wg.Add(1)
		launched++
		go func() {
			defer wg.Done()
			err := runRole(runCtx, name, wf.Roles[name], args, &outMu, outputs, deps)
			doneCh <- roleDone{role: name, err: err}
		}()
	}

	// edgesOf returns a role's outgoing edges in a deterministic (To-sorted)
	// order so launches and skip cascades are reproducible.
	edgesOf := func(role string) []Edge {
		es := append([]Edge(nil), outEdges[role]...)
		sort.Slice(es, func(i, j int) bool { return es[i].To < es[j].To })
		return es
	}

	// markSkipped records a role as skipped and cascades: a skipped role's
	// outgoing edges all resolve "dead", which may skip its dependents in
	// turn. Skips never spawn goroutines, so the completed<launched
	// invariant below is untouched. All of this runs single-threaded in the
	// drain loop.
	var markSkipped func(role string)
	resolveDead := func(dep string) {
		st := state[dep]
		st.remaining--
		st.dead++
		if st.remaining == 0 {
			// dead>0 by construction here, so the node is always skipped.
			markSkipped(dep)
		}
	}
	markSkipped = func(role string) {
		outMu.Lock()
		outputs[role] = roleOutput{Skipped: true}
		outMu.Unlock()
		skipped[role] = true
		for _, e := range edgesOf(role) {
			resolveDead(e.To)
		}
	}

	// Initial ready set: roles with no incoming edge. Sort for stable
	// ordering of bus events when concurrency=1.
	ready := []string{}
	for name, st := range state {
		if st.remaining == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)
	for _, name := range ready {
		launch(name)
	}

	// Loop drains completions until every launched role has finished.
	// We do NOT wait for `len(wf.Roles)`: roles whose incoming edges did
	// not all fire are *skipped* (never launched, resolved synchronously
	// here), and a failed role's dependents are never launched either
	// (cancel() prevents activation). Counting against `launched` lets the
	// runner drain exactly the goroutines in flight and then return.
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
		// Snapshot outputs once for resolving all of this role's outgoing
		// edge predicates. Fields is immutable post-construction, so a
		// shallow copy under the lock is safe.
		outMu.Lock()
		snap := make(map[string]roleOutput, len(outputs))
		for k, v := range outputs {
			snap[k] = v
		}
		outMu.Unlock()
		data := buildData(args, snap)

		for _, e := range edgesOf(d.role) {
			fired := evalCond(e.Cond, data, deps.Bus)
			st := state[e.To]
			st.remaining--
			if !fired {
				st.dead++
			}
			if st.remaining == 0 {
				if st.dead == 0 {
					launch(e.To)
				} else {
					markSkipped(e.To) // strict AND: any dead edge => skip
				}
			}
		}
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}

	// Project the rich per-role records back to the back-compat shapes:
	// Outputs (raw text, "" for skipped) and Fields (structured).
	res := &Result{
		Outputs: make(map[string]string, len(outputs)),
		Fields:  make(map[string]map[string]string, len(outputs)),
		Skipped: skipped,
	}
	for k, rec := range outputs {
		res.Outputs[k] = rec.Text
		res.Fields[k] = rec.Fields
	}
	// Last = the last role in topological order that actually ran. A skipped
	// tail (e.g. an un-taken branch) must not become the reported result.
	for i := len(wf.RoleOrder) - 1; i >= 0; i-- {
		if r := wf.RoleOrder[i]; !skipped[r] {
			res.Last = outputs[r].Text
			break
		}
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
	outputs map[string]roleOutput,
	deps RunDeps,
) error {
	// Snapshot outputs for prompt rendering. By construction the role
	// can only launch after every dependency has populated outputs, so
	// this snapshot has everything the template needs.
	outMu.Lock()
	snapshot := make(map[string]roleOutput, len(outputs))
	for k, v := range outputs {
		snapshot[k] = v // shallow copy ok: Fields is immutable post-construction
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
		Writer:             deps.Writer,
		GitAttribution:     deps.GitAttribution,
		GitAttributionName: deps.GitAttributionName,
		WebFetchAllowHosts: deps.WebFetchAllowHosts,
		ToolTimeouts:       deps.ToolTimeouts,
		RestrictedRoots:    deps.RestrictedRoots,
		Capabilities:       deps.Capabilities,
		IsolationNote:      deps.IsolationNote,
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

	endPayload := map[string]any{"id": agentID, "parent_id": "", "role": name}
	if runErr != nil {
		endPayload["error"] = runErr.Error()
	}
	deps.Bus.Publish(bus.Event{Type: bus.EventAgentEnd, Payload: endPayload})

	if runErr != nil {
		return fmt.Errorf("role %q: run: %w", name, runErr)
	}

	outMu.Lock()
	outputs[name] = roleOutput{Text: text, Fields: parseStructured(text)}
	outMu.Unlock()
	return nil
}

// renderPrompt executes the role's template against the shared data context
// (`{ .Args, .<role>.output, .<role>.<field> }`) built by buildData.
func renderPrompt(tmpl *template.Template, args string, outputs map[string]roleOutput) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, buildData(args, outputs)); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// buildData is the single source of truth for the `{ .Args, .<role>.* }`
// template/predicate context. Reserved keys (output/skipped) are written AFTER
// the role's structured Fields so they win if a role emits a same-named field —
// `.<role>.output` therefore always means "raw text". Authors should avoid
// emitting fields named "output" or "skipped".
func buildData(args string, outputs map[string]roleOutput) map[string]any {
	data := map[string]any{"Args": args}
	for name, rec := range outputs {
		m := make(map[string]any, len(rec.Fields)+2)
		for k, v := range rec.Fields { // fields first…
			m[k] = v
		}
		m["output"] = rec.Text // …reserved keys override.
		m["skipped"] = rec.Skipped
		data[name] = m
	}
	return data
}

var jsonFenceRe = regexp.MustCompile("(?s)```json[ \t]*\r?\n(.*?)```")

var kvLineRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*):[ \t]*(.*)$`)

// parseStructured extracts named fields from a role's final assistant text.
// Source precedence: (1) the LAST ```json fenced block, parsed as a flat
// object; (2) failing that, contiguous trailing `KEY: value` lines. A malformed
// last JSON block yields empty fields (no KV fallback). `.output` (the raw text)
// is always available regardless, so callers stay backward compatible.
func parseStructured(text string) map[string]string {
	if blocks := jsonFenceRe.FindAllStringSubmatch(text, -1); len(blocks) > 0 {
		var raw map[string]any
		if err := json.Unmarshal([]byte(blocks[len(blocks)-1][1]), &raw); err != nil {
			return map[string]string{} // malformed last block => no fields
		}
		out := make(map[string]string, len(raw))
		for k, v := range raw {
			out[k] = stringifyJSON(v)
		}
		return out
	}
	return trailingKeyValues(text)
}

// stringifyJSON renders a decoded JSON scalar/value as the string a template
// will see. Integral numbers render without a trailing ".0"; bools as
// true/false; nested objects/arrays as compact JSON.
func stringifyJSON(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		if b, err := json.Marshal(t); err == nil {
			return string(b)
		}
		return fmt.Sprint(t)
	}
}

// trailingKeyValues scans contiguous `KEY: value` lines at the end of text.
// Scanning stops at the first non-matching or blank line, so prose above the
// block is ignored.
func trailingKeyValues(text string) map[string]string {
	lines := strings.Split(strings.TrimRight(text, "\n \t"), "\n")
	out := map[string]string{}
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimRight(lines[i], " \t\r")
		if strings.TrimSpace(line) == "" {
			break
		}
		m := kvLineRe.FindStringSubmatch(line)
		if m == nil {
			break
		}
		out[m[1]] = strings.TrimSpace(m[2])
	}
	return out
}

// providerNames returns the configured provider keys in stable order
// for inclusion in error messages.
func providerNames(providers map[string]*provider.Provider) []string {
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
