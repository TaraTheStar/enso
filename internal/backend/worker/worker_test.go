// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/backend"
)

type noopCloser struct{}

func (noopCloser) Close() error { return nil }

func TestServeHandshakeAndStubLifecycle(t *testing.T) {
	// Two pipes: hostW->workerR and workerW->hostR.
	hostToWorkerR, hostToWorkerW := io.Pipe()
	workerToHostR, workerToHostW := io.Pipe()

	workerCh := backend.NewStreamChannelRW(hostToWorkerR, workerToHostW, noopCloser{})
	hostCh := backend.NewStreamChannelRW(workerToHostR, hostToWorkerW, noopCloser{})

	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), workerCh, nil) }()

	// Send the mandatory handshake envelope.
	body, err := backend.NewBody(backend.TaskSpec{TaskID: "t1", Cwd: "/tmp/p"})
	if err != nil {
		t.Fatalf("NewBody: %v", err)
	}
	if err := hostCh.Send(backend.Envelope{Kind: backend.MsgTaskSpec, Body: body}); err != nil {
		t.Fatalf("send task spec: %v", err)
	}

	ready, err := hostCh.Recv()
	if err != nil {
		t.Fatalf("recv ready: %v", err)
	}
	if ready.Kind != backend.MsgWorkerReady {
		t.Fatalf("got %q, want %q", ready.Kind, backend.MsgWorkerReady)
	}

	// Drive the stub to terminate.
	if err := hostCh.Send(backend.Envelope{Kind: backend.MsgShutdown}); err != nil {
		t.Fatalf("send shutdown: %v", err)
	}

	werr, err := hostCh.Recv()
	if err != nil {
		t.Fatalf("recv terminal: %v", err)
	}
	if werr.Kind != backend.MsgWorkerError {
		t.Fatalf("got %q, want %q (stub reports not-wired)", werr.Kind, backend.MsgWorkerError)
	}
	var eb backend.ErrorBody
	if err := json.Unmarshal(werr.Body, &eb); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if eb.Message == "" {
		t.Fatal("expected non-empty error message from stub")
	}

	select {
	case serveErr := <-done:
		if serveErr == nil {
			t.Fatal("Serve should return the stub's not-wired error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return")
	}
}

func TestServeRejectsWrongFirstEnvelope(t *testing.T) {
	hostToWorkerR, hostToWorkerW := io.Pipe()
	workerToHostR, workerToHostW := io.Pipe()
	workerCh := backend.NewStreamChannelRW(hostToWorkerR, workerToHostW, noopCloser{})
	hostCh := backend.NewStreamChannelRW(workerToHostR, hostToWorkerW, noopCloser{})

	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), workerCh, nil) }()

	if err := hostCh.Send(backend.Envelope{Kind: backend.MsgInput}); err != nil {
		t.Fatalf("send: %v", err)
	}
	msg, err := hostCh.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if msg.Kind != backend.MsgWorkerError {
		t.Fatalf("got %q, want %q", msg.Kind, backend.MsgWorkerError)
	}
	if serveErr := <-done; serveErr == nil {
		t.Fatal("Serve should error on a non-task-spec first envelope")
	}
}
