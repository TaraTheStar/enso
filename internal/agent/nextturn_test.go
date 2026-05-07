// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"reflect"
	"testing"
)

func TestSetNextTurnTools_OneShot(t *testing.T) {
	a := &Agent{}

	if got := a.consumeNextTurnTools(); got != nil {
		t.Errorf("initial consume: got %v, want nil", got)
	}

	a.SetNextTurnTools([]string{"read", "grep"})
	got := a.consumeNextTurnTools()
	if !reflect.DeepEqual(got, []string{"read", "grep"}) {
		t.Errorf("first consume: got %v, want [read grep]", got)
	}

	if got := a.consumeNextTurnTools(); got != nil {
		t.Errorf("second consume: got %v, want nil (one-shot)", got)
	}
}

func TestSetNextTurnTools_NilClears(t *testing.T) {
	a := &Agent{}
	a.SetNextTurnTools([]string{"read"})
	a.SetNextTurnTools(nil)
	if got := a.consumeNextTurnTools(); got != nil {
		t.Errorf("after nil-set: got %v, want nil", got)
	}
}

func TestSetNextTurnTools_Defensive(t *testing.T) {
	// SetNextTurnTools must copy its input so a caller mutating the
	// passed slice afterwards doesn't change what we'll consume.
	a := &Agent{}
	src := []string{"read", "grep"}
	a.SetNextTurnTools(src)
	src[0] = "MUTATED"
	got := a.consumeNextTurnTools()
	if got[0] != "read" {
		t.Errorf("got[0] = %q, want read (defensive copy missing)", got[0])
	}
}
