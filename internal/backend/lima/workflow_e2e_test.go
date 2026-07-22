// SPDX-License-Identifier: AGPL-3.0-or-later

package lima_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/TaraTheStar/azoth/llm"
	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/host"
	"github.com/TaraTheStar/enso/internal/backend/lima"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/llm/llmtest"
	"github.com/TaraTheStar/enso/internal/provider"
	"github.com/TaraTheStar/enso/internal/session"
)

// workflowVMDef mirrors the podman workflow e2e definition; alpha's
// bash probe proves the tool ran inside the Lima guest.
const workflowVMDef = `---
roles:
  alpha:
    tools: [bash]
  beta:
    tools: []
edges:
  - alpha -> beta
---

## alpha

Probe the environment. Args: {{ .Args }}

## beta

Alpha said: {{ .alpha.output }}
`

// TestLimaWorkflowOverSeam_RealVM proves a multi-role declarative
// workflow runs INSIDE a real Lima VM behind the Backend seam: raw
// definition over the Channel, the role's bash probe executing in the
// guest (lima-* hostname), role events on the host bus, persistence in
// the host DB via the remote-persist seam, result on the control leg.
//
// Real-VM gated like the conformance test: environment limits SKIP; a
// wiring regression FAILS.
func TestLimaWorkflowOverSeam_RealVM(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a static binary + boots a VM; skipped in -short")
	}
	if _, err := exec.LookPath("limactl"); err != nil {
		t.Skip("limactl (Lima) not on PATH")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm: hardware virtualization unavailable for a Lima VM")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "enso")
	build := exec.Command("go", "build", "-o", bin, "github.com/TaraTheStar/enso/cmd/enso")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("static build: %v\n%s", err, out)
	}

	mock := llmtest.New()
	probe := llm.ToolCall{ID: "tc1", Type: "function"}
	probe.Function.Name = "bash"
	probe.Function.Arguments = `{"cmd":"uname -n"}`
	mock.Push(llmtest.Script{ToolCalls: []llm.ToolCall{probe}})
	mock.Push(llmtest.Script{Text: "alpha done"})
	mock.Push(llmtest.Script{Text: "beta done"})
	providers := map[string]*provider.Provider{
		"test": {Name: "test", Model: "m", ContextWindow: 4096, Pool: llm.NewPool(2), Client: mock},
	}

	cfg := &config.Config{}
	cfg.Permissions.Mode = "allow"
	rc, _ := json.Marshal(cfg.ScrubbedForWorker())

	// Real host store + throwaway row: the VM worker can't reach this
	// DB, so every workflow append must arrive over the seam.
	store, err := session.Open()
	if err != nil {
		t.Fatalf("session.Open (host): %v", err)
	}
	sid := "wf-vm-e2e-" + uuid.NewString()
	if _, err := session.NewSessionWithID(store, sid, "m", "test", t.TempDir()); err != nil {
		t.Fatalf("NewSessionWithID: %v", err)
	}
	writer, err := session.AttachWriter(store, sid)
	if err != nil {
		t.Fatalf("AttachWriter: %v", err)
	}
	t.Cleanup(func() { _ = writer.Discard(); _ = store.Close() })

	// Unique temp project → unique per-project VM; delete exactly that
	// instance afterward (never a broad sweep).
	proj := t.TempDir()
	name := lima.VMName(proj)
	t.Cleanup(func() {
		cl, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = exec.CommandContext(cl, "limactl", "stop", "--force", name).Run()
		_ = exec.CommandContext(cl, "limactl", "delete", "--force", name).Run()
	})

	spec := backend.TaskSpec{
		TaskID:          "limawf1",
		Cwd:             proj,
		Interactive:     true, // workflow rides the control leg; no prompt
		SessionID:       sid,
		ResolvedConfig:  rc,
		Providers:       []backend.ProviderInfo{{Name: "test", Model: "m"}},
		DefaultProvider: "test",
		Isolation:       backend.IsolationSpec{NetworkSealed: true, Kind: "vm"},
	}
	b := &lima.Backend{Exe: bin}

	busInst := bus.New()
	sub := busInst.Subscribe(256)
	var mu sync.Mutex
	roles := map[string]bool{}
	go func() {
		for ev := range sub {
			if ev.Type != bus.EventAgentStart {
				continue
			}
			if m, ok := ev.Payload.(map[string]any); ok {
				if r, _ := m["role"].(string); r != "" {
					mu.Lock()
					roles[r] = true
					mu.Unlock()
				}
			}
		}
	}()

	// Generous: first run downloads a cloud image and boots a VM.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	sess, err := host.Start(ctx, b, spec, providers, busInst, host.WithWriter(writer))
	if err != nil {
		t.Skipf("Lima VM could not be brought up on this host (environment, not enso wiring): %v", err)
	}
	defer sess.Close()
	host.RecordWorkerAttach(writer, b, spec.Isolation, spec.TaskID)

	res, err := sess.RunWorkflow(ctx, "wflima", []byte(workflowVMDef), "ship it")
	if err != nil {
		t.Fatalf("RunWorkflow over the VM seam: %v", err)
	}

	if res.Outputs["alpha"] != "alpha done" || res.Outputs["beta"] != "beta done" {
		t.Fatalf("outputs = %#v, want alpha/beta done", res.Outputs)
	}

	// THE acceptance check: the bash probe ran in the GUEST — a Lima
	// VM's hostname carries the lima- prefix; the host's never does.
	var out string
	for i := 0; i < 50; i++ {
		_ = store.DB.QueryRow(
			`SELECT llm_output || ' ' || full_output FROM tool_calls
			 WHERE session_id = ? AND name = 'bash' ORDER BY seq DESC LIMIT 1`, sid,
		).Scan(&out)
		if out != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(out, "lima-") {
		t.Fatalf("workflow bash tool did not execute in the VM (tool output %q)", out)
	}

	mu.Lock()
	sawAlpha, sawBeta := roles["alpha"], roles["beta"]
	mu.Unlock()
	if !sawAlpha || !sawBeta {
		t.Fatalf("role AgentStart events missing on host bus: alpha=%v beta=%v", sawAlpha, sawBeta)
	}

	var backendCol string
	_ = store.DB.QueryRow(`SELECT backend FROM sessions WHERE id = ?`, sid).Scan(&backendCol)
	if backendCol != "lima" {
		t.Fatalf("sessions.backend = %q, want lima", backendCol)
	}

	sess.CloseInput()
	if err := sess.Wait(); err != nil {
		t.Fatalf("worker did not exit cleanly: %v", err)
	}
	if got := mock.CallCount(); got != 3 {
		t.Fatalf("host-proxied inference calls = %d, want 3 (alpha×2, beta×1)", got)
	}
}
