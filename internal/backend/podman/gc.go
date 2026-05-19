// SPDX-License-Identifier: AGPL-3.0-or-later

package podman

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Per-task containers are labelled so they can be reclaimed even when
// the owning enso process is gone:
//
//	enso.managed=true   every enso-created container
//	enso.task=<id>      marks the per-task Worker container
//	enso.created=<unix> creation time, for age-thresholded pruning
//
// Every enso podman container is a per-task `--rm` Worker (the legacy
// persistent per-project sandbox was removed); GC reclaims terminal
// orphans left when a SIGKILLed run's `--rm` never fired.

var sweepOnce sync.Once

// startupSweep runs Sweep at most once per process, best-effort. It is
// called from Backend.Start so a previous run that was SIGKILLed (so
// `--rm` never fired) gets its dead container + anonymous volumes
// reclaimed on the next launch. Only TERMINAL containers are touched
// (exited/dead) — never running or freshly "created" ones, so a
// concurrent task on the same project is safe.
func startupSweep(runtime string) {
	sweepOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = Sweep(ctx, runtime, 0)
	})
}

// Sweep removes orphaned per-task worker containers (enso.task label) in
// a terminal state, plus dangling anonymous volumes they left behind.
// olderThan>0 additionally restricts removal to containers whose
// enso.created timestamp is at least that old (the manual
// `enso prune --older-than` backstop); 0 removes every terminal
// orphan. Running containers are never touched. Returns how many
// containers were removed. Best-effort: a per-container failure is
// skipped, not fatal.
func Sweep(ctx context.Context, runtime string, olderThan time.Duration) (int, error) {
	// State is filtered in Go, not via `--filter status=`: the valid
	// status values diverge across runtimes (podman has no `dead`; a
	// `status=dead` filter makes `podman ps` itself exit 125 and abort
	// the whole sweep). Asking for every enso.task container and judging
	// terminality from .State here is runtime-agnostic and keeps the
	// "never touch a running container" invariant intact.
	out, err := exec.CommandContext(ctx, runtime,
		"ps", "-a",
		"--filter", "label=enso.task",
		"--format", `{{.Names}}|{{.State}}|{{ index .Labels "enso.created" }}`,
	).Output()
	if err != nil {
		return 0, err
	}
	// Terminal (reclaimable) states across podman & docker. Anything
	// else — running, paused, created, removing, … — is left alone.
	terminal := map[string]bool{"exited": true, "stopped": true, "dead": true}
	cutoff := time.Time{}
	if olderThan > 0 {
		cutoff = time.Now().Add(-olderThan)
	}
	removed := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, rest, _ := strings.Cut(line, "|")
		state, created, _ := strings.Cut(rest, "|")
		if name == "" || !terminal[strings.TrimSpace(state)] {
			continue
		}
		if !cutoff.IsZero() {
			if sec, e := strconv.ParseInt(strings.TrimSpace(created), 10, 64); e == nil {
				if time.Unix(sec, 0).After(cutoff) {
					continue // too young to prune
				}
			}
			// Unparseable/missing enso.created → unknown age; it's
			// already terminal, so let it be reclaimed.
		}
		// -v also drops the container's anonymous volumes (the overlay
		// upper-dir / workspace volumes the overlay attaches).
		rmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := exec.CommandContext(rmCtx, runtime, "rm", "-f", "-v", name).Run(); err == nil {
			removed++
		}
		cancel()
	}
	// Reap volumes that outlived their container (crash between
	// container reap and volume reap).
	pvCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	_ = exec.CommandContext(pvCtx, runtime,
		"volume", "prune", "-f", "--filter", "label=enso.managed=true",
	).Run()
	cancel()
	return removed, nil
}
