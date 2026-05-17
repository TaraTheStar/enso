// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/TaraTheStar/enso/internal/backend/lima"
	"github.com/TaraTheStar/enso/internal/backend/podman"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/sandbox"
)

// flagPruneOlderThan restricts `sandbox prune` to per-task workers at
// least this old (by their enso.created label). Zero = prune all.
var flagPruneOlderThan time.Duration

var sandboxCmd = &cobra.Command{
	Use:   "sandbox",
	Short: "Inspect and manage per-project bash-sandbox containers",
}

var sandboxListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all enso-managed containers across every project",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ms, err := sandbox.ListManaged(ctx, sandbox.RuntimeAuto)
		if err != nil {
			return err
		}
		if len(ms) == 0 {
			fmt.Println("(no enso-managed containers)")
			return nil
		}
		for _, m := range ms {
			state := "stopped"
			if m.Running {
				state = "running"
			}
			fmt.Printf("%s\t%s\t%s\t%s\n", m.Name, state, m.Image, m.Cwd)
		}
		return nil
	},
}

var sandboxStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the current project's sandbox container (does not remove it)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		mgr, err := projectSandboxManager()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mgr.Stop(ctx)
		fmt.Printf("stopped %s\n", mgr.ContainerName())
		return nil
	},
}

var sandboxRmCmd = &cobra.Command{
	Use:   "rm",
	Short: "Stop and remove the current project's sandbox container",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		mgr, err := projectSandboxManager()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := mgr.Remove(ctx); err != nil {
			return err
		}
		fmt.Printf("removed %s\n", mgr.ContainerName())
		return nil
	},
}

var sandboxPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove every enso-managed container across every project",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// Podman side (legacy per-project sandboxes + per-task workers).
		// A missing podman/docker is not fatal: the lima sweep below
		// must still run.
		if ms, err := sandbox.ListManaged(ctx, sandbox.RuntimeAuto); err != nil {
			fmt.Fprintf(os.Stderr, "list podman sandboxes: %v\n", err)
		} else if runtime, rerr := sandbox.ResolveRuntimeBinary(sandbox.RuntimeAuto); rerr != nil {
			fmt.Fprintf(os.Stderr, "resolve container runtime: %v\n", rerr)
		} else {
			for _, m := range ms {
				if err := sandbox.RemoveByName(ctx, runtime, m.Name); err != nil {
					fmt.Fprintf(os.Stderr, "rm %s: %v\n", m.Name, err)
					continue
				}
				fmt.Printf("removed %s\n", m.Name)
			}
			// Per-task workers (the Backend seam) carry an enso.task
			// label and own anonymous volumes the by-name rm above
			// doesn't drop. Sweep reaps terminal orphans + their
			// volumes, honouring the age threshold.
			if n, err := podman.Sweep(ctx, runtime, flagPruneOlderThan); err != nil {
				fmt.Fprintf(os.Stderr, "sweep task workers: %v\n", err)
			} else if n > 0 {
				fmt.Printf("swept %d orphaned task worker(s) + volumes\n", n)
			}
		}

		// Lima side: persistent per-project VMs. These are NOT garbage
		// by default (substrate reuse), so prune only removes them
		// here, explicitly, honouring --older-than (instance dir
		// mtime). Skipped silently when limactl is absent.
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
		return nil
	},
}

// projectSandboxManager builds a Manager from the current cwd's
// effective config — used by `sandbox stop` / `rm` so they target the
// same container the agent runtime would. Errors when bash.sandbox is
// "off" (nothing to manage).
func projectSandboxManager() (*sandbox.Manager, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(cwd, flagConfig)
	if err != nil {
		return nil, err
	}
	sbCfg, on := sandbox.FromConfig(cfg)
	if !on {
		return nil, errors.New("bash.sandbox is \"off\" in this project's config — nothing to manage")
	}
	return sandbox.NewManager(cwd, sbCfg)
}
