// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package bubble

import (
	"reflect"
	"testing"
)

func TestBuildSwitchArgs(t *testing.T) {
	cases := []struct {
		name string
		orig []string
		id   string
		want []string
	}{
		{
			name: "no existing --session: append",
			orig: []string{"/bin/enso", "tui"},
			id:   "abc123",
			want: []string{"/bin/enso", "tui", "--session", "abc123"},
		},
		{
			name: "replace `--session foo` form",
			orig: []string{"/bin/enso", "tui", "--session", "foo"},
			id:   "bar",
			want: []string{"/bin/enso", "tui", "--session", "bar"},
		},
		{
			name: "replace `--session=foo` form",
			orig: []string{"/bin/enso", "tui", "--session=foo", "--yolo"},
			id:   "bar",
			want: []string{"/bin/enso", "tui", "--yolo", "--session", "bar"},
		},
		{
			name: "preserves other flags",
			orig: []string{"/bin/enso", "--yolo", "tui", "--session", "old", "--debug"},
			id:   "new",
			want: []string{"/bin/enso", "--yolo", "tui", "--debug", "--session", "new"},
		},
		{
			name: "drops --continue and --resume too",
			orig: []string{"/bin/enso", "--continue", "--resume", "x"},
			id:   "new",
			want: []string{"/bin/enso", "--session", "new"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildSwitchArgs(tc.orig, tc.id)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %v\nwant %v", got, tc.want)
			}
		})
	}
}

func TestBuildNewSessionArgs(t *testing.T) {
	cases := []struct {
		name string
		orig []string
		want []string
	}{
		{
			name: "no session flags: unchanged",
			orig: []string{"/bin/enso", "tui"},
			want: []string{"/bin/enso", "tui"},
		},
		{
			name: "strips `--session foo` form, appends nothing",
			orig: []string{"/bin/enso", "tui", "--session", "foo"},
			want: []string{"/bin/enso", "tui"},
		},
		{
			name: "strips `--session=foo`, preserves other flags",
			orig: []string{"/bin/enso", "tui", "--session=foo", "--yolo"},
			want: []string{"/bin/enso", "tui", "--yolo"},
		},
		{
			name: "drops --continue and --resume, keeps --ephemeral",
			orig: []string{"/bin/enso", "--ephemeral", "--continue", "--resume", "x"},
			want: []string{"/bin/enso", "--ephemeral"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildNewSessionArgs(tc.orig)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %v\nwant %v", got, tc.want)
			}
		})
	}
}
