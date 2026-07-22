// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/TaraTheStar/azoth/llm"
	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/wire"
	"github.com/TaraTheStar/enso/internal/config"
)

// TestRunAgentWorkflowOverSeam proves CtrlRunWorkflow runs the whole
// declarative workflow engine WORKER-side: the definition crosses the
// seam as raw bytes (no worker filesystem read), each role's inference
// is host-proxied with the role's provider stamped per request, role
// AgentStart/AgentEnd events surface as MsgEvent, and the result comes
// back on the control response — all over one Channel, no host registry.
func TestRunAgentWorkflowOverSeam(t *testing.T) {
	workerCh, hostCh := newChannelPair()

	cfg := &config.Config{}
	cfg.Permissions.Mode = "allow"
	rc, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	spec := backend.TaskSpec{
		TaskID:          "wf1",
		Cwd:             t.TempDir(),
		Interactive:     true, // workflow rides the control leg; no prompt
		Ephemeral:       true,
		ResolvedConfig:  rc,
		Providers:       []backend.ProviderInfo{{Name: "test", Model: "m"}},
		DefaultProvider: "test",
	}

	const def = `---
roles:
  alpha:
    tools: []
  beta:
    tools: []
edges:
  - alpha -> beta
---

## alpha

Say something about {{ .Args }}.

## beta

Summarize: {{ .alpha.output }}
`

	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), workerCh, RunAgent) }()

	specBody, _ := backend.NewBody(spec)
	if err := hostCh.Send(backend.Envelope{Kind: backend.MsgTaskSpec, Body: specBody}); err != nil {
		t.Fatalf("send spec: %v", err)
	}

	var (
		starts, ends int
		inferCount   int
		result       wire.WorkflowResult
		gotResult    bool
		workerDone   bool
	)
	deadline := time.After(15 * time.Second)
	for !workerDone {
		type recvd struct {
			env backend.Envelope
			err error
		}
		rcv := make(chan recvd, 1)
		go func() { e, err := hostCh.Recv(); rcv <- recvd{e, err} }()

		select {
		case <-deadline:
			t.Fatalf("timed out (starts=%d ends=%d infer=%d gotResult=%v)", starts, ends, inferCount, gotResult)
		case got := <-rcv:
			if got.err != nil {
				t.Fatalf("host recv: %v", got.err)
			}
			switch got.env.Kind {
			case backend.MsgWorkerReady:
				// Trigger the workflow over the control leg, definition
				// as raw bytes — exactly what host.Session.RunWorkflow
				// sends. Async: the pipes are unbuffered, so a
				// synchronous send here can deadlock against a worker
				// that is itself mid-Send (e.g. telemetry).
				args, _ := json.Marshal(wire.WorkflowRun{
					Name: "wf", Definition: []byte(def), Args: "pebbles",
				})
				body, _ := backend.NewBody(wire.ControlRequest{Method: wire.CtrlRunWorkflow, Args: args})
				go func() {
					if err := hostCh.Send(backend.Envelope{Kind: backend.MsgControlRequest, Corr: "ctl-wf", Body: body}); err != nil {
						t.Errorf("send control: %v", err)
					}
				}()

			case backend.MsgInferenceRequest:
				inferCount++
				var ir wire.InferenceRequest
				if err := json.Unmarshal(got.env.Body, &ir); err != nil {
					t.Fatalf("decode inference request: %v", err)
				}
				if ir.Provider != "test" {
					t.Fatalf("inference provider = %q, want test", ir.Provider)
				}
				// beta runs second and must see alpha's output rendered
				// into its prompt (the {{ .alpha.output }} template).
				if inferCount == 2 {
					var sawAlpha bool
					for _, m := range ir.Request.Messages {
						if strings.Contains(m.Content, "reply-1") {
							sawAlpha = true
						}
					}
					if !sawAlpha {
						t.Fatalf("beta's prompt does not carry alpha's output")
					}
				}
				corr := got.env.Corr
				n := inferCount
				go func() {
					emit := func(ev wire.LLMEvent) {
						b, _ := backend.NewBody(ev)
						if err := hostCh.Send(backend.Envelope{Kind: backend.MsgInferenceEvent, Corr: corr, Body: b}); err != nil {
							t.Errorf("emit: %v", err)
						}
					}
					emit(wire.LLMEvent{Type: int(llm.EventTextDelta), Text: fmt.Sprintf("reply-%d", n)})
					emit(wire.LLMEvent{Type: int(llm.EventDone)})
					if err := hostCh.Send(backend.Envelope{Kind: backend.MsgInferenceDone, Corr: corr}); err != nil {
						t.Errorf("inference done: %v", err)
					}
				}()

			case backend.MsgEvent:
				var eb backend.EventBody
				if err := json.Unmarshal(got.env.Body, &eb); err != nil {
					t.Fatalf("decode event: %v", err)
				}
				switch eb.Type {
				case "AgentStart":
					starts++
				case "AgentEnd":
					ends++
				}

			case backend.MsgControlResponse:
				if got.env.Corr != "ctl-wf" {
					break
				}
				var resp wire.ControlResponse
				if err := json.Unmarshal(got.env.Body, &resp); err != nil {
					t.Fatalf("decode control response: %v", err)
				}
				if resp.Error != "" {
					t.Fatalf("workflow failed worker-side: %s", resp.Error)
				}
				if err := json.Unmarshal(resp.Result, &result); err != nil {
					t.Fatalf("decode workflow result: %v", err)
				}
				gotResult = true
				// Wind the worker down like the host entry points do.
				go func() {
					if err := hostCh.Send(backend.Envelope{Kind: backend.MsgShutdown}); err != nil {
						t.Errorf("send shutdown: %v", err)
					}
				}()

			case backend.MsgWorkerError:
				var e backend.ErrorBody
				_ = json.Unmarshal(got.env.Body, &e)
				t.Fatalf("worker errored: %s", e.Message)

			case backend.MsgWorkerDone:
				workerDone = true
			}
		}
	}

	if !gotResult {
		t.Fatal("never received the workflow control response")
	}
	if result.Outputs["alpha"] != "reply-1" || result.Outputs["beta"] != "reply-2" {
		t.Fatalf("outputs = %#v, want alpha=reply-1 beta=reply-2", result.Outputs)
	}
	if len(result.RoleOrder) != 2 || result.RoleOrder[0] != "alpha" || result.RoleOrder[1] != "beta" {
		t.Fatalf("role order = %v, want [alpha beta]", result.RoleOrder)
	}
	if result.Last != "reply-2" {
		t.Fatalf("last = %q, want reply-2", result.Last)
	}
	if starts != 2 || ends != 2 {
		t.Fatalf("agent events: starts=%d ends=%d, want 2/2 — role transitions must surface on the host bus", starts, ends)
	}
	select {
	case serveErr := <-done:
		if serveErr != nil {
			t.Fatalf("Serve returned error: %v", serveErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after MsgWorkerDone")
	}
}
