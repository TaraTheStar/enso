// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/sandbox"
)

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
		ms, err := sandbox.ListManaged(ctx, sandbox.RuntimeAuto)
		if err != nil {
			return err
		}
		if len(ms) == 0 {
			fmt.Println("(no enso-managed containers to prune)")
			return nil
		}
		// Resolve runtime once; reuse for each rm.
		runtime, err := sandbox.ResolveRuntimeBinary(sandbox.RuntimeAuto)
		if err != nil {
			return err
		}
		for _, m := range ms {
			if err := sandbox.RemoveByName(ctx, runtime, m.Name); err != nil {
				fmt.Fprintf(os.Stderr, "rm %s: %v\n", m.Name, err)
				continue
			}
			fmt.Printf("removed %s\n", m.Name)
		}
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
