// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import "testing"

func TestBashSandboxOptions_OCIRuntime(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"  ":      "",
		"gvisor":  "runsc",
		"GVisor":  "runsc",
		"runsc":   "runsc",
		" runsc ": "runsc",
		// Unknown values pass through verbatim so Start fails the
		// availability check loudly instead of silently unhardened.
		"kata":  "kata",
		"typo!": "typo!",
	}
	for in, want := range cases {
		if got := (BashSandboxOptions{Hardening: in}).OCIRuntime(); got != want {
			t.Errorf("OCIRuntime(%q) = %q, want %q", in, got, want)
		}
	}
}
