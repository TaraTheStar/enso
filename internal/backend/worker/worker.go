// SPDX-License-Identifier: AGPL-3.0-or-later

// Package worker is the worker-side half of the Backend seam: the code
// that runs inside the `enso __worker` child process. It owns the
// Channel handshake and lifecycle loop; the agent-core wiring (mapping
// the Channel onto the agent's chan-string input, *bus.Bus output,
// llm.ChatClient inference, and permission round-trips) is attached via
// AgentFunc so the lifecycle can be built and tested independently of
// the core lift.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/TaraTheStar/enso/internal/backend"
)

// AgentFunc runs the agent core for one task. It is handed the decoded
// TaskSpec and the live Channel (handshake already done: MsgTaskSpec
// consumed, MsgWorkerReady already sent). It must:
//
//   - feed MsgInput envelopes into the agent's input,
//   - publish the agent's bus events out as MsgEvent,
//   - service MsgCancel,
//   - return when the input stream ends (non-interactive) or ctx is
//     cancelled / MsgShutdown arrives.
//
// Returning nil means a clean run (worker sends MsgWorkerDone); a
// non-nil error is reported as MsgWorkerError. The default
// implementation is a stub (errNotWired); the real adapter is attached
// by the core lift.
type AgentFunc func(ctx context.Context, spec backend.TaskSpec, ch backend.Channel) error

var errNotWired = errors.New("worker: agent core not wired (pending core lift)")

// stubAgent is the placeholder AgentFunc. It performs the lifecycle
// contract honestly without an agent: it drains the Channel until
// shutdown so the handshake/loop is exercisable end-to-end, then
// reports that the core is not yet attached.
func stubAgent(ctx context.Context, _ backend.TaskSpec, ch backend.Channel) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		env, err := ch.Recv()
		if err != nil {
			return nil // host closed the channel; clean shutdown
		}
		if env.Kind == backend.MsgShutdown {
			return errNotWired
		}
	}
}

// Serve runs the worker-side seam over ch: it performs the handshake
// (consume MsgTaskSpec, send MsgWorkerReady), invokes agent (or the
// stub if nil), and reports terminal status (MsgWorkerDone /
// MsgWorkerError). It returns the same error it reported, so the
// process exit code can mirror it.
func Serve(ctx context.Context, ch backend.Channel, agent AgentFunc) error {
	if agent == nil {
		agent = stubAgent
	}

	spec, err := handshake(ch)
	if err != nil {
		_ = sendError(ch, fmt.Sprintf("handshake: %v", err))
		return err
	}
	if err := ch.Send(backend.Envelope{Kind: backend.MsgWorkerReady}); err != nil {
		return fmt.Errorf("worker: send ready: %w", err)
	}

	runErr := agent(ctx, spec, ch)
	if runErr != nil {
		_ = sendError(ch, runErr.Error())
		return runErr
	}
	if err := ch.Send(backend.Envelope{Kind: backend.MsgWorkerDone}); err != nil {
		return fmt.Errorf("worker: send done: %w", err)
	}
	return nil
}

// handshake reads the mandatory first envelope, which must be
// MsgTaskSpec, and decodes the TaskSpec.
func handshake(ch backend.Channel) (backend.TaskSpec, error) {
	env, err := ch.Recv()
	if err != nil {
		return backend.TaskSpec{}, fmt.Errorf("recv task spec: %w", err)
	}
	if env.Kind != backend.MsgTaskSpec {
		return backend.TaskSpec{}, fmt.Errorf("expected %q as first envelope, got %q", backend.MsgTaskSpec, env.Kind)
	}
	var spec backend.TaskSpec
	if err := json.Unmarshal(env.Body, &spec); err != nil {
		return backend.TaskSpec{}, fmt.Errorf("decode task spec: %w", err)
	}
	return spec, nil
}

func sendError(ch backend.Channel, msg string) error {
	body, err := backend.NewBody(backend.ErrorBody{Message: msg})
	if err != nil {
		return err
	}
	return ch.Send(backend.Envelope{Kind: backend.MsgWorkerError, Body: body})
}
