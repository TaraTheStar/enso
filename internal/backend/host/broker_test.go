// SPDX-License-Identifier: AGPL-3.0-or-later

package host

import (
	"context"
	"testing"

	"github.com/TaraTheStar/enso/internal/backend/egress"
	"github.com/TaraTheStar/enso/internal/backend/wire"
)

func TestAllowlistBroker(t *testing.T) {
	pr := egress.New()
	if err := pr.Start(); err != nil {
		t.Fatalf("proxy start: %v", err)
	}
	defer pr.Close()

	b := &AllowlistBroker{
		Creds:  map[string]string{"GH_TOKEN": "ghp_x"},
		Egress: map[string]bool{"api.github.com:443": true},
		Proxy:  pr,
		TTL:    30,
	}
	ctx := context.Background()

	// Listed credential → granted with the secret + TTL.
	if g := b.Authorize(ctx, wire.CapabilityRequest{Type: wire.CapCredential, Name: "GH_TOKEN"}); !g.Granted || g.Secret != "ghp_x" || g.TTLSeconds != 30 {
		t.Errorf("listed credential: %+v", g)
	}
	// Unlisted credential → denied, no secret, auditable reason.
	if g := b.Authorize(ctx, wire.CapabilityRequest{Type: wire.CapCredential, Name: "AWS"}); g.Granted || g.Secret != "" || g.Reason == "" {
		t.Errorf("unlisted credential must be denied: %+v", g)
	}
	// Listed egress → granted AND opened live on the proxy.
	if g := b.Authorize(ctx, wire.CapabilityRequest{Type: wire.CapEgress, Name: "api.github.com:443"}); !g.Granted {
		t.Errorf("listed egress should be granted: %+v", g)
	}
	if !pr.Allowed("api.github.com:443") {
		t.Error("granted egress was not opened on the proxy allowlist")
	}
	// Unlisted egress → denied and NOT opened.
	if g := b.Authorize(ctx, wire.CapabilityRequest{Type: wire.CapEgress, Name: "evil.example:443"}); g.Granted {
		t.Errorf("unlisted egress must be denied: %+v", g)
	}
	if pr.Allowed("evil.example:443") {
		t.Error("denied egress must not be on the proxy allowlist")
	}
}
