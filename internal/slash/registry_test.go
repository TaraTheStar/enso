// SPDX-License-Identifier: AGPL-3.0-or-later

package slash

import (
	"context"
	"testing"
)

// stubCmd implements Command for registry tests without dragging in the
// full Skill plumbing.
type stubCmd struct {
	name, desc string
}

func (s stubCmd) Name() string                          { return s.name }
func (s stubCmd) Description() string                   { return s.desc }
func (s stubCmd) Run(_ context.Context, _ string) error { return nil }

func TestRegistry_RegisterGetList(t *testing.T) {
	r := NewRegistry()
	r.Register(stubCmd{name: "help", desc: "h"})
	r.Register(stubCmd{name: "yolo", desc: "y"})

	if got := r.Get("help"); got == nil || got.Name() != "help" {
		t.Errorf("Get(help) = %v", got)
	}
	if got := r.Get("missing"); got != nil {
		t.Errorf("Get(missing) = %+v, want nil", got)
	}
	list := r.List()
	if len(list) != 2 {
		t.Fatalf("List len = %d, want 2", len(list))
	}
	// Sorted alphabetically.
	if list[0].Name() != "help" || list[1].Name() != "yolo" {
		t.Errorf("List order = [%s %s]", list[0].Name(), list[1].Name())
	}
}

func TestRegistry_RegisterReplaces(t *testing.T) {
	r := NewRegistry()
	r.Register(stubCmd{name: "help", desc: "first"})
	r.Register(stubCmd{name: "help", desc: "second"})

	if got := r.Get("help"); got == nil || got.Description() != "second" {
		t.Errorf("Description = %q, want second", got.Description())
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		name string
		args string
		ok   bool
	}{
		{"/help", "help", "", true},
		{"/yolo on", "yolo", "on", true},
		{"  /workflow build-feature add x  ", "workflow", "build-feature add x", true},
		{"/", "", "", true},            // empty name still parses with ok=true
		{"hello world", "", "", false}, // not a slash command
		{"", "", "", false},
		{"   ", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			name, args, ok := Parse(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if name != tc.name || args != tc.args {
				t.Errorf("got (%q, %q), want (%q, %q)", name, args, tc.name, tc.args)
			}
		})
	}
}
