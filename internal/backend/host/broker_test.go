// SPDX-License-Identifier: AGPL-3.0-or-later

package host

import (
	"context"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/backend/egress"
	"github.com/TaraTheStar/enso/internal/backend/wire"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/permissions"
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

// TestAllowlistBroker_AllowAllEgress is the --yolo broker posture: every
// egress is granted regardless of the allowlist, but credentials stay
// explicit (all-network ≠ all-secrets).
func TestAllowlistBroker_AllowAllEgress(t *testing.T) {
	b := &AllowlistBroker{
		Creds:          map[string]string{"GH_TOKEN": "ghp_x"},
		Egress:         map[string]bool{},
		AllowAllEgress: true,
	}
	ctx := context.Background()

	// Any egress, never listed → granted under yolo.
	if g := b.Authorize(ctx, wire.CapabilityRequest{Type: wire.CapEgress, Name: "anything.example:443"}); !g.Granted {
		t.Errorf("AllowAllEgress must grant any egress: %+v", g)
	}
	// Credentials are still explicit: an unlisted one is denied.
	if g := b.Authorize(ctx, wire.CapabilityRequest{Type: wire.CapCredential, Name: "AWS"}); g.Granted {
		t.Errorf("AllowAllEgress must NOT open credentials: %+v", g)
	}
	// A listed credential still works.
	if g := b.Authorize(ctx, wire.CapabilityRequest{Type: wire.CapCredential, Name: "GH_TOKEN"}); !g.Granted || g.Secret != "ghp_x" {
		t.Errorf("listed credential under yolo: %+v", g)
	}
}

// TestEgressBrokerOpts_Yolo verifies the wiring: --yolo starts an
// allow-all proxy and an all-egress broker even with no configured
// allowlist or credentials (where the non-yolo path produces nothing).
func TestEgressBrokerOpts_Yolo(t *testing.T) {
	// No config, no yolo, non-interactive → no broker (default-deny stays).
	if opts := egressBrokerOpts(config.EgressConfig{}, false, false, func(string) {}); opts != nil {
		t.Fatal("no config, no yolo, headless: must yield no broker")
	}

	// No config, yolo → a broker + an injected allow-all proxy URL.
	var injected string
	opts := egressBrokerOpts(config.EgressConfig{}, true, false, func(u string) { injected = u })
	if opts == nil {
		t.Fatal("yolo must yield a broker even with no config")
	}
	if injected == "" {
		t.Fatal("yolo must start + inject the allow-all proxy as the box's route out")
	}
	s := &Session{}
	for _, o := range opts {
		o(s)
	}
	ab, ok := s.broker.(*AllowlistBroker)
	if !ok || !ab.AllowAllEgress {
		t.Fatalf("yolo broker must be an AllowlistBroker with AllowAllEgress: %#v", s.broker)
	}
	if g := ab.Authorize(context.Background(), wire.CapabilityRequest{Type: wire.CapEgress, Name: "x.example:443"}); !g.Granted {
		t.Errorf("yolo broker must grant arbitrary egress: %+v", g)
	}
}

// egressResponder subscribes to busInst and answers every
// EventEgressRequest with want, counting how many it saw. It mirrors
// what the TUI does, minus rendering.
func egressResponder(t *testing.T, busInst *bus.Bus, want permissions.EgressDecision) (*int, func()) {
	t.Helper()
	sub := busInst.Subscribe(16)
	seen := new(int)
	done := make(chan struct{})
	go func() {
		for ev := range sub {
			if ev.Type != bus.EventEgressRequest {
				continue
			}
			if p, ok := ev.Payload.(*permissions.EgressPrompt); ok {
				*seen++
				p.Respond <- want
			}
		}
		close(done)
	}()
	return seen, func() { busInst.Close(); <-done }
}

func TestInteractiveBroker_StaticAndNonInteractive(t *testing.T) {
	static := &AllowlistBroker{
		Creds:  map[string]string{"GH": "tok"},
		Egress: map[string]bool{"api.github.com:443": true},
	}

	// Non-interactive: configured egress still passes (static path), but
	// anything else denies WITHOUT prompting (no bus bound, no hang).
	ib := NewInteractiveBroker(static, false)
	ctx := context.Background()
	if g := ib.Authorize(ctx, wire.CapabilityRequest{Type: wire.CapEgress, Name: "api.github.com:443"}); !g.Granted {
		t.Errorf("configured egress must pass via static: %+v", g)
	}
	if ib.AuthorizeEgress(ctx, "evil.example:443") {
		t.Error("non-interactive must deny an unlisted target")
	}
	if g := ib.Authorize(ctx, wire.CapabilityRequest{Type: wire.CapEgress, Name: "evil.example:443"}); g.Granted || g.Reason == "" {
		t.Errorf("non-interactive deny must carry an actionable reason: %+v", g)
	}
	// Credentials are never prompted — delegated straight to static.
	if g := ib.Authorize(ctx, wire.CapabilityRequest{Type: wire.CapCredential, Name: "GH"}); !g.Granted || g.Secret != "tok" {
		t.Errorf("credential must delegate to static: %+v", g)
	}
}

func TestInteractiveBroker_PromptAllowOnceAndTaskCache(t *testing.T) {
	pr := egress.New()
	if err := pr.Start(); err != nil {
		t.Fatalf("proxy start: %v", err)
	}
	defer pr.Close()
	static := &AllowlistBroker{Egress: map[string]bool{}, Proxy: pr}
	ib := NewInteractiveBroker(static, true)

	busInst := bus.New()
	ib.BindBus(busInst)
	seen, stop := egressResponder(t, busInst, permissions.EgressAllowTask)
	defer stop()

	ctx := context.Background()
	if !ib.AuthorizeEgress(ctx, "research.example:443") {
		t.Fatal("granted prompt must allow")
	}
	// AllowTask must be memoised: a second call to the same target is
	// served from cache without a new prompt.
	if !ib.AuthorizeEgress(ctx, "research.example:443") {
		t.Fatal("cached task-grant must still allow")
	}
	if *seen != 1 {
		t.Fatalf("AllowTask must prompt once then cache, prompted %d times", *seen)
	}
	// A granted target is opened on the proxy so the connection passes.
	if !pr.Allowed("research.example:443") {
		t.Error("granted egress must be opened on the proxy")
	}
}

func TestInteractiveBroker_PromptDenyAndCancel(t *testing.T) {
	ib := NewInteractiveBroker(&AllowlistBroker{Egress: map[string]bool{}}, true)
	busInst := bus.New()
	ib.BindBus(busInst)
	_, stop := egressResponder(t, busInst, permissions.EgressDeny)
	defer stop()

	if ib.AuthorizeEgress(context.Background(), "blocked.example:443") {
		t.Error("a denied prompt must refuse")
	}

	// Context cancellation while waiting must unblock as a refusal.
	ib2 := NewInteractiveBroker(&AllowlistBroker{Egress: map[string]bool{}}, true)
	silent := bus.New() // nobody answers
	ib2.BindBus(silent)
	silent.Subscribe(1) // a subscriber that never responds
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if ib2.AuthorizeEgress(ctx, "hang.example:443") {
		t.Error("cancelled prompt must refuse, not hang")
	}
}

func TestEgressBrokerOpts_Interactive(t *testing.T) {
	// No config, no yolo, but interactive → an InteractiveBroker plus a
	// started (empty, default-deny) proxy wired as its decider.
	var injected string
	opts := egressBrokerOpts(config.EgressConfig{}, false, true, func(u string) { injected = u })
	if opts == nil {
		t.Fatal("interactive must yield a broker even with no config")
	}
	if injected == "" {
		t.Fatal("interactive must start+inject the proxy so a grant has a route out")
	}
	s := &Session{}
	for _, o := range opts {
		o(s)
	}
	ib, ok := s.broker.(*InteractiveBroker)
	if !ok {
		t.Fatalf("interactive broker expected, got %#v", s.broker)
	}
	if ib.Static == nil || ib.Static.Proxy == nil {
		t.Fatal("interactive broker must wrap a static broker holding the proxy")
	}
}
