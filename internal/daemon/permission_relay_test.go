// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/permissions"
)

// These tests cover the daemon leg of the permission relay
// (proxyPermission / resolvePermission / onPermissionResponse): the
// fan-out to attached clients, the fail-closed decision mapping, and
// the permission_timeout auto-deny.

// newPermServer constructs a minimal Server + registered sessionState,
// mirroring how the other daemon unit tests build a Server directly.
func newPermServer(timeoutSecs int) (*Server, *sessionState) {
	srv := &Server{
		cfg:      &config.Config{Daemon: config.DaemonConfig{PermissionTimeout: timeoutSecs}},
		sessions: map[string]*sessionState{},
	}
	st := &sessionState{
		id:           "s1",
		pendingPerms: map[string]*pendingPerm{},
		server:       srv,
	}
	srv.sessions[st.id] = st
	return srv, st
}

func newPromptRequest() (*permissions.PromptRequest, chan permissions.Decision) {
	respond := make(chan permissions.Decision, 1)
	return &permissions.PromptRequest{
		ToolName:  "bash",
		Args:      map[string]any{"cmd": "make test"},
		Diff:      "a-diff",
		AgentID:   "a1",
		AgentRole: "builder",
		Respond:   respond,
	}, respond
}

func wantDaemonDecision(t *testing.T, respond <-chan permissions.Decision, want permissions.Decision) {
	t.Helper()
	select {
	case d := <-respond:
		if d != want {
			t.Fatalf("decision = %v, want %v", d, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for permission decision")
	}
}

// recvPermEvent pulls the fanned-out PermissionRequest wire event from a
// subscriber channel and decodes its payload.
func recvPermEvent(t *testing.T, events <-chan Event) PermissionRequestPayload {
	t.Helper()
	select {
	case evt := <-events:
		if evt.Type != "PermissionRequest" {
			t.Fatalf("event type = %q, want PermissionRequest", evt.Type)
		}
		var p PermissionRequestPayload
		if err := json.Unmarshal(evt.Payload, &p); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the fanned-out permission event")
		return PermissionRequestPayload{}
	}
}

func pendingPermCount(st *sessionState) int {
	st.permsMu.Lock()
	defer st.permsMu.Unlock()
	return len(st.pendingPerms)
}

// TestDaemonProxyPermission_NoSubscribersDenies: with nobody attached
// there is no UI to ask — deny immediately rather than hanging the
// agent until the timeout.
func TestDaemonProxyPermission_NoSubscribersDenies(t *testing.T) {
	_, st := newPermServer(30)
	pr, respond := newPromptRequest()

	st.proxyPermission(pr, false)
	wantDaemonDecision(t, respond, permissions.Deny)
}

// TestDaemonProxyPermission_YoloAllows: the yolo plumbing short-circuits
// to Allow without fanning anything out.
func TestDaemonProxyPermission_YoloAllows(t *testing.T) {
	_, st := newPermServer(30)
	pr, respond := newPromptRequest()

	st.proxyPermission(pr, true)
	wantDaemonDecision(t, respond, permissions.Allow)
}

// TestDaemonProxyPermission_AllowRoundTrip: the request fans out to the
// subscriber with all fields + a deadline, and the client's explicit
// "allow" resolves Allow via onPermissionResponse.
func TestDaemonProxyPermission_AllowRoundTrip(t *testing.T) {
	srv, st := newPermServer(30)
	events, _ := st.subscribe(0)
	pr, respond := newPromptRequest()

	before := time.Now()
	st.proxyPermission(pr, false)

	p := recvPermEvent(t, events)
	if p.RequestID == "" {
		t.Fatal("fanned-out request must carry a request id")
	}
	if p.Tool != "bash" || p.Diff != "a-diff" || p.AgentID != "a1" || p.AgentRole != "builder" {
		t.Fatalf("payload fields did not survive: %+v", p)
	}
	if got, _ := p.Args["cmd"].(string); got != "make test" {
		t.Fatalf("args[cmd] = %q, want %q", got, "make test")
	}
	// The wire deadline must reflect the configured permission_timeout
	// (what the client renders must match what the server enforces).
	if p.Deadline.Before(before.Add(25*time.Second)) || p.Deadline.After(before.Add(35*time.Second)) {
		t.Fatalf("deadline %v not ~30s from %v", p.Deadline, before)
	}

	if err := srv.onPermissionResponse(PermissionResponseReq{
		SessionID: st.id, RequestID: p.RequestID, Decision: PermissionAllow,
	}); err != nil {
		t.Fatalf("onPermissionResponse: %v", err)
	}
	wantDaemonDecision(t, respond, permissions.Allow)
	if n := pendingPermCount(st); n != 0 {
		t.Fatalf("pendingPerms not drained: %d entries", n)
	}
}

// TestDaemonOnPermissionResponse_GarbageDenies: anything other than the
// exact "allow" string — wrong verb, wrong case, empty — must resolve
// Deny (fail-closed; an earlier int encoding defaulted to Allow).
func TestDaemonOnPermissionResponse_GarbageDenies(t *testing.T) {
	for _, garbage := range []string{"", "ALLOW", "approve", "yes"} {
		t.Run("decision="+garbage, func(t *testing.T) {
			srv, st := newPermServer(30)
			events, _ := st.subscribe(0)
			pr, respond := newPromptRequest()

			st.proxyPermission(pr, false)
			p := recvPermEvent(t, events)

			if err := srv.onPermissionResponse(PermissionResponseReq{
				SessionID: st.id, RequestID: p.RequestID, Decision: garbage,
			}); err != nil {
				t.Fatalf("onPermissionResponse: %v", err)
			}
			wantDaemonDecision(t, respond, permissions.Deny)
		})
	}
}

// TestDaemonOnPermissionResponse_UnknownSessionErrors: a response for a
// session the daemon doesn't host is rejected, not silently dropped.
func TestDaemonOnPermissionResponse_UnknownSessionErrors(t *testing.T) {
	srv, _ := newPermServer(30)
	err := srv.onPermissionResponse(PermissionResponseReq{
		SessionID: "nope", RequestID: "r1", Decision: PermissionAllow,
	})
	if err == nil {
		t.Fatal("unknown session must return an error")
	}
}

// TestDaemonResolvePermission_UnknownAndDuplicateNoOp: resolving an
// unknown id is a no-op, and only the FIRST resolution for an id is
// delivered — a late timeout-deny racing a real client answer (or a
// duplicate client frame) must not flip the decision.
func TestDaemonResolvePermission_UnknownAndDuplicateNoOp(t *testing.T) {
	srv, st := newPermServer(30)
	events, _ := st.subscribe(0)

	// Unknown id: no panic, nothing delivered.
	st.resolvePermission("never-issued", permissions.Allow)

	pr, respond := newPromptRequest()
	st.proxyPermission(pr, false)
	p := recvPermEvent(t, events)

	if err := srv.onPermissionResponse(PermissionResponseReq{
		SessionID: st.id, RequestID: p.RequestID, Decision: PermissionDeny,
	}); err != nil {
		t.Fatalf("onPermissionResponse: %v", err)
	}
	wantDaemonDecision(t, respond, permissions.Deny)

	// Duplicate (now an allow!) for the same id: must be a no-op.
	st.resolvePermission(p.RequestID, permissions.Allow)
	select {
	case d := <-respond:
		t.Fatalf("duplicate resolution was delivered: %v", d)
	case <-time.After(50 * time.Millisecond):
		// expected: nothing arrives
	}
}

// TestDaemonProxyPermission_TimeoutAutoDeny: with a subscriber attached
// but silent, the configured permission_timeout must auto-deny so the
// agent goroutine never hangs on a disappeared client.
func TestDaemonProxyPermission_TimeoutAutoDeny(t *testing.T) {
	_, st := newPermServer(1) // 1s: the smallest configurable budget
	events, _ := st.subscribe(0)
	pr, respond := newPromptRequest()

	st.proxyPermission(pr, false)
	_ = recvPermEvent(t, events) // prompt reached the client; nobody answers

	wantDaemonDecision(t, respond, permissions.Deny)
	if n := pendingPermCount(st); n != 0 {
		t.Fatalf("pendingPerms not drained after timeout: %d entries", n)
	}
}

// TestDaemonResolvePermission_StopsAutoDenyTimer: answering a prompt must
// disarm its auto-deny timer. The old implementation parked a goroutine
// in time.Sleep for the full budget after every answered prompt (and
// abandoned them at shutdown); with AfterFunc the timer must already be
// stopped by the time resolvePermission returns.
func TestDaemonResolvePermission_StopsAutoDenyTimer(t *testing.T) {
	srv, st := newPermServer(30)
	events, _ := st.subscribe(0)
	pr, respond := newPromptRequest()

	st.proxyPermission(pr, false)
	p := recvPermEvent(t, events)

	// Grab the armed timer before resolving (the entry is deleted after).
	st.permsMu.Lock()
	pend := st.pendingPerms[p.RequestID]
	st.permsMu.Unlock()
	if pend == nil || pend.timer == nil {
		t.Fatal("pending entry must carry the auto-deny timer")
	}

	if err := srv.onPermissionResponse(PermissionResponseReq{
		SessionID: st.id, RequestID: p.RequestID, Decision: PermissionAllow,
	}); err != nil {
		t.Fatalf("onPermissionResponse: %v", err)
	}
	wantDaemonDecision(t, respond, permissions.Allow)

	// Stop() == false means the timer was already stopped — it cannot
	// have fired on its own with a 30s budget. Stop() == true would mean
	// resolution left it armed, i.e. a late deny was still scheduled.
	if pend.timer.Stop() {
		t.Fatal("auto-deny timer still armed after resolution")
	}
}

// TestDaemonProxyPermission_MarshalFailureDenies: if the payload cannot
// be serialized the prompt can never reach a client — deny immediately.
func TestDaemonProxyPermission_MarshalFailureDenies(t *testing.T) {
	_, st := newPermServer(30)
	_, _ = st.subscribe(0) // subscriber present, so we get past hasSubs

	respond := make(chan permissions.Decision, 1)
	pr := &permissions.PromptRequest{
		ToolName: "bash",
		// A chan is not JSON-serializable: json.Marshal must fail.
		Args:    map[string]any{"bad": make(chan int)},
		Respond: respond,
	}
	st.proxyPermission(pr, false)
	wantDaemonDecision(t, respond, permissions.Deny)
}
