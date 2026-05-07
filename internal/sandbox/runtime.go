// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sandbox runs the bash tool inside a podman or docker container so
// the agent's shell can't escape the project directory. The container is
// per-project and persistent — created on first use, reused on subsequent
// enso runs in the same cwd, and recreated only when the user changes its
// configuration (image, init script, mounts, env).
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Runtime selects which container CLI to invoke. The CLIs are
// near-identical for the subset of commands we use, so a single string
// switch covers everything.
type Runtime string

const (
	RuntimeAuto   Runtime = "auto"
	RuntimePodman Runtime = "podman"
	RuntimeDocker Runtime = "docker"
)

// resolveRuntime returns the binary name to invoke. `RuntimeAuto`
// prefers podman (rootless by default) and falls back to docker.
// Errors out if neither is on PATH.
func resolveRuntime(r Runtime) (string, error) {
	switch r {
	case RuntimePodman:
		if _, err := exec.LookPath("podman"); err != nil {
			return "", fmt.Errorf("podman not found on PATH")
		}
		return "podman", nil
	case RuntimeDocker:
		if _, err := exec.LookPath("docker"); err != nil {
			return "", fmt.Errorf("docker not found on PATH")
		}
		return "docker", nil
	case "", RuntimeAuto:
		for _, name := range []string{"podman", "docker"} {
			if _, err := exec.LookPath(name); err == nil {
				return name, nil
			}
		}
		return "", errors.New("neither podman nor docker found on PATH")
	default:
		return "", fmt.Errorf("unknown runtime %q (want \"auto\", \"podman\", or \"docker\")", r)
	}
}

// runCapture runs the runtime CLI and captures stdout. Errors include
// stderr text so failures are debuggable from the slog audit trail.
func runCapture(ctx context.Context, runtime string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, runtime, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w (stderr: %s)", runtime, args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// runStreaming runs the runtime CLI and forwards stdout+stderr to `w`
// as bytes arrive. Used for `exec` so the model sees long-running tool
// output incrementally.
func runStreaming(ctx context.Context, w io.Writer, runtime string, args ...string) error {
	cmd := exec.CommandContext(ctx, runtime, args...)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}
