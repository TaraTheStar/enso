// SPDX-License-Identifier: AGPL-3.0-or-later

package podman

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/backend"
)

// joinArgs renders argv as a single space-joined string for substring
// assertions (the values here never contain spaces).
func joinArgs(a []string) string { return strings.Join(a, " ") }

func TestBuildRunArgs_Posture(t *testing.T) {
	cwd := "/home/u/proj"
	b := &Backend{Image: "alpine", Network: ""}
	got := joinArgs(b.buildRunArgs("enso-proj-t1", "t1", "/host/enso", cwd, ""))

	// One namespace: project at its REAL path, cwd = that path; no /work.
	if !strings.Contains(got, "-v "+cwd+":"+cwd+" ") {
		t.Errorf("project must mount at its real path 1:1, got: %s", got)
	}
	if !strings.Contains(got, "-w "+cwd) {
		t.Errorf("cwd must be the real path, got: %s", got)
	}
	if strings.Contains(got, "/work") {
		t.Errorf("the /work split-brain mount must be gone, got: %s", got)
	}
	// Sealed by default.
	if !strings.Contains(got, "--network none") {
		t.Errorf("default must be network-sealed, got: %s", got)
	}
	// enso bind-mounted read-only; PID-1 worker entrypoint.
	if !strings.Contains(got, "-v /host/enso:/usr/local/bin/enso:ro") ||
		!strings.HasSuffix(got, "alpine /usr/local/bin/enso __worker") {
		t.Errorf("worker entrypoint/bind-mount wrong, got: %s", got)
	}
	// Credential scrub: NO host environment is forwarded. Pick an env
	// var that is essentially always set and assert it never appears.
	_ = os.Setenv("ENSO_SCRUB_PROBE", "leaked-secret")
	got2 := joinArgs(b.buildRunArgs("n", "t1", "/host/enso", cwd, ""))
	if strings.Contains(got2, "leaked-secret") || strings.Contains(got2, "ENSO_SCRUB_PROBE") {
		t.Errorf("host env must never be forwarded to the box, got: %s", got2)
	}
}

func TestBuildRunArgs_OverlayAndEgress(t *testing.T) {
	cwd := "/p"
	b := &Backend{Image: "alpine", MountSource: "/tmp/enso-ws/merged", EgressProxy: "http://127.0.0.1:54321"}
	got := joinArgs(b.buildRunArgs("n", "t1", "/e", cwd, ""))

	// Overlay = host-side throwaway copy bind-mounted at the REAL path
	// (NOT podman's :O, which would silently discard the agent's work).
	if !strings.Contains(got, "-v /tmp/enso-ws/merged:"+cwd+" ") {
		t.Errorf("overlay must bind the host copy at the real project path, got: %s", got)
	}
	if strings.Contains(got, ":O") {
		t.Errorf("must NOT use podman :O (auto-discard); overlay is host-controlled, got: %s", got)
	}
	// Egress: the box leaves "none" for slirp WITH host-loopback so the
	// host proxy is reachable, and the in-container proxy env points at
	// the slirp gateway (not 127.0.0.1, which is the container itself).
	if strings.Contains(got, "--network none") {
		t.Errorf("with an egress proxy the box must not be fully sealed, got: %s", got)
	}
	if !strings.Contains(got, "--network slirp4netns:allow_host_loopback=true") {
		t.Errorf("egress needs slirp host-loopback so the host proxy is reachable, got: %s", got)
	}
	if !strings.Contains(got, "-e HTTPS_PROXY=http://10.0.2.2:54321") ||
		!strings.Contains(got, "-e https_proxy=http://10.0.2.2:54321") {
		t.Errorf("in-container proxy env must target the slirp gateway, got: %s", got)
	}
	if strings.Contains(got, "HTTPS_PROXY=http://127.0.0.1:54321") {
		t.Errorf("must NOT hand the container the host-loopback URL, got: %s", got)
	}
	if !strings.Contains(got, "-e NO_PROXY=127.0.0.1,localhost,10.0.2.2") {
		t.Errorf("loopback/gateway must bypass the proxy, got: %s", got)
	}
}

func TestContainerProxyURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:8080":      "http://10.0.2.2:8080",
		"http://localhost:9999":      "http://10.0.2.2:9999",
		"http://10.1.2.3:8080":       "http://10.1.2.3:8080", // real host: unchanged
		"http://proxy.internal:3128": "http://proxy.internal:3128",
	} {
		if got := containerProxyURL(in); got != want {
			t.Errorf("containerProxyURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildRunArgs_GVisorRuntime(t *testing.T) {
	// No hardening → no --runtime flag (default runc/crun).
	plain := joinArgs((&Backend{Image: "alpine"}).buildRunArgs("n", "t", "/e", "/p", ""))
	if strings.Contains(plain, "--runtime") {
		t.Errorf("unhardened run must not pin a runtime, got: %s", plain)
	}
	// Hardened → `--runtime runsc` (gVisor) right after `run`.
	got := joinArgs((&Backend{Image: "alpine", OCIRuntime: "runsc"}).buildRunArgs("n", "t", "/e", "/p", "runsc"))
	if !strings.Contains(got, "run --rm -i --runtime runsc ") {
		t.Errorf("gVisor run must pass --runtime runsc, got: %s", got)
	}
}

func TestStart_RefusesWhenHardenedRuntimeMissing(t *testing.T) {
	b := &Backend{Image: "alpine", OCIRuntime: "enso-nonexistent-runtime-xyz"}
	_, err := b.Start(context.Background(), backend.TaskSpec{Cwd: t.TempDir()})
	if err == nil {
		t.Fatal("Start must refuse when the hardened runtime is unavailable")
	}
	for _, want := range []string{"not found", "Refusing to run unhardened", "gvisor.dev"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must be actionable (missing %q): %v", want, err)
		}
	}
}
