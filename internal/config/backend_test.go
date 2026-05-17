// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import "testing"

func TestResolveBackend(t *testing.T) {
	tests := []struct {
		name        string
		backendType string
		bashSandbox string
		want        BackendKind
	}{
		// Back-compat: no [backend] type, derive from [bash] sandbox.
		{"empty config defaults local", "", "", BackendLocal},
		{"sandbox off → local", "", "off", BackendLocal},
		{"sandbox auto → podman", "", "auto", BackendPodman},
		{"sandbox podman → podman", "", "podman", BackendPodman},
		{"sandbox docker → podman", "", "docker", BackendPodman},
		// Explicit [backend] type wins over the derived value.
		{"explicit local overrides sandbox", "local", "podman", BackendLocal},
		{"explicit podman overrides off", "podman", "off", BackendPodman},
		// Whitespace / case tolerance.
		{"case-insensitive", "PODMAN", "", BackendPodman},
		{"trims whitespace", "  local  ", "auto", BackendLocal},
		// Unknown explicit value fails safe to local (never silently
		// drops into a container).
		{"unknown type fails safe", "qubes", "podman", BackendLocal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{}
			c.Backend.Type = tt.backendType
			c.Bash.Sandbox = tt.bashSandbox
			if got := c.ResolveBackend(); got != tt.want {
				t.Fatalf("ResolveBackend() = %q, want %q", got, tt.want)
			}
		})
	}
}
