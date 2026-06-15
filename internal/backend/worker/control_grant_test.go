// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/agent"
	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/wire"
	"github.com/TaraTheStar/enso/internal/permissions"
)

// TestServeControl_AddAllowGrants is the U1 regression: the modal's
// "always"/"turn" grants reach the worker's REAL enforcing checker over
// the seam (CtrlAddAllow / CtrlAddTurnAllow), mirroring how /yolo
// (CtrlSetYolo) toggles it. Before the fix there was no wire verb at all
// and the grants were silently dead on the default local path.
func TestServeControl_AddAllowGrants(t *testing.T) {
	// Deny-by-default so a successful grant is observable: Check flips
	// from Deny to Allow only because the rule was applied.
	checker := permissions.NewChecker(nil, nil, nil, "deny")
	bashArgs := map[string]any{"cmd": "ls -la"}

	if d, _ := checker.Check("bash", bashArgs, nil); d != permissions.Deny {
		t.Fatalf("precondition: bash should be denied, got %v", d)
	}

	workerCh, hostCh := newChannelPair()
	// agt is non-nil only to clear serveControl's readiness guard; these
	// two verbs touch s.checker exclusively and never call the agent.
	s := withWriter(&seam{ch: workerCh, agt: &agent.Agent{}, checker: checker})

	call := func(method, pattern string) wire.ControlResponse {
		t.Helper()
		args, _ := backend.NewBody(wire.ControlName{Name: pattern})
		body, _ := backend.NewBody(wire.ControlRequest{Method: method, Args: args})
		// serveControl writes the response synchronously over the pipe,
		// which blocks until the host reads — so run it concurrently.
		go s.serveControl(context.Background(), backend.Envelope{
			Kind: backend.MsgControlRequest, Corr: "c1", Body: body,
		})
		got := recvControlResp(t, hostCh)
		return got
	}

	// "always" → persistent allow on the worker checker.
	if resp := call(wire.CtrlAddAllow, permissions.DerivePattern("bash", bashArgs, "")); resp.Error != "" {
		t.Fatalf("CtrlAddAllow returned error: %s", resp.Error)
	}
	if d, _ := checker.Check("bash", bashArgs, nil); d != permissions.Allow {
		t.Fatalf("after CtrlAddAllow, bash should be allowed, got %v", d)
	}

	// "turn" → turn-scoped allow on a different tool so we don't conflate
	// it with the persistent grant above.
	if checker.HasTurnAllows() {
		t.Fatalf("precondition: no turn allows yet")
	}
	if resp := call(wire.CtrlAddTurnAllow, "read(**)"); resp.Error != "" {
		t.Fatalf("CtrlAddTurnAllow returned error: %s", resp.Error)
	}
	if !checker.HasTurnAllows() {
		t.Fatalf("after CtrlAddTurnAllow, expected an active turn grant")
	}
	if d, _ := checker.Check("read", map[string]any{"path": "/etc/hosts"}, nil); d != permissions.Allow {
		t.Fatalf("after CtrlAddTurnAllow, read should be allowed, got %v", d)
	}

	// A malformed pattern is reported as an Error (not silently swallowed).
	if resp := call(wire.CtrlAddAllow, "this is not a pattern"); resp.Error == "" {
		t.Fatalf("CtrlAddAllow with a malformed pattern should error")
	}
}

func recvControlResp(t *testing.T, hostCh backend.Channel) wire.ControlResponse {
	t.Helper()
	type res struct {
		env backend.Envelope
		err error
	}
	ch := make(chan res, 1)
	go func() { e, err := hostCh.Recv(); ch <- res{e, err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("recv control response: %v", r.err)
		}
		if r.env.Kind != backend.MsgControlResponse {
			t.Fatalf("expected MsgControlResponse, got %v", r.env.Kind)
		}
		var resp wire.ControlResponse
		if err := json.Unmarshal(r.env.Body, &resp); err != nil {
			t.Fatalf("decode control response: %v", err)
		}
		return resp
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for control response")
		return wire.ControlResponse{}
	}
}
