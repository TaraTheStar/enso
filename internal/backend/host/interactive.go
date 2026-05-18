// SPDX-License-Identifier: AGPL-3.0-or-later

package host

import (
	"context"
	"log/slog"
	"sync"

	"github.com/TaraTheStar/enso/internal/backend/wire"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/permissions"
)

// InteractiveBroker makes a sealed-by-default box usable for
// research/build work without pre-enumerating every domain: it wraps
// the static AllowlistBroker and, on anything the static policy does
// not already grant, prompts the operator (y / this-task / no) over the
// host bus — the same HITL shape as the tool-permission modal.
// Default-deny and host-mediation still hold; the operator just grants
// interactively instead of editing [bash.sandbox_options] egress up
// front.
//
// It is the single decision point for BOTH egress entry points, so they
// share one per-task cache and one prompt:
//   - the host egress proxy's denied-target callback (egress.Decider) —
//     this is what covers `bash` (curl/git/pip/npm/go) and, since they
//     route through the injected HTTPS_PROXY, web_fetch/web_search too;
//   - the worker tier-3 CapabilityBroker.Authorize path (credentials
//     stay strictly static — "all network" never means "all secrets").
//
// Fail-closed: a non-interactive run (enso run / no TTY) or a not-yet-
// bound bus denies with an actionable reason rather than hanging on a
// prompt nobody can answer.
type InteractiveBroker struct {
	Static      *AllowlistBroker // config allowlist / creds / yolo allow-all — instant path
	interactive bool             // false ⇒ deny-with-reason, never block

	mu    sync.Mutex
	bus   *bus.Bus        // host bus; injected by host.Start via BindBus
	cache map[string]bool // per-task "allow for this task" memo, keyed by host:port
}

// NewInteractiveBroker wraps static with interactive prompting. The bus
// is bound later (host.Start) once it exists.
func NewInteractiveBroker(static *AllowlistBroker, interactive bool) *InteractiveBroker {
	return &InteractiveBroker{
		Static:      static,
		interactive: interactive,
		cache:       map[string]bool{},
	}
}

// BindBus injects the host event bus. host.Start calls this after
// applying options (the bus does not exist at SelectBackend time).
func (b *InteractiveBroker) BindBus(busInst *bus.Bus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bus = busInst
}

// Authorize satisfies CapabilityBroker. Credentials are strictly static
// (never prompted). Egress falls through to the shared interactive
// decision.
func (b *InteractiveBroker) Authorize(ctx context.Context, req wire.CapabilityRequest) wire.CapabilityGrant {
	if req.Type != wire.CapEgress {
		return b.Static.Authorize(ctx, req) // credentials + unknown types
	}
	reason := req.Reason
	if reason == "" {
		reason = "sandboxed tool requested network egress"
	}
	if b.decide(ctx, req.Name, reason) {
		return wire.CapabilityGrant{Granted: true, TTLSeconds: b.Static.TTL}
	}
	return wire.CapabilityGrant{Reason: b.denyReason(req.Name)}
}

// AuthorizeEgress satisfies egress.Decider — the proxy's callback on a
// target that is not on the static allowlist. This is the path that
// uniformly covers bash and the proxied web_* tools.
func (b *InteractiveBroker) AuthorizeEgress(ctx context.Context, hostport string) bool {
	return b.decide(ctx, hostport, "a sandboxed command tried to reach the network")
}

// decide is the shared core: static grant → cache → prompt. On a grant
// it opens the target on the proxy so the rest of the connection (and
// retries) pass without re-asking; the proxy path also self-promotes,
// which is idempotent.
func (b *InteractiveBroker) decide(ctx context.Context, target, reason string) bool {
	// Static policy (config allowlist, or --yolo allow-all) is the
	// instant, no-prompt path.
	if g := b.Static.Authorize(ctx, wire.CapabilityRequest{Type: wire.CapEgress, Name: target}); g.Granted {
		return true
	}

	b.mu.Lock()
	if cached, seen := b.cache[target]; seen {
		b.mu.Unlock()
		return cached
	}
	busInst := b.bus
	b.mu.Unlock()

	if !b.interactive || busInst == nil {
		slog.Info("egress denied (non-interactive)", "target", target)
		return false
	}

	respond := make(chan permissions.EgressDecision, 1) // buffered: never wedge on a gone subscriber
	busInst.Publish(bus.Event{
		Type:    bus.EventEgressRequest,
		Payload: &permissions.EgressPrompt{Target: target, Reason: reason, Respond: respond},
	})

	select {
	case d := <-respond:
		allow := d == permissions.EgressAllowOnce || d == permissions.EgressAllowTask
		if d == permissions.EgressAllowTask {
			b.mu.Lock()
			b.cache[target] = true
			b.mu.Unlock()
		}
		if allow && b.Static.Proxy != nil {
			b.Static.Proxy.Allow(target)
		}
		slog.Info("egress decision", "target", target, "granted", allow,
			"scope", map[bool]string{true: "task", false: "once"}[d == permissions.EgressAllowTask])
		return allow
	case <-ctx.Done():
		slog.Info("egress prompt cancelled", "target", target)
		return false
	}
}

func (b *InteractiveBroker) denyReason(target string) string {
	if !b.interactive {
		return "egress to " + target + " denied (no interactive TTY; pass --yolo or add it to [bash.sandbox_options] egress)"
	}
	return "egress to " + target + " denied by the operator"
}
