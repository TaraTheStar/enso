// SPDX-License-Identifier: AGPL-3.0-or-later

// Package local implements the default Backend: the agent-core Worker
// runs as a host child process (`enso __worker`) with no container, no
// overlay, no network seal — exactly today's logical semantics, just
// hoisted behind the seam so there is one execution path. Channel
// transport is the child's stdio: the host writes envelopes to the
// child's stdin and reads them from its stdout. The child's stderr is
// inherited for crash diagnostics.
package local

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/TaraTheStar/enso/internal/backend"
)

// Backend is the no-isolation Backend. The zero value is usable; Exe
// overrides the worker binary (defaults to the running executable).
type Backend struct {
	Exe string
}

func (b *Backend) Name() string { return "local" }

// Start spawns `enso __worker` and wires its stdio as the Channel. The
// returned Worker is live; the caller sends MsgTaskSpec next (the host
// adapter does this).
func (b *Backend) Start(ctx context.Context, _ backend.TaskSpec) (backend.Worker, error) {
	exe := b.Exe
	if exe == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("local: resolve executable: %w", err)
		}
		exe = self
	}

	// Not CommandContext: ctx cancellation is handled via Teardown so
	// the worker gets an explicit, ordered shutdown rather than an
	// abrupt kill mid-frame.
	cmd := exec.Command(exe, "__worker")
	cmd.Stderr = os.Stderr // crash diagnostics surface to the user

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("local: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("local: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("local: start worker: %w", err)
	}

	w := &localWorker{cmd: cmd}
	w.ch = backend.NewStreamChannelRW(stdout, stdin, &pipePair{stdin: stdin, stdout: stdout})
	return w, nil
}

// pipePair closes the host's ends of the child stdio. Closing stdin
// sends the worker EOF on its Channel reader, the clean shutdown
// signal; closing stdout releases the read side.
type pipePair struct {
	stdin  io.Closer
	stdout io.Closer
}

func (p *pipePair) Close() error {
	_ = p.stdout.Close()
	return p.stdin.Close()
}

type localWorker struct {
	cmd  *exec.Cmd
	ch   backend.Channel
	once sync.Once
}

func (w *localWorker) Channel() backend.Channel { return w.ch }

// Wait blocks until the worker process exits or ctx is cancelled. A
// non-zero exit is returned as an error.
func (w *localWorker) Wait(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- w.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Teardown closes the Channel (EOF → worker winds down) then ensures
// the process is gone. Idempotent; safe after Wait.
func (w *localWorker) Teardown(context.Context) error {
	w.once.Do(func() {
		_ = w.ch.Close()
		if w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
			_ = w.cmd.Wait()
		}
	})
	return nil
}
