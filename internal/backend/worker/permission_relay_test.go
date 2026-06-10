// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/wire"
	"github.com/TaraTheStar/enso/internal/permissions"
)

// These tests cover the worker leg of the cross-seam permission relay
// (proxyPermission / routePermission / deliverPermission): the security
// boundary that decides whether a sandboxed agent's tool call may run.
// The contract is fail-closed — anything but an explicit, well-formed
// "allow" decision must resolve to Deny.

// newPermSeam returns a seam wired to workerCh with just the permission
// plumbing initialized (the only state these paths touch).
func newPermSeam(ch backend.Channel) *seam {
	return &seam{ch: ch, pending: map[string]chan permissions.Decision{}}
}

// recvEnv receives one envelope from ch with a timeout, failing the test
// on transport error.
func recvEnv(t *testing.T, ch backend.Channel) backend.Envelope {
	t.Helper()
	type res struct {
		env backend.Envelope
		err error
	}
	c := make(chan res, 1)
	go func() { e, err := ch.Recv(); c <- res{e, err} }()
	select {
	case r := <-c:
		if r.err != nil {
			t.Fatalf("recv: %v", r.err)
		}
		return r.env
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for envelope")
		return backend.Envelope{}
	}
}

// wantDecision asserts the agent's Respond channel resolves to want.
func wantDecision(t *testing.T, respond <-chan permissions.Decision, want permissions.Decision) {
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

func pendingCount(s *seam) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// failSendChannel is a Channel whose Send always fails — the wire shape
// of a dead host pipe.
type failSendChannel struct{}

func (failSendChannel) Send(backend.Envelope) error { return errors.New("send failed") }
func (failSendChannel) Recv() (backend.Envelope, error) {
	return backend.Envelope{}, errors.New("closed")
}
func (failSendChannel) Close() error { return nil }

// TestProxyPermission_AllowRoundTrip proves the happy path: the prompt
// crosses the seam with all fields intact, and an explicit "allow"
// decision resolves the agent's Respond chan to Allow.
func TestProxyPermission_AllowRoundTrip(t *testing.T) {
	workerCh, hostCh := newChannelPair()
	s := newPermSeam(workerCh)

	respond := make(chan permissions.Decision, 1)
	pr := &permissions.PromptRequest{
		ToolName:  "bash",
		ArgString: "rm -rf build",
		Args:      map[string]any{"cmd": "rm -rf build"},
		Diff:      "a-diff",
		AgentID:   "a1",
		AgentRole: "reviewer",
		Respond:   respond,
	}
	go s.proxyPermission(pr)

	env := recvEnv(t, hostCh)
	if env.Kind != backend.MsgPermissionRequest {
		t.Fatalf("kind = %q, want %q", env.Kind, backend.MsgPermissionRequest)
	}
	if env.Corr == "" {
		t.Fatal("permission request must carry a correlation id")
	}
	var pw wire.PermissionRequest
	if err := json.Unmarshal(env.Body, &pw); err != nil {
		t.Fatalf("decode permission request: %v", err)
	}
	if pw.Tool != "bash" || pw.ArgString != "rm -rf build" || pw.Diff != "a-diff" ||
		pw.AgentID != "a1" || pw.AgentRole != "reviewer" {
		t.Fatalf("request fields did not survive the seam: %+v", pw)
	}
	if got, _ := pw.Args["cmd"].(string); got != "rm -rf build" {
		t.Fatalf("args[cmd] = %q, want %q", got, "rm -rf build")
	}

	body, _ := backend.NewBody(wire.PermissionDecision{Decision: wire.PermAllow})
	s.routePermission(backend.Envelope{Kind: backend.MsgPermissionDecision, Corr: env.Corr, Body: body})

	wantDecision(t, respond, permissions.Allow)
	if n := pendingCount(s); n != 0 {
		t.Fatalf("pending map not drained after delivery: %d entries", n)
	}
}

// TestRoutePermission_DenyAndGarbageDeny is the fail-closed core: an
// explicit "deny" denies, and so does EVERY malformed decision — wrong
// string, wrong case, empty body, undecodable JSON. Only the exact
// wire.PermAllow string may allow.
func TestRoutePermission_DenyAndGarbageDeny(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"explicit deny", []byte(`{"decision":"deny"}`)},
		{"unknown verb", []byte(`{"decision":"approve"}`)},
		{"wrong case", []byte(`{"decision":"ALLOW"}`)},
		{"empty object", []byte(`{}`)},
		{"null body", []byte(`null`)},
		{"undecodable body", []byte(`"garbage"`)},
		{"missing body", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workerCh, hostCh := newChannelPair()
			s := newPermSeam(workerCh)

			respond := make(chan permissions.Decision, 1)
			go s.proxyPermission(&permissions.PromptRequest{ToolName: "edit", Respond: respond})

			env := recvEnv(t, hostCh)
			s.routePermission(backend.Envelope{
				Kind: backend.MsgPermissionDecision, Corr: env.Corr, Body: tc.body,
			})
			wantDecision(t, respond, permissions.Deny)
		})
	}
}

// TestDeliverPermission_UnknownAndDuplicateCorrNoOp: a decision for a
// corr we never issued (or already resolved) must be silently dropped —
// it must neither panic nor re-resolve an already-answered prompt.
func TestDeliverPermission_UnknownAndDuplicateCorrNoOp(t *testing.T) {
	workerCh, hostCh := newChannelPair()
	s := newPermSeam(workerCh)

	// Unknown corr before any request exists: no-op.
	s.deliverPermission("perm-never-issued", permissions.Allow)

	respond := make(chan permissions.Decision, 1)
	go s.proxyPermission(&permissions.PromptRequest{ToolName: "bash", Respond: respond})
	env := recvEnv(t, hostCh)

	// First decision wins (Deny)...
	denyBody, _ := backend.NewBody(wire.PermissionDecision{Decision: wire.PermDeny})
	s.routePermission(backend.Envelope{Kind: backend.MsgPermissionDecision, Corr: env.Corr, Body: denyBody})
	wantDecision(t, respond, permissions.Deny)

	// ...a duplicate (this time an Allow!) for the same corr is a no-op:
	// it must not deliver a second, contradictory decision.
	allowBody, _ := backend.NewBody(wire.PermissionDecision{Decision: wire.PermAllow})
	s.routePermission(backend.Envelope{Kind: backend.MsgPermissionDecision, Corr: env.Corr, Body: allowBody})
	select {
	case d := <-respond:
		t.Fatalf("duplicate decision was delivered: %v", d)
	case <-time.After(50 * time.Millisecond):
		// expected: nothing arrives
	}
	if n := pendingCount(s); n != 0 {
		t.Fatalf("pending entries leaked: %d", n)
	}
}

// TestProxyPermission_MarshalFailureDenies: if the request body cannot
// be serialized the prompt can never reach the host — the agent must be
// denied immediately, not left hanging (and certainly not allowed).
func TestProxyPermission_MarshalFailureDenies(t *testing.T) {
	workerCh, _ := newChannelPair()
	s := newPermSeam(workerCh)

	respond := make(chan permissions.Decision, 1)
	pr := &permissions.PromptRequest{
		ToolName: "bash",
		// A chan is not JSON-serializable: backend.NewBody must fail.
		Args:    map[string]any{"bad": make(chan int)},
		Respond: respond,
	}
	// Goroutine on purpose: if the marshal unexpectedly succeeded the
	// pipe send would block forever and hang the test instead of failing.
	go s.proxyPermission(pr)

	wantDecision(t, respond, permissions.Deny)
	if n := pendingCount(s); n != 0 {
		t.Fatalf("pending entries leaked after marshal failure: %d", n)
	}
}

// TestProxyPermission_SendFailureDenies: a dead Channel (host gone)
// must resolve Deny, never hang the agent loop or default open.
func TestProxyPermission_SendFailureDenies(t *testing.T) {
	s := newPermSeam(failSendChannel{})

	respond := make(chan permissions.Decision, 1)
	s.proxyPermission(&permissions.PromptRequest{ToolName: "bash", Respond: respond})

	wantDecision(t, respond, permissions.Deny)
	if n := pendingCount(s); n != 0 {
		t.Fatalf("pending entries leaked after send failure: %d", n)
	}
}
