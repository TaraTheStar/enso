// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import "testing"

func TestResolveBackend(t *testing.T) {
	tests := []struct {
		name        string
		backendType string
		want        BackendKind
	}{
		// [backend] type is the ONLY backend selector.
		{"empty config defaults local", "", BackendLocal},
		{"explicit local", "local", BackendLocal},
		{"explicit podman", "podman", BackendPodman},
		{"explicit lima", "lima", BackendLima},
		// Whitespace / case tolerance.
		{"case-insensitive podman", "PODMAN", BackendPodman},
		{"case-insensitive lima", "LiMa", BackendLima},
		{"trims whitespace", "  local  ", BackendLocal},
		// Unknown explicit value fails safe to local (never silently
		// drops into a container/VM).
		{"unknown type fails safe", "qubes", BackendLocal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{}
			c.Backend.Type = tt.backendType
			if got := c.ResolveBackend(); got != tt.want {
				t.Fatalf("ResolveBackend() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPodmanRuntime(t *testing.T) {
	cases := map[string]string{"": "auto", "  ": "auto", "AUTO": "auto", "podman": "podman", " Docker ": "docker"}
	for in, want := range cases {
		c := &Config{}
		c.Backend.Runtime = in
		if got := c.PodmanRuntime(); got != want {
			t.Errorf("PodmanRuntime(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnrecognizedBackendType(t *testing.T) {
	// Empty / known values are intentional selections → no flag.
	for _, ok := range []string{"", "local", "LOCAL", "  podman  ", "lima"} {
		c := &Config{}
		c.Backend.Type = ok
		if got := c.UnrecognizedBackendType(); got != "" {
			t.Errorf("type %q must not flag, got %q", ok, got)
		}
	}
	// An unrecognized non-empty value is a (safety-relevant)
	// misconfiguration → return it verbatim for the user-facing flag.
	c := &Config{}
	c.Backend.Type = "qubes"
	if got := c.UnrecognizedBackendType(); got != "qubes" {
		t.Errorf("unrecognized type must be flagged verbatim, got %q", got)
	}
	// Resolution still fails safe to local regardless.
	if c.ResolveBackend() != BackendLocal {
		t.Error("unrecognized type must still resolve to local (fail safe)")
	}
}
