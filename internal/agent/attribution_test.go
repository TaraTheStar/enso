// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"strings"
	"testing"
)

func TestGitAttributionNote(t *testing.T) {
	cases := []struct {
		style, name string
		wantEmpty   bool
		wantSubs    []string
	}{
		{"", "", true, nil},
		{"none", "", true, nil},
		{"NONE", "anything", true, nil},
		{"unknown-style", "x", true, nil},

		{"co-authored-by", "", false, []string{"Co-Authored-By: enso"}},
		{"co-authored-by", "robo", false, []string{"Co-Authored-By: robo"}},
		{"  Co-Authored-By  ", "robo", false, []string{"Co-Authored-By: robo"}},

		{"assisted-by", "", false, []string{"Assisted-by: enso"}},
		{"assisted-by", "Robot", false, []string{"Assisted-by: Robot"}},
	}
	for _, tc := range cases {
		t.Run(tc.style+"/"+tc.name, func(t *testing.T) {
			got := gitAttributionNote(tc.style, tc.name)
			if tc.wantEmpty {
				if got != "" {
					t.Errorf("expected empty, got %q", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("expected non-empty for style %q", tc.style)
			}
			for _, s := range tc.wantSubs {
				if !strings.Contains(got, s) {
					t.Errorf("output missing %q\nfull:\n%s", s, got)
				}
			}
		})
	}
}
