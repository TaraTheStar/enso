// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"encoding/json"
	"testing"
)

// TestToolDefsByteStable is the H8/Q1 cache-stability guard: the serialised
// tool definitions sent to the provider must be byte-identical across calls,
// or every turn busts the prompt prefix cache. encoding/json sorts map keys
// at every nesting level, and ToolDefs() sorts the tool array by name, so
// this should hold — the test locks the guarantee against a future tool
// whose Parameters() returns a non-deterministic shape.
func TestToolDefsByteStable(t *testing.T) {
	build := func() []byte {
		r := NewRegistry()
		r.Register(ReadTool{})
		r.Register(BashTool{})
		r.Register(GrepTool{})
		b, err := json.Marshal(r.ToolDefs())
		if err != nil {
			t.Fatalf("marshal tool defs: %v", err)
		}
		return b
	}
	first := build()
	for i := 0; i < 5; i++ {
		if got := build(); string(got) != string(first) {
			t.Fatalf("tool-def serialization not byte-stable on iteration %d:\nfirst: %s\ngot:   %s", i, first, got)
		}
	}
}
