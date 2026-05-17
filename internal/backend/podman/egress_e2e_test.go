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
