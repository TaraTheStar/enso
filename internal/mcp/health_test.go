// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/config"
)

func TestManager_RecordsInvalidNameAsFailed(t *testing.T) {
	m := NewManager()
	// Names with whitespace fail validateName immediately, before any
	// dial attempt — fast deterministic failure path that exercises
	// the recordFailure code without needing a live MCP server.
	m.Start(context.Background(), map[string]config.MCPConfig{
		"bad name": {},
	})

	got := m.ConfiguredNames()
	if !reflect.DeepEqual(got, []string{"bad name"}) {
		t.Errorf("ConfiguredNames=%v, want [bad name]", got)
	}
	state, reason := m.State("bad name")
	if state != StateFailed {
		t.Errorf("state=%v, want Failed", state)
	}
	if reason == "" {
		t.Error("expected non-empty failure reason")
	}
}

func TestManager_ConfiguredNamesSorted(t *testing.T) {
	m := NewManager()
	m.Start(context.Background(), map[string]config.MCPConfig{
		"zulu":  {Command: "/nonexistent/foo"},
		"alpha": {Command: "/nonexistent/bar"},
		"mike":  {Command: "/nonexistent/baz"},
	})
	got := m.ConfiguredNames()
	want := []string{"alpha", "mike", "zulu"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ConfiguredNames=%v, want %v", got, want)
	}
	// All should be StateFailed (commands don't exist).
	for _, n := range want {
		state, _ := m.State(n)
		if state != StateFailed {
			t.Errorf("%q state=%v, want Failed (nonexistent command)", n, state)
		}
	}
}

func TestManager_StateForUnknownNameDefaultsHealthy(t *testing.T) {
	// Defensive: callers should only ask about names returned by
	// ConfiguredNames, but we don't want a stray probe to surface as
	// "failed" and confuse the sidebar.
	m := NewManager()
	state, reason := m.State("never-configured")
	if state != StateHealthy {
		t.Errorf("state=%v, want Healthy default", state)
	}
	if reason != "" {
		t.Errorf("reason=%q, want empty", reason)
	}
}

func TestManager_MarkFailedRequiresConfigured(t *testing.T) {
	// Don't let an arbitrary string sneak into the status map — it
	// would make ConfiguredNames inconsistent with State.
	m := NewManager()
	m.MarkFailed("phantom", errors.New("nope"))
	state, _ := m.State("phantom")
	if state != StateHealthy {
		t.Errorf("state=%v, want Healthy (phantom not in configured)", state)
	}
}

func TestManager_MarkFailedFlipsHealthyToFailed(t *testing.T) {
	m := NewManager()
	m.mu.Lock()
	m.configured = []string{"good"}
	m.status["good"] = &serverStatus{state: StateHealthy}
	m.mu.Unlock()

	m.MarkFailed("good", errors.New("broken pipe"))
	state, reason := m.State("good")
	if state != StateFailed {
		t.Errorf("state=%v, want Failed", state)
	}
	if reason == "" {
		t.Error("expected reason after MarkFailed")
	}
}

func TestShortErr_TruncatesLong(t *testing.T) {
	long := errors.New("a really really really really really really really really really really really long error message")
	got := shortErr(long)
	if len(got) > 80 {
		t.Errorf("shortErr returned %d chars, want <=80", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("shortErr should end with ellipsis when truncating, got %q", got)
	}
}
