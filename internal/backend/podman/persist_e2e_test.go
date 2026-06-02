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

// TestPodmanRemoteSessionPersistence proves the Option-A fix end to end
// through a REAL sealed container: an isolated worker cannot reach the
// host DB, so it ships each append over the seam and the host applies
// it via host.WithWriter. Asserts the host DB ends up with the rows the
// session picker (ListRecentWithStats, HAVING msg_count>0) and
// --continue (ListRecent) read — the exact symptoms of the bug.
//
// Real-container gated: environment limits SKIP; a real regression
// (host DB still empty after an isolated turn) FAILS.
func TestPodmanRemoteSessionPersistence(t *testing.T) {
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

	// NOTE: do NOT override XDG_DATA_HOME — rootless podman uses it for
	// container storage, and redirecting it into a t.TempDir() leaves
	// mapped-root files the harness can't delete. Use the REAL session
	// store (as the conformance tests use the real environment) with a
	// unique throwaway session id, Discard()'d on cleanup.

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

	// Host owns the row + the writer the worker's persist envelopes are
	// applied to (exactly what bubble/run.go + cmd/enso/run.go now do).
	store, err := session.Open()
	if err != nil {
		t.Fatalf("session.Open (host): %v", err)
	}
	sid := "persist-e2e-" + uuid.NewString()
	if _, err := session.NewSessionWithID(store, sid, "m", "test", t.TempDir()); err != nil {
		t.Fatalf("NewSessionWithID: %v", err)
	}
	writer, err := session.AttachWriter(store, sid)
	if err != nil {
		t.Fatalf("AttachWriter: %v", err)
	}
	// Remove the throwaway row (cascades messages/tool_calls/events)
	// before closing the real store — keep the user's DB pristine.
	t.Cleanup(func() { _ = writer.Discard(); _ = store.Close() })

	proj := t.TempDir()
	spec := backend.TaskSpec{
		TaskID:          "pmpersist1",
		Cwd:             proj,
		Interactive:     true,
		Ephemeral:       false,
		SessionID:       sid,
		ResolvedConfig:  rc,
		Providers:       []backend.ProviderInfo{{Name: "test", Model: "m"}},
		DefaultProvider: "test",
		// Kind != "" → adapter routes the writer over the seam (the fix).
		Isolation: backend.IsolationSpec{NetworkSealed: true, Image: testImage, Kind: "container"},
	}
	b := &podman.Backend{Exe: bin, Image: testImage, Runtime: "podman", Network: "none"}

	busInst := bus.New()
	sub := busInst.Subscribe(256)
	idle := make(chan struct{}, 1)
	go func() {
		for ev := range sub {
			if ev.Type == bus.EventAgentIdle {
				select {
				case idle <- struct{}{}:
				default:
				}
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sess, err := host.Start(ctx, b, spec, providers, busInst, host.WithWriter(writer))
	if err != nil {
		t.Skipf("host.Start (podman) failed (environment, not enso wiring): %v", err)
	}
	defer sess.Close()

	if err := sess.Submit("ping", nil); err != nil {
		t.Fatalf("submit: %v", err)
	}
	select {
	case <-idle:
	case <-ctx.Done():
		t.Fatal("timed out waiting for the turn inside the container")
	}
	sess.CloseInput()
	_ = sess.Wait()

	// The host DB must now hold the session's messages, written ONLY via
	// the remote-persist seam (the worker never touched this file).
	// Poll briefly: the last persist envelope may land just after idle.
	var hist int
	for i := 0; i < 50; i++ {
		if st, lerr := session.Load(store, sid); lerr == nil && len(st.History) > 0 {
			hist = len(st.History)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if hist == 0 {
		t.Fatal("host DB has NO messages after an isolated turn — remote session " +
			"persistence broken (picker/--continue would not see this session)")
	}

	// Picker symptom: ListRecentWithStats requires msg_count > 0.
	stats, err := session.ListRecentWithStats(store, "", 10)
	if err != nil {
		t.Fatalf("ListRecentWithStats: %v", err)
	}
	var picked bool
	for _, s := range stats {
		if s.ID == sid {
			if s.MessageCount == 0 {
				t.Fatalf("session %s present but msg_count=0 — still hidden from the picker", sid)
			}
			picked = true
		}
	}
	if !picked {
		t.Fatalf("session %s absent from ListRecentWithStats — not selectable in the picker", sid)
	}

	// --continue symptom: ListRecent must surface it (use a window, not
	// strictly [0] — the real DB may hold the user's other sessions).
	recent, err := session.ListRecent(store, "", 50)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	var found bool
	for _, r := range recent {
		if r.ID == sid {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListRecent did not surface %s for --continue", sid)
	}
}
