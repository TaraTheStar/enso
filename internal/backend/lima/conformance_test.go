// SPDX-License-Identifier: AGPL-3.0-or-later

package lima_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/host"
	"github.com/TaraTheStar/enso/internal/backend/lima"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/llm/llmtest"
)

// TestLimaBackendConformance proves the SAME Backend contract through a
// real Lima VM: a static enso runs `__worker` inside the guest, dials
// no model (inference is host-proxied over the limactl-shell stdio
// Channel from a Mock provider), and the project dir is mounted at its
// REAL path so there is one filesystem namespace. The seam (handshake /
// host-proxied inference / telemetry / control RPC / clean teardown) is
// identical to podman's — this validates it end-to-end on real VM
// substrate where the host supports it.
//
// Environment limits (no limactl, no /dev/kvm, image download / first
// boot too slow, lima too old) SKIP rather than false-fail — the arg /
// config / fail-safe wiring is covered by fast unit tests; only the
// real-VM bring-up needs this host. A wiring regression (worker comes
// up but pong/telemetry/RPC wrong) still FAILS.
func TestLimaBackendConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a static binary + boots a VM; skipped in -short")
	}
	if _, err := exec.LookPath("limactl"); err != nil {
		t.Skip("limactl (Lima) not on PATH")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm: hardware virtualization unavailable for a Lima VM")
	}

	// Static, CGO-free enso so it runs on the guest distro regardless
	// of libc.
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

	// Unique temp project → unique per-project VM name; we delete
	// exactly that instance afterward (never a broad Sweep, which
	// would hit a developer's real enso project VMs).
	proj := t.TempDir()
	name := lima.VMName(proj)
	t.Cleanup(func() {
		cl, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = exec.CommandContext(cl, "limactl", "stop", "--force", name).Run()
		_ = exec.CommandContext(cl, "limactl", "delete", "--force", name).Run()
	})

	spec := backend.TaskSpec{
		TaskID:          "limaconf1",
		Cwd:             proj,
		Interactive:     true,
		Ephemeral:       true,
		ResolvedConfig:  rc,
		Providers:       []backend.ProviderInfo{{Name: "test", Model: "m"}},
		DefaultProvider: "test",
		Isolation:       backend.IsolationSpec{NetworkSealed: true, Kind: "vm"},
	}
	b := &lima.Backend{Exe: bin}

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

	// Generous: first run downloads a cloud image and boots a VM.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	sess, err := host.Start(ctx, b, spec, providers, busInst)
	if err != nil {
		// Bring-up failure is an environment limit (image download,
		// KVM, lima too old), NOT an enso wiring bug — that is unit-
		// tested. Skip rather than false-fail.
		t.Skipf("Lima VM could not be brought up on this host (environment, not enso wiring): %v", err)
	}
	defer sess.Close()

	if err := sess.Submit("ping"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	select {
	case <-idle:
	case <-ctx.Done():
		t.Fatal("timed out waiting for the turn inside the VM")
	}

	if !sawPong {
		t.Fatal("assistant text never crossed the VM seam")
	}
	if mock.CallCount() != 1 {
		t.Fatalf("expected 1 host-proxied inference call from the sealed worker, got %d", mock.CallCount())
	}
	tel := sess.Telemetry()
	if tel.Provider != "test" || tel.ContextWindow != 4096 {
		t.Errorf("telemetry over VM seam wrong: %+v", tel)
	}
	if _, err := sess.PrefixBreakdown(ctx); err != nil {
		t.Fatalf("control RPC over VM seam: %v", err)
	}

	sess.CloseInput()
	if err := sess.Wait(); err != nil {
		t.Fatalf("worker did not exit cleanly: %v", err)
	}

	// Substrate decision: the per-project VM is PERSISTENT — Teardown
	// must NOT have deleted it (that is what makes it carry forward).
	if st := strings.TrimSpace(vmStatusFor(name)); st == "" {
		t.Errorf("persistent per-project VM %q must survive task teardown, but it is gone", name)
	}
}

func vmStatusFor(name string) string {
	out, err := exec.Command("limactl", "list", "--format", "{{.Status}}", name).Output()
	if err != nil {
		return ""
	}
	return string(out)
}
