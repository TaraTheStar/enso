// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package daemon

import (
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
)

// The daemon hosts every agent loop in-process, so cross-session pool
// coordination reduces to one invariant: every session's agent.New
// receives the same provider map (hence the same *llm.Pool pointers).
// sessionProviders is that seam.
func TestSessionProviders_SharesOneMapAcrossSessions(t *testing.T) {
	shared := llm.NewPoolNamed("gpu", 1, 0)
	fast := &llm.Provider{Name: "fast", Pool: shared, PoolName: "gpu"}
	deep := &llm.Provider{Name: "deep", Pool: shared, PoolName: "gpu"}
	s := &Server{
		provider:  fast,
		providers: map[string]*llm.Provider{"fast": fast, "deep": deep},
	}

	a := s.sessionProviders()
	b := s.sessionProviders()

	if a["fast"] != b["fast"] || a["deep"] != b["deep"] {
		t.Fatal("two sessions got different provider pointers — pools would not coordinate")
	}
	if a["fast"].Pool != a["deep"].Pool {
		t.Fatal("co-pooled providers must share one *llm.Pool")
	}
	if len(a) != 2 {
		t.Fatalf("expected full provider set exposed to sessions, got %d", len(a))
	}
}

// When only the legacy single `provider` field is set (Server built
// directly, e.g. older tests), sessionProviders still yields a usable
// one-entry map rather than nil.
func TestSessionProviders_FallsBackToSingleProvider(t *testing.T) {
	only := &llm.Provider{Name: "solo", Pool: llm.NewPool(1)}
	s := &Server{provider: only}

	got := s.sessionProviders()
	if len(got) != 1 || got["solo"] != only {
		t.Fatalf("fallback map wrong: %#v", got)
	}
}
