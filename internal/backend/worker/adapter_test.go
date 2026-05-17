// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/wire"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/llm"
)

// TestRunAgentEndToEnd proves the lifted agent core runs entirely
// behind the Channel: input in, host-proxied inference, events out —
// no in-process agent, no real model, no host secrets.
func TestRunAgentEndToEnd(t *testing.T) {
	workerCh, hostCh := newChannelPair()

	cfg := &config.Config{}
	cfg.Permissions.Mode = "allow" // no permission prompts in this path
	rc, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	spec := backend.TaskSpec{
		TaskID:          "t1",
		Cwd:             t.TempDir(),
		Prompt:          "say hello",
		Interactive:     false,
		Ephemeral:       true,
		ResolvedConfig:  rc,
		Providers:       []backend.ProviderInfo{{Name: "test", Model: "m"}},
		DefaultProvider: "test",
	}

	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), workerCh, RunAgent) }()

	// Handshake.
	specBody, _ := backend.NewBody(spec)
	if err := hostCh.Send(backend.Envelope{Kind: backend.MsgTaskSpec, Body: specBody}); err != nil {
		t.Fatalf("send spec: %v", err)
	}

	var sawAssistantText, workerDone bool
	deadline := time.After(10 * time.Second)
	for !workerDone {
		type recvd struct {
			env backend.Envelope
			err error
		}
		rc := make(chan recvd, 1)
		go func() { e, err := hostCh.Recv(); rc <- recvd{e, err} }()

		select {
		case <-deadline:
			t.Fatal("timed out waiting for worker to finish")
		case got := <-rc:
			if got.err != nil {
				t.Fatalf("host recv: %v", got.err)
			}
			switch got.env.Kind {
			case backend.MsgWorkerReady:
				// proceed

			case backend.MsgInferenceRequest:
				// Host-proxied inference: the worker carries no model.
				// Verify the request is a real serialized ChatRequest
				// tagged with its provider, then stream a canned reply.
				var ir wire.InferenceRequest
				if err := json.Unmarshal(got.env.Body, &ir); err != nil {
					t.Fatalf("decode inference request: %v", err)
				}
				if ir.Provider != "test" {
					t.Fatalf("inference request provider = %q, want %q", ir.Provider, "test")
				}
				corr := got.env.Corr
				emit := func(ev wire.LLMEvent) {
					b, _ := backend.NewBody(ev)
					if err := hostCh.Send(backend.Envelope{
						Kind: backend.MsgInferenceEvent, Corr: corr, Body: b,
					}); err != nil {
						t.Errorf("emit inference event: %v", err)
					}
				}
				emit(wire.LLMEvent{Type: int(llm.EventTextDelta), Text: "hello there"})
				emit(wire.LLMEvent{Type: int(llm.EventDone)})
				if err := hostCh.Send(backend.Envelope{
					Kind: backend.MsgInferenceDone, Corr: corr,
				}); err != nil {
					t.Fatalf("inference done: %v", err)
				}

			case backend.MsgEvent:
				var eb backend.EventBody
				if err := json.Unmarshal(got.env.Body, &eb); err != nil {
					t.Fatalf("decode event body: %v", err)
				}
				if eb.Type == "AssistantDelta" || eb.Type == "AssistantDone" {
					var s string
					_ = json.Unmarshal(eb.Payload, &s)
					if eb.Type == "AssistantDelta" && s != "" {
						sawAssistantText = true
					}
				}

			case backend.MsgWorkerError:
				var e backend.ErrorBody
				_ = json.Unmarshal(got.env.Body, &e)
				t.Fatalf("worker errored: %s", e.Message)

			case backend.MsgWorkerDone:
				workerDone = true
			}
		}
	}

	if !sawAssistantText {
		t.Fatal("never saw assistant text forwarded over the Channel")
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

func pipe() (io.Reader, io.Writer) {
	r, w := io.Pipe()
	return r, w
}

// newChannelPair returns two Channels wired back to back over a pair of
// in-memory pipes (worker side, host side).
func newChannelPair() (workerCh, hostCh backend.Channel) {
	h2wR, h2wW := pipe()
	w2hR, w2hW := pipe()
	workerCh = backend.NewStreamChannelRW(h2wR, w2hW, noopCloser{})
	hostCh = backend.NewStreamChannelRW(w2hR, h2wW, noopCloser{})
	return
}
