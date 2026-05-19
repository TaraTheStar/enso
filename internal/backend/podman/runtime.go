// SPDX-License-Identifier: AGPL-3.0-or-later

package podman

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ResolveRuntime is the exported entry point for callers outside the
// package (e.g. the `enso prune` command) that need the same
// podman/docker resolution the backend uses.
func ResolveRuntime(sel string) (string, error) { return resolveRuntimeBinary(sel) }

// resolveRuntimeBinary returns the container CLI to invoke for the
// configured [backend.podman] runtime selector. "auto" (or "") prefers
// podman (rootless, no daemon) and falls back to docker; "podman" /
// "docker" pin one. Errors if the requested binary is not on PATH.
// (Previously lived in internal/sandbox; inlined here when that legacy
// package was removed so the podman backend owns its own resolution.)
func resolveRuntimeBinary(sel string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(sel)) {
	case "podman":
		if _, err := exec.LookPath("podman"); err != nil {
			return "", fmt.Errorf("podman not found on PATH")
		}
		return "podman", nil
	case "docker":
		if _, err := exec.LookPath("docker"); err != nil {
			return "", fmt.Errorf("docker not found on PATH")
		}
		return "docker", nil
	case "", "auto":
		for _, name := range []string{"podman", "docker"} {
			if _, err := exec.LookPath(name); err == nil {
				return name, nil
			}
		}
		return "", errors.New("neither podman nor docker found on PATH")
	default:
		return "", fmt.Errorf("unknown runtime %q (want \"auto\", \"podman\", or \"docker\")", sel)
	}
}
