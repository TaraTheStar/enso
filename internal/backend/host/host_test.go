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
	"github.com/TaraTheStar/enso/internal/backend/worker"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/llm/llmtest"
)

type noopCloser struct{}

func (noopCloser) Close() error { return nil }

// fakeBackend runs worker.Serve(RunAgent) in-process on the far side of
// an in-memory pipe pair — exercising the real host adapter against the
// real worker adapter, no subprocess, no real model.
type fakeBackend struct{ workerDone chan struct{} }

func (f *fakeBackend) Name() string { return "fake" }

func (f *fakeBackend) Start(ctx context.Context, spec backend.TaskSpec) (backend.Worker, error) {
	h2wR, h2wW := io.Pipe()
	w2hR, w2hW := io.Pipe()
	workerCh := backend.NewStreamChannelRW(h2wR, w2hW, noopCloser{})
	hostCh := backend.NewStreamChannelRW(w2hR, h2wW, noopCloser{})

	f.workerDone = make(chan struct{})
	go func() {
		defer close(f.workerDone)
		_ = worker.Serve(ctx, workerCh, worker.RunAgent)
	}()
	return &fakeWorker{ch: hostCh, done: f.workerDone}, nil
}

type fakeWorker struct {
	ch   backend.Channel
	done chan struct{}
}

func (w *fakeWorker) Channel() backend.Channel { return w.ch }
func (w *fakeWorker) Wait(ctx context.Context) error {
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (w *fakeWorker) Teardown(context.Context) error { return nil }

func TestSessionEndToEnd(t *testing.T) {
	mock := llmtest.New()
	mock.Push(llmtest.Script{Text: "pong"})

	providers := map[string]*llm.Provider{
		"test": {Name: "test", Model: "m", Pool: llm.NewPool(4), Client: mock},
	}

	cfg := &config.Config{}
	cfg.Permissions.Mode = "allow"
	rc, _ := json.Marshal(cfg)

	spec := backend.TaskSpec{
		TaskID:          "t1",
		Cwd:             t.TempDir(),
		Prompt:          "ping",
		Interactive:     false,
		Ephemeral:       true,
		ResolvedConfig:  rc,
		Providers:       []backend.ProviderInfo{{Name: "test", Model: "m"}},
		DefaultProvider: "test",
	}

	busInst := bus.New()
	sub := busInst.Subscribe(256)

	var sawText bool
	collected := make(chan struct{})
	go func() {
		for ev := range sub {
			if ev.Type == bus.EventAssistantDelta {
				if s, _ := ev.Payload.(string); s == "pong" {
					sawText = true
				}
			}
		}
		close(collected)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := host.Start(ctx, &fakeBackend{}, spec, providers, busInst)
	if err != nil {
		t.Fatalf("host.Start: %v", err)
	}

	if err := sess.Wait(); err != nil {
		t.Fatalf("session ended with error: %v", err)
	}
	busInst.Close()
	<-collected

	if !sawText {
		t.Fatal("assistant text never reached the host bus")
	}
	if mock.CallCount() != 1 {
		t.Fatalf("expected exactly 1 host-proxied inference call, got %d", mock.CallCount())
	}
}

// TestSessionTelemetry exercises the worker→host telemetry leg: the
// worker reports token accounting + the provider it selected; the host
// augments that with the REAL provider's context window (which the
// credential-scrubbed worker never sees), and exposes it via Telemetry().
func TestSessionTelemetry(t *testing.T) {
	mock := llmtest.New()
	mock.Push(llmtest.Script{Text: "pong"})

	providers := map[string]*llm.Provider{
		// ContextWindow is set ONLY host-side: the worker rebuilds
		// providers from the non-secret catalog and has no window. If
		// Telemetry() reports it, the host augmentation works.
		"test": {Name: "test", Model: "m", ContextWindow: 32000, Pool: llm.NewPool(4), Client: mock},
	}

	cfg := &config.Config{}
	cfg.Permissions.Mode = "allow"
	rc, _ := json.Marshal(cfg)

	spec := backend.TaskSpec{
		TaskID:          "t1",
		Cwd:             t.TempDir(),
		Prompt:          "ping",
		Ephemeral:       true,
		ResolvedConfig:  rc,
		Providers:       []backend.ProviderInfo{{Name: "test", Model: "m"}},
		DefaultProvider: "test",
	}

	busInst := bus.New()
	sub := busInst.Subscribe(256)
	go func() {
		for range sub {
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := host.Start(ctx, &fakeBackend{}, spec, providers, busInst)
	if err != nil {
		t.Fatalf("host.Start: %v", err)
	}

	// Seeded from the spec before any worker telemetry arrives.
	if got := sess.Telemetry(); got.Provider != "test" || got.Model != "m" {
		t.Fatalf("seeded telemetry: got %+v, want provider=test model=m", got)
	}

	if err := sess.Wait(); err != nil {
		t.Fatalf("session ended with error: %v", err)
	}
	busInst.Close()

	tel := sess.Telemetry()
	if tel.Provider != "test" {
		t.Errorf("provider: got %q, want %q", tel.Provider, "test")
	}
	if tel.Model != "m" {
		t.Errorf("model: got %q, want %q", tel.Model, "m")
	}
	if tel.ContextWindow != 32000 {
		t.Errorf("context window not augmented host-side: got %d, want 32000", tel.ContextWindow)
	}
	if tel.CumIn <= 0 || tel.CumOut <= 0 {
		t.Errorf("token accounting not reported: in=%d out=%d (want both >0 after a completed turn)", tel.CumIn, tel.CumOut)
	}
}
