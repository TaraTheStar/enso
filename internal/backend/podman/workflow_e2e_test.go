// SPDX-License-Identifier: AGPL-3.0-or-later

package podman_test

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

	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/host"
	"github.com/TaraTheStar/enso/internal/backend/podman"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/llm/llmtest"
	"github.com/TaraTheStar/enso/internal/session"
)

// workflowE2EDef is the two-role definition both isolated-backend
// workflow tests ship over the seam: alpha runs one bash probe (whose
// output proves WHERE the tool executed), beta consumes alpha's output.
const workflowE2EDef = `---
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

// TestPodmanWorkflowOverSeam proves a multi-role declarative workflow
// runs INSIDE a real sealed container behind the Backend seam: the
// definition crosses the Channel as raw bytes, the role's bash tool
// call executes in the guest (asserted via /run/.containerenv), role
// events surface on the host bus, persistence lands in the host DB via
// the remote-persist seam, and the result returns on the control leg.
// This is the end-to-end acceptance for routing workflow.Run through
// the worker (the old host-side path + its refusal guard are gone).
//
// Real-container gated: environment limits SKIP; a wiring regression
// FAILS.
func TestPodmanWorkflowOverSeam(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a static binary + runs a container; skipped in -short")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not on PATH")
	}
	pullCtx, cancelPull := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelPull()
	if out, err := exec.CommandContext(pullCtx, "podman", "pull", "-q", testImage).CombinedOutput(); err != nil {
		t.Skipf("cannot pull %s (no registry access?): %v\n%s", testImage, err, out)
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "enso")
	build := exec.Command("go", "build", "-o", bin, "github.com/TaraTheStar/enso/cmd/enso")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("static build: %v\n%s", err, out)
	}

	// Scripted inference, host-side (the sealed worker proxies every
	// call): alpha's turn 1 issues the bash probe, turn 2 settles;
	// beta's single turn settles.
	mock := llmtest.New()
	probe := llm.ToolCall{ID: "tc1", Type: "function"}
	probe.Function.Name = "bash"
	probe.Function.Arguments = `{"cmd":"test -f /run/.containerenv && echo IN_CONTAINER || echo ON_HOST"}`
	mock.Push(llmtest.Script{ToolCalls: []llm.ToolCall{probe}})
	mock.Push(llmtest.Script{Text: "alpha done"})
	mock.Push(llmtest.Script{Text: "beta done"})
	providers := map[string]*llm.Provider{
		"test": {Name: "test", Model: "m", ContextWindow: 4096, Pool: llm.NewPool(2), Client: mock},
	}

	cfg := &config.Config{}
	cfg.Permissions.Mode = "allow"
	rc, _ := json.Marshal(cfg.ScrubbedForWorker())

	// Real host store + throwaway session row, exactly like the
	// persist e2e: workflow role appends must land HERE over the seam.
	store, err := session.Open()
	if err != nil {
		t.Fatalf("session.Open (host): %v", err)
	}
	sid := "wf-e2e-" + uuid.NewString()
	if _, err := session.NewSessionWithID(store, sid, "m", "test", t.TempDir()); err != nil {
		t.Fatalf("NewSessionWithID: %v", err)
	}
	writer, err := session.AttachWriter(store, sid)
	if err != nil {
		t.Fatalf("AttachWriter: %v", err)
	}
	t.Cleanup(func() { _ = writer.Discard(); _ = store.Close() })

	proj := t.TempDir()
	spec := backend.TaskSpec{
		TaskID:          "pmwf1",
		Cwd:             proj,
		Interactive:     true, // workflow rides the control leg; no prompt
		SessionID:       sid,
		ResolvedConfig:  rc,
		Providers:       []backend.ProviderInfo{{Name: "test", Model: "m"}},
		DefaultProvider: "test",
		Isolation:       backend.IsolationSpec{NetworkSealed: true, Image: testImage, Kind: "container"},
	}
	b := &podman.Backend{Exe: bin, Image: testImage, Runtime: "podman", Network: "none"}

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

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	sess, err := host.Start(ctx, b, spec, providers, busInst, host.WithWriter(writer))
	if err != nil {
		t.Skipf("host.Start (podman) failed (environment, not enso wiring): %v", err)
	}
	defer sess.Close()
	host.RecordWorkerAttach(writer, b, spec.Isolation, spec.TaskID)

	res, err := sess.RunWorkflow(ctx, "wfpodman", []byte(workflowE2EDef), "ship it")
	if err != nil {
		t.Fatalf("RunWorkflow over the podman seam: %v", err)
	}

	if res.Outputs["alpha"] != "alpha done" || res.Outputs["beta"] != "beta done" {
		t.Fatalf("outputs = %#v, want alpha/beta done", res.Outputs)
	}
	if len(res.RoleOrder) != 2 || res.RoleOrder[0] != "alpha" || res.RoleOrder[1] != "beta" {
		t.Fatalf("role order = %v, want [alpha beta]", res.RoleOrder)
	}

	// THE acceptance check: the role's bash call ran in the GUEST.
	// Its persisted tool_call row (shipped over MsgPersistToolCall,
	// applied by the host before the control response on the ordered
	// seam) must carry the container marker.
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
	if !strings.Contains(out, "IN_CONTAINER") {
		t.Fatalf("workflow bash tool did not execute in the container (tool output %q)", out)
	}

	// Role transitions must have surfaced on the HOST bus (agents tree).
	mu.Lock()
	sawAlpha, sawBeta := roles["alpha"], roles["beta"]
	mu.Unlock()
	if !sawAlpha || !sawBeta {
		t.Fatalf("role AgentStart events missing on host bus: alpha=%v beta=%v", sawAlpha, sawBeta)
	}

	// Provenance: the attach stamped the backend column.
	var backendCol string
	_ = store.DB.QueryRow(`SELECT backend FROM sessions WHERE id = ?`, sid).Scan(&backendCol)
	if backendCol != "podman" {
		t.Fatalf("sessions.backend = %q, want podman", backendCol)
	}

	sess.CloseInput()
	if err := sess.Wait(); err != nil {
		t.Fatalf("worker did not exit cleanly: %v", err)
	}
	if got := mock.CallCount(); got != 3 {
		t.Fatalf("host-proxied inference calls = %d, want 3 (alpha×2, beta×1)", got)
	}
}
