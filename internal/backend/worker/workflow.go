// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/wire"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/tools"
	"github.com/TaraTheStar/enso/internal/workflow"
)

// workflowEnv is the slice of the worker's agent construction that the
// CtrlRunWorkflow handler needs to run the declarative workflow engine
// worker-side. Captured once in RunAgent (before the demux starts, so
// no locking is needed to read it) — the SAME registry, enforcing
// checker, bus and session writer the interactive agent uses, which is
// exactly what makes workflow tool calls execute inside the box, their
// permission prompts ride the seam proxy, and their persistence land in
// the host DB via the remoteWriter on isolated backends.
type workflowEnv struct {
	providers          map[string]*llm.Provider
	defaultProvider    string
	bus                *bus.Bus
	registry           *tools.Registry
	perms              *permissions.Checker
	cwd                string
	maxTurns           int
	writer             tools.SessionWriter
	gitAttribution     string
	gitAttributionName string
	webFetchAllowHosts []string
	toolTimeouts       tools.ToolTimeouts
	restrictedRoots    []string
	capabilities       tools.CapabilityRequester
	isolationNote      string
}

// serveRunWorkflow executes one CtrlRunWorkflow request: re-parse the
// shipped definition (raw markdown — the worker never reads a host
// file; host and worker binaries are identical so the parse cannot
// drift), build RunDeps from the worker's live agent stack, and run the
// engine to completion. Runs on serveControl's per-request goroutine,
// so a minutes-long workflow never blocks the Channel demux; its
// inference round-trips through the same demux it was dispatched from.
func (s *seam) serveRunWorkflow(ctx context.Context, args json.RawMessage) wire.ControlResponse {
	var resp wire.ControlResponse
	env := s.wfEnv
	if env == nil {
		resp.Error = "workflow environment not ready"
		return resp
	}
	var wr wire.WorkflowRun
	if err := json.Unmarshal(args, &wr); err != nil {
		resp.Error = fmt.Sprintf("decode workflow run: %v", err)
		return resp
	}
	wf, err := workflow.Parse(wr.Name+".md", wr.Definition)
	if err != nil {
		resp.Error = err.Error()
		return resp
	}

	// MsgCancel (the host's Ctrl-C equivalent) aborts in-flight
	// workflow runs alongside the interactive turn.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	id := s.registerWfCancel(cancel)
	defer s.dropWfCancel(id)

	deps := workflow.RunDeps{
		Providers:          env.providers,
		DefaultProvider:    env.defaultProvider,
		Bus:                env.bus,
		Registry:           env.registry,
		Perms:              env.perms,
		Cwd:                env.cwd,
		MaxTurns:           env.maxTurns,
		GlobalAgents:       &atomic.Int64{}, // per-run budget (agent.New resolves Max* defaults)
		Writer:             env.writer,
		GitAttribution:     env.gitAttribution,
		GitAttributionName: env.gitAttributionName,
		WebFetchAllowHosts: env.webFetchAllowHosts,
		ToolTimeouts:       env.toolTimeouts,
		RestrictedRoots:    env.restrictedRoots,
		Capabilities:       env.capabilities,
		IsolationNote:      env.isolationNote,
	}

	res, err := workflow.Run(runCtx, wf, wr.Args, deps)
	if err != nil {
		resp.Error = err.Error()
		return resp
	}
	resp.Result, _ = backend.NewBody(wire.WorkflowResult{
		Outputs:   res.Outputs,
		Fields:    res.Fields,
		Skipped:   res.Skipped,
		Last:      res.Last,
		RoleOrder: wf.RoleOrder,
	})
	return resp
}

func (s *seam) registerWfCancel(cancel context.CancelFunc) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wfSeq++
	id := s.wfSeq
	s.wfCancels[id] = cancel
	return id
}

func (s *seam) dropWfCancel(id uint64) {
	s.mu.Lock()
	delete(s.wfCancels, id)
	s.mu.Unlock()
}

// cancelWorkflows aborts every in-flight workflow run (MsgCancel).
func (s *seam) cancelWorkflows() {
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.wfCancels))
	for _, c := range s.wfCancels {
		cancels = append(cancels, c)
	}
	s.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}
