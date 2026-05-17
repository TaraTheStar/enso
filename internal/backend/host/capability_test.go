// SPDX-License-Identifier: AGPL-3.0-or-later

package host_test

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/host"
	"github.com/TaraTheStar/enso/internal/backend/wire"
	"github.com/TaraTheStar/enso/internal/backend/worker"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/llm/llmtest"
)

// capAgent is a worker-side AgentFunc that issues two capability
// requests over the real Channel and reports the grants back as an
// error string — enough to assert the host broker decision crossed the
// seam intact. (worker.Serve already did the handshake.)
func capAgent(want chan<- [2]wire.CapabilityGrant) worker.AgentFunc {
	return func(ctx context.Context, _ backend.TaskSpec, ch backend.Channel) error {
		send := func(name string) wire.CapabilityGrant {
			body, _ := backend.NewBody(wire.CapabilityRequest{Type: wire.CapCredential, Name: name, Reason: "test"})
			_ = ch.Send(backend.Envelope{Kind: backend.MsgCapabilityRequest, Corr: name, Body: body})
			for {
				env, err := ch.Recv()
				if err != nil {
					return wire.CapabilityGrant{Reason: "recv: " + err.Error()}
				}
				if env.Kind == backend.MsgCapabilityGrant && env.Corr == name {
					var g wire.CapabilityGrant
					_ = json.Unmarshal(env.Body, &g)
					return g
				}
			}
		}
		want <- [2]wire.CapabilityGrant{send("GH_TOKEN"), send("UNKNOWN")}
		return nil
	}
}

type testBroker struct{}

func (testBroker) Authorize(_ context.Context, req wire.CapabilityRequest) wire.CapabilityGrant {
	if req.Type == wire.CapCredential && req.Name == "GH_TOKEN" {
		return wire.CapabilityGrant{Granted: true, Secret: "ghp_xyz", TTLSeconds: 60}
	}
	return wire.CapabilityGrant{Granted: false, Reason: "not on allowlist"}
}

type capBackend struct{ agent worker.AgentFunc }

func (c *capBackend) Name() string { return "capfake" }
func (c *capBackend) Start(ctx context.Context, _ backend.TaskSpec) (backend.Worker, error) {
	h2wR, h2wW := io.Pipe()
	w2hR, w2hW := io.Pipe()
	workerCh := backend.NewStreamChannelRW(h2wR, w2hW, noopCloser{})
	hostCh := backend.NewStreamChannelRW(w2hR, h2wW, noopCloser{})
	done := make(chan struct{})
	go func() { defer close(done); _ = worker.Serve(ctx, workerCh, c.agent) }()
	return &fakeWorker{ch: hostCh, done: done}, nil
}

func TestCapabilityBroker(t *testing.T) {
	cfg := &config.Config{}
	rc, _ := json.Marshal(cfg)
	spec := backend.TaskSpec{
		TaskID: "cap", Cwd: t.TempDir(), Ephemeral: true,
		ResolvedConfig: rc,
		Providers:      []backend.ProviderInfo{{Name: "x", Model: "m"}},
	}
	providers := map[string]*llm.Provider{"x": {Name: "x", Model: "m", Pool: llm.NewPool(1), Client: llmtest.New()}}

	t.Run("granted+denied via broker", func(t *testing.T) {
		got := make(chan [2]wire.CapabilityGrant, 1)
		busInst := bus.New()
		sess, err := host.Start(context.Background(), &capBackend{agent: capAgent(got)},
			spec, providers, busInst, host.WithBroker(testBroker{}))
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer sess.Close()
		select {
		case g := <-got:
			if !g[0].Granted || g[0].Secret != "ghp_xyz" || g[0].TTLSeconds != 60 {
				t.Errorf("GH_TOKEN: want granted secret, got %+v", g[0])
			}
			if g[1].Granted {
				t.Errorf("UNKNOWN: want denied, got %+v", g[1])
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for capability round-trip")
		}
	})

	t.Run("default deny without broker", func(t *testing.T) {
		got := make(chan [2]wire.CapabilityGrant, 1)
		busInst := bus.New()
		sess, err := host.Start(context.Background(), &capBackend{agent: capAgent(got)},
			spec, providers, busInst) // no WithBroker → denyBroker
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer sess.Close()
		select {
		case g := <-got:
			if g[0].Granted || g[1].Granted {
				t.Errorf("default policy must deny all, got %+v", g)
			}
			if g[0].Reason == "" {
				t.Error("denied grant should carry an auditable reason")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timeout")
		}
	})
}
