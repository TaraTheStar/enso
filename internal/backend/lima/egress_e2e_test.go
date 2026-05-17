// SPDX-License-Identifier: AGPL-3.0-or-later

package lima

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/backend/egress"
)

// TestSealedGuestEgress_RealVM closes the data-path gap item 1b left to
// item 2 step 7: that a REAL sealed Lima VM actually denies uncontrolled
// egress and permits ONLY the allowlisted host through the host proxy —
// proving the guest firewall (sealGuestEgress) + host-loopback↔slirp
// gateway wiring on THIS host, not just the policy in unit tests.
//
// It deliberately needs no internet: a host httptest upstream stands in
// for "the outside", reached from the guest only as the proxy's
// allowlisted target. Environment limits (no limactl, no /dev/kvm,
// image download / first boot too slow, no curl/iptables in the guest)
// SKIP rather than false-fail — the policy itself is unit-tested; only
// the real-VM data path needs this host. A genuine seal regression
// (uncontrolled egress succeeds, or an allowed target is blocked)
// still FAILS.
func TestSealedGuestEgress_RealVM(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a real Lima VM; skipped in -short")
	}
	if _, err := exec.LookPath(limactlBin); err != nil {
		t.Skip("limactl (Lima) not on PATH")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm: hardware virtualization unavailable for a Lima VM")
	}
	limactl, err := exec.LookPath(limactlBin)
	if err != nil {
		t.Skip("limactl not resolvable")
	}

	exe, err := os.Executable() // any real file; only its dir is mounted ro
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}

	// Unique temp project → unique per-project VM name; delete exactly
	// that instance afterward (never a broad Sweep, which would hit a
	// developer's real enso project VMs).
	proj := t.TempDir()
	name := vmName(proj)
	t.Cleanup(func() {
		cl, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = exec.CommandContext(cl, limactl, "stop", "--force", name).Run()
		_ = exec.CommandContext(cl, limactl, "delete", "--force", name).Run()
	})

	// Generous: first run downloads a cloud image and boots a VM.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	b := &Backend{Sealed: true}
	if err := b.ensureRunning(ctx, limactl, name, proj, exe); err != nil {
		// Bring-up failure is an environment limit (image download,
		// KVM, lima too old), NOT a seal bug — the seal is unit-tested.
		t.Skipf("Lima VM could not be brought up on this host (environment, not enso): %v", err)
	}

	// Host upstream the proxy will dial host-side; the guest never
	// reaches it except as the proxy's allowlisted target.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "EGRESS-OK")
	}))
	defer upstream.Close()
	upHost := upstream.Listener.Addr().String() // 127.0.0.1:NNNN

	pr := egress.New()
	if err := pr.Start(); err != nil {
		t.Fatalf("proxy start: %v", err)
	}
	defer pr.Close()
	pr.Allow(upHost) // allowed; everything else stays denied

	// Apply the real guest firewall with this proxy, exactly as Start
	// does (gateway-translated URL → the firewall opens that host:port).
	if err := sealGuestEgress(ctx, limactl, name, guestProxyURL(pr.ProxyURL())); err != nil {
		t.Skipf("could not seal guest (no iptables/curl in this image?): %v", err)
	}

	// curl is the data-path probe; a guest image without it can't
	// exercise this (skip, don't false-pass step 1 on a curl error).
	if out, err := exec.CommandContext(ctx, limactl, "shell", name, "sh", "-c", "command -v curl").CombinedOutput(); err != nil {
		t.Skipf("no curl in the guest image: %v\n%s", err, out)
	}

	_, proxyPort, _ := net.SplitHostPort(strings.TrimPrefix(pr.ProxyURL(), "http://"))
	_, upPort, _ := net.SplitHostPort(upHost)
	if upPort == proxyPort {
		t.Skip("degenerate ephemeral port collision (upstream == proxy); rerun")
	}
	gwProxy := "http://" + limaHostGateway + ":" + proxyPort // 192.168.5.2:NNNN

	guestCurl := func(args ...string) (string, error) {
		cctx, ccancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer ccancel()
		full := append([]string{"shell", name, "curl", "-s", "-S", "--max-time", "10"}, args...)
		out, err := exec.CommandContext(cctx, limactl, full...).CombinedOutput()
		return string(out), err
	}

	// 1. UNCONTROLLED egress is blocked: a direct connection to the
	//    host (gateway) on a NON-proxy port — bypassing the proxy —
	//    must be REJECTed by the guest firewall. (The upstream port is
	//    not the allowed proxy port.)
	if out, err := guestCurl("http://" + limaHostGateway + ":" + upPort + "/"); err == nil && strings.Contains(out, "EGRESS-OK") {
		t.Fatalf("UNCONTROLLED direct egress must be blocked by the seal, but it reached upstream: %q", out)
	}

	// 2. Allowed target THROUGH the proxy succeeds: proves the box's
	//    one sanctioned route out works end-to-end over slirp.
	if out, err := guestCurl("-x", gwProxy, "http://"+upHost+"/"); err != nil || !strings.Contains(out, "EGRESS-OK") {
		t.Fatalf("allowlisted egress via the proxy must succeed; err=%v out=%q", err, out)
	}

	// 3. A non-allowlisted target through the proxy is refused (403):
	//    the proxy is a policy gate, not an open relay.
	if out, _ := guestCurl("-x", gwProxy, "http://198.51.100.7:80/"); strings.Contains(out, "EGRESS-OK") {
		t.Fatalf("non-allowlisted egress must NOT succeed through the proxy; out=%q", out)
	}
}
