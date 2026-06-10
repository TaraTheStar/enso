// SPDX-License-Identifier: AGPL-3.0-or-later

package host_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/host"
	"github.com/TaraTheStar/enso/internal/backend/wire"
	"github.com/TaraTheStar/enso/internal/backend/worker"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/permissions"
)

// These tests cover the host leg of the cross-seam permission relay:
// servePermission must reconstruct a real *permissions.PromptRequest on
// the host bus and relay the decision back as MsgPermissionDecision —
// fail-closed on a malformed request and on ctx cancellation.

// permReportAgent is a worker-side AgentFunc that sends one raw
// MsgPermissionRequest with the given body and reports the decision
// string the host relays back. (worker.Serve already did the handshake.)
func permReportAgent(body json.RawMessage, report chan<- string) worker.AgentFunc {
	return func(ctx context.Context, _ backend.TaskSpec, ch backend.Channel) error {
		if err := ch.Send(backend.Envelope{
			Kind: backend.MsgPermissionRequest, Corr: "perm-1", Body: body,
		}); err != nil {
			report <- "send-error: " + err.Error()
			return nil
		}
		for {
			env, err := ch.Recv()
			if err != nil {
				report <- "recv-error: " + err.Error()
				return nil
			}
			if env.Kind == backend.MsgPermissionDecision && env.Corr == "perm-1" {
				var d wire.PermissionDecision
				_ = json.Unmarshal(env.Body, &d)
				report <- d.Decision
				return nil
			}
		}
	}
}

// startPermSession brings up a Session whose worker immediately issues
// the given permission-request body. Returns the worker's decision
// report channel and the host-bus prompt stream.
func startPermSession(t *testing.T, ctx context.Context, body json.RawMessage) (<-chan string, <-chan *permissions.PromptRequest, *host.Session) {
	t.Helper()

	busInst := bus.New()
	t.Cleanup(busInst.Close)
	sub := busInst.Subscribe(64)
	prompts := make(chan *permissions.PromptRequest, 4)
	go func() {
		for ev := range sub {
			if ev.Type != bus.EventPermissionRequest {
				continue
			}
			if pr, ok := ev.Payload.(*permissions.PromptRequest); ok {
				prompts <- pr
			}
		}
	}()

	report := make(chan string, 1)
	spec := backend.TaskSpec{TaskID: "perm", Cwd: t.TempDir(), Ephemeral: true}
	sess, err := host.Start(ctx, &capBackend{agent: permReportAgent(body, report)},
		spec, nil, busInst)
	if err != nil {
		t.Fatalf("host.Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return report, prompts, sess
}

func waitReport(t *testing.T, report <-chan string) string {
	t.Helper()
	select {
	case d := <-report:
		return d
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the worker to receive a decision")
		return ""
	}
}

func waitPrompt(t *testing.T, prompts <-chan *permissions.PromptRequest) *permissions.PromptRequest {
	t.Helper()
	select {
	case pr := <-prompts:
		return pr
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the prompt on the host bus")
		return nil
	}
}

func permBody(t *testing.T) json.RawMessage {
	t.Helper()
	body, err := backend.NewBody(wire.PermissionRequest{
		Tool:      "bash",
		ArgString: "make test",
		Args:      map[string]any{"cmd": "make test"},
		Diff:      "a-diff",
		AgentID:   "a1",
		AgentRole: "builder",
	})
	if err != nil {
		t.Fatalf("NewBody: %v", err)
	}
	return body
}

// TestServePermission_RelaysAllow: the request reaches the host bus with
// every field intact, and the user's Allow crosses back as "allow".
func TestServePermission_RelaysAllow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, prompts, sess := startPermSession(t, ctx, permBody(t))

	pr := waitPrompt(t, prompts)
	if pr.ToolName != "bash" || pr.ArgString != "make test" || pr.Diff != "a-diff" ||
		pr.AgentID != "a1" || pr.AgentRole != "builder" {
		t.Fatalf("prompt fields did not survive the seam: %+v", pr)
	}
	if got, _ := pr.Args["cmd"].(string); got != "make test" {
		t.Fatalf("args[cmd] = %q, want %q", got, "make test")
	}
	pr.Respond <- permissions.Allow

	if d := waitReport(t, report); d != wire.PermAllow {
		t.Fatalf("worker received decision %q, want %q", d, wire.PermAllow)
	}
	if err := sess.Wait(); err != nil {
		t.Fatalf("session ended with error: %v", err)
	}
}

// TestServePermission_RelaysDeny mirrors the allow path for an explicit
// user Deny.
func TestServePermission_RelaysDeny(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, prompts, sess := startPermSession(t, ctx, permBody(t))

	pr := waitPrompt(t, prompts)
	pr.Respond <- permissions.Deny

	if d := waitReport(t, report); d != wire.PermDeny {
		t.Fatalf("worker received decision %q, want %q", d, wire.PermDeny)
	}
	if err := sess.Wait(); err != nil {
		t.Fatalf("session ended with error: %v", err)
	}
}

// TestServePermission_MalformedBodyDenies: a request the host cannot
// decode must be denied immediately and must never reach the prompt UI
// (there is nothing meaningful to ask the user).
func TestServePermission_MalformedBodyDenies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Valid JSON, but not a PermissionRequest object → decode error.
	report, prompts, sess := startPermSession(t, ctx, json.RawMessage(`"garbage"`))

	if d := waitReport(t, report); d != wire.PermDeny {
		t.Fatalf("malformed request resolved %q, want %q (fail-closed)", d, wire.PermDeny)
	}
	// sendDecision happens strictly after any would-be bus publish in
	// servePermission, so once the worker holds the deny an empty prompt
	// stream is conclusive.
	select {
	case pr := <-prompts:
		t.Fatalf("malformed request must not be published to the prompt UI, got %+v", pr)
	default:
	}
	if err := sess.Wait(); err != nil {
		t.Fatalf("session ended with error: %v", err)
	}
}

// TestServePermission_CtxCancelDenies: an unanswered prompt whose
// session ctx ends (host shutting down, run aborted) must auto-deny so
// the sandboxed agent is never left granted-by-silence or hung.
func TestServePermission_CtxCancelDenies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	report, prompts, sess := startPermSession(t, ctx, permBody(t))

	// The prompt is up on the host bus, but nobody ever answers.
	_ = waitPrompt(t, prompts)
	cancel()

	if d := waitReport(t, report); d != wire.PermDeny {
		t.Fatalf("ctx cancel resolved %q, want %q (fail-closed)", d, wire.PermDeny)
	}
	if err := sess.Wait(); err != nil {
		t.Fatalf("session ended with error: %v", err)
	}
}
