// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"sync"
	"time"
)

// ConnState is the LLM-provider connection state surfaced to the TUI
// status bar. It tracks the *transport* relationship to the configured
// provider endpoint — HTTP-status errors (401, 429, 5xx) leave the state
// alone since the TLS+TCP path was healthy enough to get a response.
type ConnState int

const (
	StateConnected ConnState = iota
	StateReconnecting
	StateDisconnected
)

func (s ConnState) String() string {
	switch s {
	case StateConnected:
		return "connected"
	case StateReconnecting:
		return "reconnecting"
	case StateDisconnected:
		return "disconnected"
	default:
		return "unknown"
	}
}

// ConnStateReporter is the read side of a Client's connection-state
// tracker. The TUI status bar takes a *Provider.Client and type-asserts
// to this interface; test fakes that don't implement it simply render
// nothing (treated as healthy).
type ConnStateReporter interface {
	LLMConnState() ConnState
}

// connTracker holds a Client's last-known transport state plus the
// lifecycle bookkeeping for the recovery probe goroutine. All fields
// are mutex-guarded so Chat() (writer) and the TUI refresh ticker
// (reader) can race safely.
type connTracker struct {
	mu      sync.Mutex
	state   ConnState
	since   time.Time
	probing bool // true while a probe goroutine is running
}

func (t *connTracker) get() ConnState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

// set transitions the tracker to s and returns the previous state. The
// `since` timestamp only updates on an actual change so callers reading
// "how long have we been degraded?" see the original transition time.
func (t *connTracker) set(s ConnState) ConnState {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev := t.state
	if prev != s {
		t.state = s
		t.since = time.Now()
	}
	return prev
}

// claimProbe atomically marks a probe as running. Returns true if the
// caller is the one that should start the probe goroutine (idempotent
// across concurrent Disconnected transitions).
func (t *connTracker) claimProbe() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.probing {
		return false
	}
	t.probing = true
	return true
}

// releaseProbe clears the probing flag. Called from the probe goroutine
// before it exits.
func (t *connTracker) releaseProbe() {
	t.mu.Lock()
	t.probing = false
	t.mu.Unlock()
}
