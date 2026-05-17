// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/host"
	"github.com/TaraTheStar/enso/internal/backend/local"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/llm/llmtest"
)

// buildEnso compiles the real enso binary once per test run. The
// conformance suite exercises the Backend through a genuine subprocess
// (`enso __worker`), not an in-process fake, so it proves the wire
// framing, handshake, demux, inference proxy, control RPC, telemetry,
// and teardown over real OS pipes — the path PodmanBackend will inherit.
var (
	ensoBinOnce sync.Once
	ensoBin     string
	ensoBinErr  error
)

func ensoBinary(t *testing.T) string {
	t.Helper()
	ensoBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "enso-conformance")
		if err != nil {
			ensoBinErr = err
			return
		}
		ensoBin = filepath.Join(dir, "enso")
		cmd := exec.Command("go", "build", "-o", ensoBin, "github.com/TaraTheStar/enso/cmd/enso")
		out, err := cmd.CombinedOutput()
		if err != nil {
			ensoBinErr = err
			t.Logf("go build output:\n%s", out)
		}
	})
	if ensoBinErr != nil {
		t.Fatalf("build enso: %v", ensoBinErr)
	}
	return ensoBin
}

// TestLocalBackendConformance drives a real worker subprocess end to
// end: handshake → host-proxied inference (Mock provider host-side,
// since the worker is credential-scrubbed) → bus events out → control
// RPC + telemetry round-trip → clean shutdown + teardown.
func TestLocalBackendConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}
	bin := ensoBinary(t)

	mock := llmtest.New()
	mock.Push(llmtest.Script{Text: "pong"})

	providers := map[string]*llm.Provider{
		// ContextWindow is host-only: if Telemetry() reports it the
		// host augmentation is wired through a real subprocess.
		"test": {Name: "test", Model: "m", ContextWindow: 32000, Pool: llm.NewPool(4), Client: mock},
	}

	cfg := &config.Config{}
	cfg.Permissions.Mode = "allow"
	rc, _ := json.Marshal(cfg.ScrubbedForWorker())

	spec := backend.TaskSpec{
		TaskID:          "conf-1",
		Cwd:             t.TempDir(),
		Interactive:     true, // stay up so we can exercise control RPC
		Ephemeral:       true, // never touch the real session store
		ResolvedConfig:  rc,
		Providers:       []backend.ProviderInfo{{Name: "test", Model: "m"}},
		DefaultProvider: "test",
	}

	busInst := bus.New()
	sub := busInst.Subscribe(256)
	idle := make(chan struct{}, 1)
	var sawPong bool
	go func() {
		for ev := range sub {
			switch ev.Type {
			case bus.EventAssistantDelta:
				if s, _ := ev.Payload.(string); s == "pong" {
					sawPong = true
				}
			case bus.EventAgentIdle:
				select {
				case idle <- struct{}{}:
				default:
				}
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess, err := host.Start(ctx, &local.Backend{Exe: bin}, spec, providers, busInst)
	if err != nil {
		t.Fatalf("host.Start: %v", err)
	}
	defer sess.Close()

	if err := sess.Submit("ping"); err != nil {
		t.Fatalf("submit: %v", err)
	}

	select {
	case <-idle:
	case <-ctx.Done():
		t.Fatal("timed out waiting for the turn to complete")
	}

	if !sawPong {
		t.Fatal("assistant text never crossed the subprocess seam")
	}
	if mock.CallCount() != 1 {
		t.Fatalf("expected 1 host-proxied inference call, got %d", mock.CallCount())
	}

	// Telemetry: worker-sourced token accounting + host-augmented window.
	tel := sess.Telemetry()
	if tel.Provider != "test" || tel.Model != "m" {
		t.Errorf("telemetry identity: got %+v", tel)
	}
	if tel.ContextWindow != 32000 {
		t.Errorf("context window not host-augmented over subprocess: %d", tel.ContextWindow)
	}
	if tel.CumIn <= 0 || tel.CumOut <= 0 {
		t.Errorf("token accounting not reported: in=%d out=%d", tel.CumIn, tel.CumOut)
	}

	// Control RPC over the real pipe: prefix breakdown reflects the
	// system+user+assistant history the worker built.
	bd, err := sess.PrefixBreakdown(ctx)
	if err != nil {
		t.Fatalf("PrefixBreakdown RPC: %v", err)
	}
	if bd.Total <= 0 {
		t.Errorf("prefix breakdown total should be > 0, got %d", bd.Total)
	}
	if _, err := sess.CompactPreview(ctx); err != nil {
		t.Fatalf("CompactPreview RPC: %v", err)
	}
	if err := sess.SetNextTurnTools(ctx, []string{"bash"}); err != nil {
		t.Fatalf("SetNextTurnTools RPC: %v", err)
	}

	// Clean shutdown: closing input winds the worker down quiescent.
	sess.CloseInput()
	if err := sess.Wait(); err != nil {
		t.Fatalf("worker did not exit cleanly: %v", err)
	}
}
