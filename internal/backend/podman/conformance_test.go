// SPDX-License-Identifier: AGPL-3.0-or-later

package podman_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/host"
	"github.com/TaraTheStar/enso/internal/backend/podman"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/llm/llmtest"
)

const testImage = "docker.io/library/alpine:latest"

// TestPodmanBackendConformance proves the SAME Backend contract through
// a real network-sealed container: a static enso bind-mounted as
// /usr/local/bin/enso runs `__worker` as the container process, dials
// no model (inference is host-proxied over the stdio Channel from a
// Mock provider), and the project dir is mounted at its REAL path so
// there is one filesystem namespace. Skips cleanly where podman or the
// image isn't available.
func TestPodmanBackendConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a static binary + runs a container; skipped in -short")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not on PATH")
	}
	// Ensure the image is present (a sealed `--network none` run cannot
	// pull). Bounded; skip if the environment has no registry access.
	pullCtx, cancelPull := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelPull()
	if out, err := exec.CommandContext(pullCtx, "podman", "pull", "-q", testImage).CombinedOutput(); err != nil {
		t.Skipf("cannot pull %s (no registry access?): %v\n%s", testImage, err, out)
	}

	// Static, CGO-free enso so it runs on alpine/musl.
	dir := t.TempDir()
	bin := filepath.Join(dir, "enso")
	build := exec.Command("go", "build", "-o", bin, "github.com/TaraTheStar/enso/cmd/enso")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("static build: %v\n%s", err, out)
	}

	mock := llmtest.New()
	mock.Push(llmtest.Script{Text: "pong"})
	providers := map[string]*llm.Provider{
		"test": {Name: "test", Model: "m", ContextWindow: 4096, Pool: llm.NewPool(2), Client: mock},
	}

	cfg := &config.Config{}
	cfg.Permissions.Mode = "allow"
	rc, _ := json.Marshal(cfg.ScrubbedForWorker())

	// Project dir mounted at its real path inside the container.
	proj := t.TempDir()
	spec := backend.TaskSpec{
		TaskID:          "pmconf1",
		Cwd:             proj,
		Interactive:     true,
		Ephemeral:       true,
		ResolvedConfig:  rc,
		Providers:       []backend.ProviderInfo{{Name: "test", Model: "m"}},
		DefaultProvider: "test",
		Isolation:       backend.IsolationSpec{NetworkSealed: true, Image: testImage},
	}

	b := &podman.Backend{Exe: bin, Image: testImage, Runtime: "podman", Network: "none"}

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

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sess, err := host.Start(ctx, b, spec, providers, busInst)
	if err != nil {
		t.Fatalf("host.Start (podman): %v", err)
	}
	defer sess.Close()

	if err := sess.Submit("ping"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	select {
	case <-idle:
	case <-ctx.Done():
		t.Fatal("timed out waiting for the turn inside the container")
	}

	if !sawPong {
		t.Fatal("assistant text never crossed the container seam")
	}
	if mock.CallCount() != 1 {
		t.Fatalf("expected 1 host-proxied inference call from the sealed worker, got %d", mock.CallCount())
	}
	tel := sess.Telemetry()
	if tel.Provider != "test" || tel.ContextWindow != 4096 {
		t.Errorf("telemetry over container seam wrong: %+v", tel)
	}
	if _, err := sess.PrefixBreakdown(ctx); err != nil {
		t.Fatalf("control RPC over container seam: %v", err)
	}

	sess.CloseInput()
	if err := sess.Wait(); err != nil {
		t.Fatalf("sealed worker did not exit cleanly: %v", err)
	}
}
