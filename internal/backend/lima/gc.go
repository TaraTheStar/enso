// SPDX-License-Identifier: AGPL-3.0-or-later

package lima

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// GC policy differs intentionally from podman's. Podman workers are
// PER-TASK and short-lived, so any terminal one is an orphan and a
// startup sweep reaps it aggressively. Lima VMs are PERSISTENT and
// PER-PROJECT by design (the locked substrate decision): a Stopped enso
// VM is the carried-forward substrate, NOT garbage. So the startup
// sweep is deliberately a no-op — reclaiming a persistent VM is only
// ever explicit, via `enso sandbox prune` (age-thresholded).
//
// enso VMs are identified by the `enso-` name prefix (Lima has no
// label concept like podman); age is the instance dir's mtime.

var sweepOnce sync.Once

// startupSweep is intentionally inert for lima: a persistent
// per-project VM must survive host reboots and idle gaps between
// sessions (that is the whole point of substrate reuse). Kept for
// signature parity with podman; reclamation is the manual,
// age-thresholded `enso sandbox prune` path only.
func startupSweep(_ string) {
	sweepOnce.Do(func() {})
}

// Sweep stops and deletes enso-managed Lima VMs (the `enso-` name
// prefix). olderThan>0 restricts removal to instances whose instance
// dir has not been modified within that window (stale projects); 0
// removes every enso VM. It is the `enso sandbox prune` backstop —
// never called implicitly, because persistent VMs are not garbage.
// Best-effort: a per-VM failure is skipped, not fatal. Returns how many
// VMs were deleted.
func Sweep(ctx context.Context, limactl string, olderThan time.Duration) (int, error) {
	out, err := exec.CommandContext(ctx, limactl,
		"list", "--format", "{{.Name}}\t{{.Dir}}").Output()
	if err != nil {
		return 0, err
	}
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
		name, dir, _ := strings.Cut(line, "\t")
		name = strings.TrimSpace(name)
		if !strings.HasPrefix(name, "enso-") {
			continue // never touch a user's own VMs
		}
		if !cutoff.IsZero() {
			if fi, e := os.Stat(strings.TrimSpace(dir)); e == nil {
				if fi.ModTime().After(cutoff) {
					continue // too recently active to prune
				}
			}
			// Unstattable dir → unknown age; let it be reclaimed.
		}
		sCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_ = exec.CommandContext(sCtx, limactl, "stop", "--force", name).Run()
		if err := exec.CommandContext(sCtx, limactl, "delete", "--force", name).Run(); err == nil {
			removed++
		}
		cancel()
	}
	return removed, nil
}
