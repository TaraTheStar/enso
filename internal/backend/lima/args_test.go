// SPDX-License-Identifier: AGPL-3.0-or-later

package lima

import (
	"context"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/backend"
)

func joinArgs(a []string) string { return strings.Join(a, " ") }

func TestBuildVMConfig_Posture(t *testing.T) {
	cwd := "/home/u/proj"
	b := &Backend{Memory: "4GiB", CPUs: 4, Disk: "20GiB"}
	y := b.buildVMConfig(cwd, "/host/bin/enso")

	// One namespace: project mounted WRITABLE at its REAL path; cwd is
	// that path. No /work split-brain mount.
	if !strings.Contains(y, `location: "`+cwd+`"`) ||
		!strings.Contains(y, `mountPoint: "`+cwd+`"`) ||
		!strings.Contains(y, "writable: true") {
		t.Errorf("project must mount writable at its real path 1:1, got:\n%s", y)
	}
	if strings.Contains(y, "/work") {
		t.Errorf("the /work split-brain mount must not exist, got:\n%s", y)
	}
	// enso binary dir mounted READ-ONLY (no image rebuild).
	if !strings.Contains(y, `location: "/host/bin"`) || !strings.Contains(y, "writable: false") {
		t.Errorf("enso binary dir must be mounted read-only, got:\n%s", y)
	}
	// Minimal VM: no containerd. (No ssh.loadDotSSH — that key trips
	// Lima's schema check on some versions and is irrelevant to a
	// sealed, host-proxied box.)
	if !strings.Contains(y, "containerd:\n  system: false\n  user: false") {
		t.Errorf("containerd must be disabled, got:\n%s", y)
	}
	if strings.Contains(y, "loadDotSSH") {
		t.Errorf("must not emit ssh.loadDotSSH (Lima schema warning), got:\n%s", y)
	}
	// Tunables applied; base template inherited.
	if !strings.Contains(y, `base: "template://default"`) {
		t.Errorf("default base template must be template://default, got:\n%s", y)
	}
	if !strings.Contains(y, "cpus: 4") || !strings.Contains(y, `memory: "4GiB"`) || !strings.Contains(y, `disk: "20GiB"`) {
		t.Errorf("cpus/memory/disk must be pinned, got:\n%s", y)
	}
}

func TestBuildVMConfig_OverlayAndTemplate(t *testing.T) {
	cwd := "/p"
	b := &Backend{Template: "debian", MountSource: "/var/lib/enso/stage/abcd/merged"}
	y := b.buildVMConfig(cwd, "/e/enso")

	// Overlay = the stable host copy bound at the REAL project path
	// (the real project is never the mount source here).
	if !strings.Contains(y, `location: "/var/lib/enso/stage/abcd/merged"`) ||
		!strings.Contains(y, `mountPoint: "`+cwd+`"`) {
		t.Errorf("overlay copy must bind at the real project path, got:\n%s", y)
	}
	// Bare template name → template:// scheme.
	if !strings.Contains(y, `base: "template://debian"`) {
		t.Errorf("bare template name must become template://, got:\n%s", y)
	}

	// A path/URL template is used verbatim as base.
	y2 := (&Backend{Template: "/opt/my.yaml"}).buildVMConfig(cwd, "/e/enso")
	if !strings.Contains(y2, `base: "/opt/my.yaml"`) {
		t.Errorf("path template must be used verbatim, got:\n%s", y2)
	}
}

func TestBuildShellArgs_NoPTYRealCwd(t *testing.T) {
	got := joinArgs(buildShellArgs("enso-proj-deadbeef", "/home/u/proj", "/host/bin/enso", ""))
	// Worker pinned to the REAL cwd, exec the bind-mounted enso.
	if !strings.Contains(got, "shell --workdir /home/u/proj enso-proj-deadbeef /host/bin/enso __worker") {
		t.Errorf("shell must run the worker at the real cwd, got: %s", got)
	}
	// Must be PTY-free so the binary frame stays clean: no -t/--tty.
	if strings.Contains(got, "--tty") || strings.Contains(got, " -t ") {
		t.Errorf("worker shell must not allocate a tty, got: %s", got)
	}
}

func TestBuildShellArgs_ProxyInjection(t *testing.T) {
	got := joinArgs(buildShellArgs("vm", "/p", "/e/enso", "http://192.168.5.2:54321"))
	// Worker wrapped in `env` so its only route out is the host proxy
	// (reached at the Lima gateway), and loopback is never proxied.
	for _, want := range []string{
		"shell --workdir /p vm env ",
		"HTTPS_PROXY=http://192.168.5.2:54321",
		"HTTP_PROXY=http://192.168.5.2:54321",
		"NO_PROXY=127.0.0.1,localhost",
		"/e/enso __worker",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in: %s", want, got)
		}
	}
	// No proxy → no env wrapper (fully sealed box).
	if bare := joinArgs(buildShellArgs("vm", "/p", "/e/enso", "")); strings.Contains(bare, "env ") || strings.Contains(bare, "PROXY") {
		t.Errorf("no-proxy launch must not inject env: %s", bare)
	}
}

func TestGuestProxyURL_GatewayTranslation(t *testing.T) {
	// Host loopback → the Lima slirp gateway (host bind stays loopback;
	// only the in-guest value is translated).
	if got := guestProxyURL("http://127.0.0.1:9000"); got != "http://192.168.5.2:9000" {
		t.Errorf("loopback must map to the Lima gateway, got %q", got)
	}
	// Non-loopback (operator pointed at a real address) passes through.
	if got := guestProxyURL("http://10.1.2.3:8080"); got != "http://10.1.2.3:8080" {
		t.Errorf("non-loopback must pass through, got %q", got)
	}
	// Empty in → empty out (no proxy → no env).
	if got := guestProxyURL(""); got != "" {
		t.Errorf("empty must stay empty, got %q", got)
	}
}

func TestSealScript_DefaultDenyPosture(t *testing.T) {
	// Sealed, no proxy: loopback + established only, then REJECT, and
	// NO proxy ACCEPT line (a fully sealed box, like default podman
	// --network none).
	s := sealScript("")
	for _, want := range []string{
		"ENSO_EGRESS -o lo -j ACCEPT",
		"--ctstate ESTABLISHED,RELATED -j ACCEPT",
		"ENSO_EGRESS -j REJECT",
		"-C OUTPUT -j ENSO_EGRESS", // idempotent jump install
	} {
		if !strings.Contains(s, want) {
			t.Errorf("sealed script missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "--dport") {
		t.Errorf("no-proxy seal must not open any egress port:\n%s", s)
	}
	// The REJECT must come AFTER the allow rules (order matters).
	if strings.Index(s, "REJECT") < strings.Index(s, "lo -j ACCEPT") {
		t.Errorf("REJECT must follow the ACCEPT allowances:\n%s", s)
	}

	// With a proxy: exactly that host:port is opened, before the REJECT.
	p := sealScript("192.168.5.2:54321")
	allow := "ENSO_EGRESS -p tcp -d 192.168.5.2 --dport 54321 -j ACCEPT"
	if !strings.Contains(p, allow) {
		t.Errorf("proxied seal must open the gateway:port, got:\n%s", p)
	}
	if strings.Index(p, allow) > strings.Index(p, "-j REJECT") {
		t.Errorf("proxy ACCEPT must precede REJECT:\n%s", p)
	}
}

func TestBuildStartArgs_NonInteractive(t *testing.T) {
	got := joinArgs(buildStartArgs("enso-proj-deadbeef", "/run/enso/lima/x.yaml"))
	if got != "start --name enso-proj-deadbeef --tty=false /run/enso/lima/x.yaml" {
		t.Errorf("start must be non-interactive (--tty=false) from generated YAML, got: %s", got)
	}
}

func TestVMName_PerProjectStable(t *testing.T) {
	// Stable across calls (persistent per-project, NOT per-task).
	a1 := vmName("/home/u/proj")
	a2 := vmName("/home/u/proj")
	if a1 != a2 {
		t.Errorf("vmName must be stable per project: %q vs %q", a1, a2)
	}
	// Same basename, different path → different VM (no silent sharing).
	if vmName("/home/u/proj") == vmName("/tmp/proj") {
		t.Error("same-basename projects in different paths must not collide")
	}
	if !strings.HasPrefix(a1, "enso-proj-") {
		t.Errorf("vmName must be enso-<base>-<hash>, got %q", a1)
	}
}

func TestStart_RefusesWhenLimactlMissing(t *testing.T) {
	old := limactlBin
	limactlBin = "enso-nonexistent-limactl-xyz"
	defer func() { limactlBin = old }()

	_, err := (&Backend{}).Start(context.Background(), backend.TaskSpec{Cwd: t.TempDir()})
	if err == nil {
		t.Fatal("Start must refuse when limactl is unavailable")
	}
	for _, want := range []string{"not found", "Refusing to run", "lima-vm.io"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must be actionable (missing %q): %v", want, err)
		}
	}
}
