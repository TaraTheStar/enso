// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/TaraTheStar/enso/internal/backend/exestage"
	"github.com/TaraTheStar/enso/internal/backend/lima"
	"github.com/TaraTheStar/enso/internal/backend/podman"
	"github.com/TaraTheStar/enso/internal/session"
)

// flagPruneOlderThan restricts `enso prune` to instances at least this
// old (podman per-task workers by their enso.created label; lima VMs by
// instance-dir mtime). Zero = prune everything enso-managed.
var flagPruneOlderThan time.Duration

// pruneCmd reclaims enso-managed sandboxes across every project: stale
// per-task podman workers (+ their anonymous volumes) and the
// persistent per-project lima VMs (+ accumulated workspace review
// copies). Persistent lima VMs are NOT garbage by default — they are
// only reclaimed here, explicitly. Best-effort: a missing runtime or a
// per-instance failure is skipped, never fatal.
var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove enso-managed sandboxes (podman task workers + lima VMs) across every project",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Podman per-task workers + their anonymous volumes. A missing
		// podman/docker is not fatal — the lima sweep below still runs.
		if runtime, rerr := podman.ResolveRuntime("auto"); rerr != nil {
			fmt.Fprintf(os.Stderr, "skip podman sweep: %v\n", rerr)
		} else if n, err := podman.Sweep(ctx, runtime, flagPruneOlderThan); err != nil {
			fmt.Fprintf(os.Stderr, "sweep task workers: %v\n", err)
		} else if n > 0 {
			fmt.Printf("swept %d orphaned task worker(s) + volumes\n", n)
		}

		// Persistent per-project lima VMs (substrate reuse → only
		// reclaimed here, honouring --older-than). Skipped when limactl
		// is absent.
		if limactl, lerr := exec.LookPath("limactl"); lerr == nil {
			if n, err := lima.Sweep(ctx, limactl, flagPruneOlderThan); err != nil {
				fmt.Fprintf(os.Stderr, "sweep lima VMs: %v\n", err)
			} else if n > 0 {
				fmt.Printf("removed %d enso lima VM(s)\n", n)
			}
		}
		// Bound accumulated workspace review copies across every lima
		// project stage dir (independent of limactl availability).
		lima.SweepStageKept(os.Stdout)

		// Immutable staged enso binary snapshots (one per built binary
		// content ever run under an isolated backend). --older-than
		// keeps recently-used ones a persistent VM may still mount.
		if n, err := exestage.Sweep(flagPruneOlderThan); err != nil {
			fmt.Fprintf(os.Stderr, "sweep staged binaries: %v\n", err)
		} else if n > 0 {
			fmt.Printf("removed %d staged enso binary snapshot(s)\n", n)
		}

		// Orphaned per-turn /rewind checkpoint snapshots: a discarded
		// session's FK CASCADE drops the checkpoint rows but can't reach
		// the on-disk snapshot tree, so those dirs accumulate under
		// $XDG_STATE_HOME/enso/checkpoints until swept here.
		if n, err := session.SweepCheckpoints(flagPruneOlderThan); err != nil {
			fmt.Fprintf(os.Stderr, "sweep checkpoints: %v\n", err)
		} else if n > 0 {
			fmt.Printf("removed %d checkpoint snapshot(s)\n", n)
		}
		return nil
	},
}
