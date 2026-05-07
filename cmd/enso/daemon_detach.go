// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

// The daemon subsystem is POSIX-only (unix sockets, syscall.Kill for the
// pidfile liveness probe, syscall.Setsid for fork-and-detach), so there's
// no Windows companion file — the whole `enso daemon` path doesn't build
// on Windows today, by design.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/TaraTheStar/enso/internal/daemon"
)

// spawnDetachedDaemon re-execs `enso daemon` (without `--detach`) as a
// detached process and exits the parent. The child gets its own session
// (Setsid) so closing the controlling terminal doesn't kill it; stdio is
// redirected to /dev/null since slog already routes warnings to
// ~/.enso/enso.log.
//
// If a daemon is already running we say so and return cleanly — the
// pidfile lock would otherwise reject the new child silently (its stderr
// is /dev/null, so the user wouldn't see the error).
func spawnDetachedDaemon() error {
	if c, err := daemon.Dial(); err == nil {
		_ = c.Close()
		fmt.Println("daemon already running")
		return nil
	}

	// Replay every CLI argument except --detach onto the child. The child
	// is just `enso daemon ...` (foreground) so it'll fall into the
	// non-detach branch of daemonCmd.RunE.
	args := make([]string, 0, len(os.Args)-1)
	for _, a := range os.Args[1:] {
		if a == "--detach" {
			continue
		}
		args = append(args, a)
	}

	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/null: %w", err)
	}
	defer devnull.Close()

	cmd := exec.Command(os.Args[0], args...)
	cmd.Stdin = devnull
	cmd.Stdout = devnull
	cmd.Stderr = devnull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("fork daemon: %w", err)
	}
	socket, _ := daemon.SocketPath()
	fmt.Printf("daemon started (pid %d, socket %s)\n", cmd.Process.Pid, socket)

	// Don't Wait — the child is intentionally orphaned to init.
	_ = cmd.Process.Release()
	return nil
}
