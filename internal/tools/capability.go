// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import "context"

// Capability types. Mirror wire.Cap* (kept as plain strings here so the
// tools package doesn't depend on the backend wire package).
const (
	CapCredential = "credential"
	CapEgress     = "egress"
)

// CapabilityRequester is the worker-side handle to the host's tier-3
// capability broker. When the worker is network-sealed and runs with an
// empty environment (the sandboxed backend), a tool that needs a
// host secret or a one-off outbound connection asks for it here instead
// of reading the environment or dialing directly. The host brokers it;
// the default answer is deny.
//
// AgentContext.Capabilities is nil on the in-process / LocalBackend
// path (no seam, no sealing) — callers must treat nil as "no broker;
// behave as today" so unsealed runs are unaffected.
type CapabilityRequester interface {
	// RequestCapability asks the host for capType (CapCredential or
	// CapEgress) named name, with a human-facing reason for audit/UI.
	// For CapCredential a granted call returns the secret value; for
	// CapEgress secret is empty and ok alone signals the allowance.
	// ok=false is the default-deny outcome.
	RequestCapability(ctx context.Context, capType, name, reason string) (secret string, ttlSeconds int, ok bool)
}
