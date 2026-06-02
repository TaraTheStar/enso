// SPDX-License-Identifier: AGPL-3.0-or-later

package podman_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/backend/egress"
	"github.com/TaraTheStar/enso/internal/backend/seal"
)

// TestEgressDataPath_RealContainer closes the egress data-path gap: that
// a sealed rootless container can actually reach the host allowlist
// proxy (and ONLY allowed targets through it). It runs a real
// `slirp4netns:allow_host_loopback=true` container that wgets through
// the proxy — proving the host-loopback↔slirp-gateway wiring on THIS
// host, not just the policy in unit tests.
func TestEgressDataPath_RealContainer(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real container; skipped in -short")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not on PATH")
	}
	pull, cancelPull := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelPull()
	if out, err := exec.CommandContext(pull, "podman", "pull", "-q", testImage).CombinedOutput(); err != nil {
		t.Skipf("cannot pull %s: %v\n%s", testImage, err, out)
	}

	// Host upstream the proxy will dial (host-side, so the container
	// never needs to reach it directly — only the proxy does).
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
	pr.Allow(upHost) // allowed; a different target stays denied

	// Translate the host-loopback proxy addr to what the container
	// reaches it on, exactly as podman.Backend does.
	_, proxyPort, _ := net.SplitHostPort(strings.TrimPrefix(pr.ProxyURL(), "http://"))
	inContainerProxy := "http://10.0.2.2:" + proxyPort

	run := func(target string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "podman", "run", "--rm",
			"--network", "slirp4netns:allow_host_loopback=true",
			"-e", "http_proxy="+inContainerProxy,
			testImage,
			"wget", "-q", "-T", "10", "-O", "-", "http://"+target+"/",
		).CombinedOutput()
		return string(out), err
	}

	// Allowed target: reaches upstream THROUGH the proxy over slirp.
	if out, err := run(upHost); err != nil || !strings.Contains(out, "EGRESS-OK") {
		t.Fatalf("allowed egress should succeed through the sealed container; err=%v out=%q", err, out)
	}

	// Denied target: the proxy refuses (not on the allowlist), so the
	// fetch fails — nothing leaves the box uncontrolled.
	if out, err := run("198.51.100.7:80"); err == nil && strings.Contains(out, "EGRESS-OK") {
		t.Fatalf("non-allowlisted egress must NOT succeed; out=%q", out)
	}
}

// TestEgressSeal_BlocksProxyBypass_RealContainer is the S3 regression: the
// HTTPS_PROXY env is only advisory, so a model can unset it and dial
// slirp's host-loopback gateway directly (reaching the host proxy's
// loopback siblings, or slirp's NAT for the open internet). With the
// in-guest packet seal (NET_ADMIN + the entrypoint's ENSO_EGRESS chain)
// the proxy gateway:port is the ONLY reachable target — a direct,
// proxy-bypassing fetch must FAIL. This runs the exact seal program the
// backend injects (seal.Rules), so it tracks the production entrypoint.
func TestEgressSeal_BlocksProxyBypass_RealContainer(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real container; skipped in -short")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not on PATH")
	}
	pull, cancelPull := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelPull()
	if out, err := exec.CommandContext(pull, "podman", "pull", "-q", testImage).CombinedOutput(); err != nil {
		t.Skipf("cannot pull %s: %v\n%s", testImage, err, out)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "EGRESS-OK")
	}))
	defer upstream.Close()
	upHost := upstream.Listener.Addr().String() // 127.0.0.1:NNNN (host view)
	_, upPort, _ := net.SplitHostPort(upHost)

	pr := egress.New()
	if err := pr.Start(); err != nil {
		t.Fatalf("proxy start: %v", err)
	}
	defer pr.Close()
	pr.Allow(upHost) // proxy may reach the upstream (explicit → exempt from the SSRF denylist)

	_, proxyPort, _ := net.SplitHostPort(strings.TrimPrefix(pr.ProxyURL(), "http://"))
	gateway := "10.0.2.2"

	// Build the SAME sealed entrypoint the backend injects (NET_ADMIN +
	// seal.Rules), then run a busybox-wget probe after the seal instead of
	// the worker. env is passed as podman -e pairs.
	sealedProbe := func(env []string, wgetArgs string) (string, error) {
		script := "set -e\n{\n" +
			"command -v iptables >/dev/null 2>&1 && command -v ip6tables >/dev/null 2>&1 || apk add --no-cache iptables ip6tables\n" +
			seal.Rules(gateway+":"+proxyPort) +
			"} 1>&2\nwget " + wgetArgs + "\n"
		args := []string{"run", "--rm",
			"--network", "slirp4netns:allow_host_loopback=true",
			"--cap-add", "NET_ADMIN"}
		for _, e := range env {
			args = append(args, "-e", e)
		}
		args = append(args, testImage, "sh", "-c", script)
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "podman", args...).CombinedOutput()
		return string(out), err
	}

	// Through the proxy (the sanctioned route): busybox wget honours the
	// http_proxy env, asks the proxy (at the gateway) for the host-view URL.
	if out, err := sealedProbe(
		[]string{"http_proxy=http://" + gateway + ":" + proxyPort},
		"-q -T 10 -O - http://"+upHost+"/",
	); err != nil || !strings.Contains(out, "EGRESS-OK") {
		t.Fatalf("sealed container must still reach the upstream THROUGH the proxy; err=%v out=%q", err, out)
	}

	// Proxy-bypass: no proxy env, `-Y off`, dial the host loopback at the
	// slirp gateway directly. Without the seal this reaches host loopback
	// (the S3 bug); the seal must drop it — no EGRESS-OK.
	if out, err := sealedProbe(nil, "-Y off -q -T 8 -O - http://"+gateway+":"+upPort+"/"); err == nil && strings.Contains(out, "EGRESS-OK") {
		t.Fatalf("in-guest seal must block the direct host-loopback bypass; out=%q", out)
	}
}
