// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"path/filepath"
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
)

// TestSetBackend_Provenance covers the execution-provenance column:
// SetBackend stamps it, both listing reads surface it, and re-stamping
// (a resume under a different backend) records the LATEST value.
func TestSetBackend_Provenance(t *testing.T) {
	s, err := OpenAt(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	defer s.Close()

	w, err := NewSessionWithID(s, "sid-1", "m", "test", "/proj")
	if err != nil {
		t.Fatalf("NewSessionWithID: %v", err)
	}
	// A message so ListRecentWithStats (HAVING msg_count > 0) sees it.
	if _, err := w.AppendMessage(llm.Message{Role: "user", Content: "hi"}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// Default: unknown (pre-provenance rows look the same).
	infos, err := ListRecent(s, "", 10)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(infos) != 1 || infos[0].Backend != "" {
		t.Fatalf("fresh row backend = %q, want empty", infos[0].Backend)
	}

	if err := w.SetBackend("podman"); err != nil {
		t.Fatalf("SetBackend: %v", err)
	}
	stats, err := ListRecentWithStats(s, "", 10)
	if err != nil {
		t.Fatalf("ListRecentWithStats: %v", err)
	}
	if len(stats) != 1 || stats[0].Backend != "podman" {
		t.Fatalf("backend after stamp = %q, want podman", stats[0].Backend)
	}

	// Resume under a different backend re-stamps to the LATEST.
	if err := w.SetBackend("lima"); err != nil {
		t.Fatalf("SetBackend (restamp): %v", err)
	}
	infos, err = ListRecent(s, "", 10)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if infos[0].Backend != "lima" {
		t.Fatalf("backend after restamp = %q, want lima", infos[0].Backend)
	}
}
