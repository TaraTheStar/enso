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
	// 2nd arg is the STABLE exestage mount ROOT (exe/ parent), mounted
	// verbatim — invariant across host rebuilds (no drift-recreate).
	y := b.buildVMConfig(cwd, "/host/state/exe")

	// One namespace: project mounted WRITABLE at its REAL path; cwd is
	// that path. No /work split-brain mount.
	if !strings.Contains(y, `location: "`+cwd+`"`) ||
		!strings.Contains(y, `mountPoint: "`+cwd+`"`) ||
		!strings.Contains(y, "writable: true") {
		t.Errorf("project must mount writable at its real path 1:1, got:\n%s", y)
	}
	// Host $HOME must NOT be mounted: the image-only base inherits no
	// `~` mount and we emit none, so the agent can't read ~/.ssh,
	// ~/.config/enso, sibling repos. This is the isolation guarantee.
	if strings.Contains(y, `location: "~"`) {
		t.Errorf("host $HOME must NOT be mounted (confidentiality), got:\n%s", y)
	}
	if strings.Contains(y, "/work") {
		t.Errorf("the /work split-brain mount must not exist, got:\n%s", y)
	}
	// enso snapshot ROOT mounted READ-ONLY, verbatim (no filepath.Dir
	// derivation — the root IS the mount, stable across rebuilds).
	if !strings.Contains(y, `location: "/host/state/exe"`) || !strings.Contains(y, "writable: false") {
		t.Errorf("enso snapshot root must be mounted read-only, got:\n%s", y)
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
	// Default base is the IMAGE-ONLY Alpine sub-template (no mounts in
	// its base chain → no inherited $HOME mount). NOT template:default.
	if !strings.Contains(y, `base: "template:_images/alpine"`) {
		t.Errorf("default base must be the image-only template:_images/alpine, got:\n%s", y)
	}
	if strings.Contains(y, "template:default") || strings.Contains(y, "template://") {
		t.Errorf("must not inherit a full template (pulls _default/mounts → $HOME), got:\n%s", y)
	}
	if !strings.Contains(y, "cpus: 4") || !strings.Contains(y, `memory: "4GiB"`) || !strings.Contains(y, `disk: "20GiB"`) {
		t.Errorf("cpus/memory/disk must be pinned, got:\n%s", y)
	}
}

func TestBuildVMConfig_OverlayAndTemplate(t *testing.T) {
	cwd := "/p"
	b := &Backend{Template: "debian", MountSource: "/var/lib/enso/stage/abcd/merged"}
	y := b.buildVMConfig(cwd, "/e/exe")

	// Overlay = the stable host copy bound at the REAL project path
	// (the real project is never the mount source here).
	if !strings.Contains(y, `location: "/var/lib/enso/stage/abcd/merged"`) ||
		!strings.Contains(y, `mountPoint: "`+cwd+`"`) {
		t.Errorf("overlay copy must bind at the real project path, got:\n%s", y)
	}
	// Bare distro name → IMAGE-ONLY sub-template, never the full
	// template:debian (which would re-mount $HOME via _default/mounts).
	if !strings.Contains(y, `base: "template:_images/debian"`) {
		t.Errorf("bare distro name must resolve to template:_images/<name>, got:\n%s", y)
	}
	if strings.Contains(y, `location: "~"`) {
		t.Errorf("custom bare distro must still not mount $HOME, got:\n%s", y)
	}

	// A path/URL template is used verbatim as base (advanced; user owns
	// the posture).
	y2 := (&Backend{Template: "/opt/my.yaml"}).buildVMConfig(cwd, "/e/exe")
	if !strings.Contains(y2, `base: "/opt/my.yaml"`) {
		t.Errorf("path template must be used verbatim, got:\n%s", y2)
	}
}

func TestBuildVMConfig_IptablesBootstrap(t *testing.T) {
	cwd := "/p"

	// Sealed + Alpine (default): three ordered system provision steps —
	// the cosmetic boot-speedup, THEN the apk iptables bootstrap (the
	// Alpine cloud image ships no iptables and the seal needs it), THEN
	// user init. Each is its own step so a broken later one can't
	// strand the seal.
	y := (&Backend{Sealed: true, Init: []string{"echo user-step"}}).buildVMConfig(cwd, "/e/exe")
	speed := strings.Index(y, "set timeout=0")
	boot := strings.Index(y, "apk add --no-cache iptables ip6tables")
	user := strings.Index(y, "echo user-step")
	if boot < 0 || !strings.Contains(y, "ip6tables") {
		t.Errorf("sealed Alpine bootstrap must install ip6tables for the fail-closed v6 seal, got:\n%s", y)
	}
	if speed < 0 || boot < 0 {
		t.Errorf("sealed Alpine must emit boot-speedup AND iptables bootstrap, got:\n%s", y)
	}
	if !(speed < boot && boot < user) {
		t.Errorf("order must be speedup < iptables < user init, got:\n%s", y)
	}
	if strings.Count(y, "  - mode: system") != 3 {
		t.Errorf("expected 3 system provision steps (speedup + bootstrap + init), got:\n%s", y)
	}

	// Sealed but NOT Alpine: Ubuntu/Debian images already carry
	// iptables, and the boot-speedup is Alpine-only — no provision at
	// all without user init.
	yu := (&Backend{Sealed: true, Template: "ubuntu"}).buildVMConfig(cwd, "/e/exe")
	if strings.Contains(yu, "apk add") || strings.Contains(yu, "set timeout=0") || strings.Contains(yu, "provision:") {
		t.Errorf("non-Alpine sealed must inject no provision, got:\n%s", yu)
	}

	// Unsealed Alpine: no seal → no iptables bootstrap, but the
	// Alpine-only boot-speedup is still applied (it is not gated on
	// Sealed — boot time is not a security property).
	yn := (&Backend{Sealed: false}).buildVMConfig(cwd, "/e/exe")
	if strings.Contains(yn, "apk add") {
		t.Errorf("unsealed must not bootstrap iptables, got:\n%s", yn)
	}
	if !strings.Contains(yn, "set timeout=0") {
		t.Errorf("Alpine boot-speedup must apply even unsealed, got:\n%s", yn)
	}
}

func TestBuildVMConfig_ProvisionInit(t *testing.T) {
	// No Init, non-Alpine → no provision block at all (Lima default
	// behaviour unchanged for the common non-Alpine case).
	bare := (&Backend{Template: "ubuntu"}).buildVMConfig("/p", "/e/exe")
	if strings.Contains(bare, "provision:") {
		t.Errorf("empty Init on non-Alpine must emit no provision block, got:\n%s", bare)
	}

	// No Init, Alpine → exactly the cosmetic boot-speedup step, nothing
	// user-supplied.
	alp := (&Backend{}).buildVMConfig("/p", "/e/exe")
	if !strings.Contains(alp, "set timeout=0") || strings.Count(alp, "  - mode: system") != 1 {
		t.Errorf("Alpine no-init must emit exactly the boot-speedup step, got:\n%s", alp)
	}

	b := &Backend{Template: "ubuntu", Init: []string{
		"apt-get update",
		"apt-get install -y git build-essential",
	}}
	y := b.buildVMConfig("/p", "/e/exe")

	// One system-mode provision script, set -e so a failed install
	// aborts loudly, every Init line indented under the block scalar.
	for _, want := range []string{
		"provision:",
		"  - mode: system",
		"    script: |",
		"      set -e",
		"      apt-get update",
		"      apt-get install -y git build-essential",
	} {
		if !strings.Contains(y, want) {
			t.Errorf("provision YAML missing %q, got:\n%s", want, y)
		}
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
	allow := "iptables -w -A ENSO_EGRESS -p tcp -d 192.168.5.2 --dport 54321 -j ACCEPT"
	if !strings.Contains(p, allow) {
		t.Errorf("proxied seal must open the gateway:port, got:\n%s", p)
	}
	if strings.Index(p, allow) > strings.Index(p, "iptables -w -A ENSO_EGRESS -j REJECT") {
		t.Errorf("proxy ACCEPT must precede REJECT:\n%s", p)
	}
}

// TestSealScript_IPv6DefaultDeny is the S4 regression: the seal must
// default-deny IPv6 OUTPUT too, or a guest with working IPv6 egresses
// around the v4-only allowlist. The v6 chain mirrors v4 (lo + established
// + REJECT) but carries NO proxy ACCEPT — the proxy is at the IPv4
// gateway, so v6 is a pure seal.
func TestSealScript_IPv6DefaultDeny(t *testing.T) {
	for _, in := range []string{"", "192.168.5.2:54321"} {
		s := sealScript(in)
		for _, want := range []string{
			"ip6tables -w -F ENSO_EGRESS 2>/dev/null || ip6tables -w -N ENSO_EGRESS",
			"ip6tables -w -A ENSO_EGRESS -o lo -j ACCEPT",
			"ip6tables -w -A ENSO_EGRESS -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
			"ip6tables -w -A ENSO_EGRESS -j REJECT --reject-with icmp6-port-unreachable",
			"ip6tables -w -C OUTPUT -j ENSO_EGRESS",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("v6 seal (proxy=%q) missing %q:\n%s", in, want, s)
			}
		}
		// The v6 chain never opens a port — the proxy is IPv4-only.
		v6Start := strings.Index(s, "ip6tables")
		if strings.Contains(s[v6Start:], "--dport") {
			t.Errorf("v6 chain must not open any egress port (proxy is IPv4), got:\n%s", s)
		}
		// v6 REJECT must follow its ACCEPT allowances (order matters).
		rej := strings.Index(s, "ip6tables -w -A ENSO_EGRESS -j REJECT")
		lo := strings.Index(s, "ip6tables -w -A ENSO_EGRESS -o lo -j ACCEPT")
		if rej < lo {
			t.Errorf("v6 REJECT must follow the ACCEPT rules:\n%s", s)
		}
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
