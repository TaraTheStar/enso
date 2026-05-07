// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestParseHexColor(t *testing.T) {
	cases := []struct {
		in        string
		wantR     int32
		wantG     int32
		wantB     int32
		wantError bool
	}{
		{"#ffd866", 0xff, 0xd8, 0x66, false},
		{"ffd866", 0xff, 0xd8, 0x66, false}, // # is optional
		{"  #19181a  ", 0x19, 0x18, 0x1a, false},
		{"#000000", 0, 0, 0, false},
		{"#ffffff", 0xff, 0xff, 0xff, false},
		// Invalid
		{"#abc", 0, 0, 0, true},    // 3-digit form unsupported
		{"#xyzxyz", 0, 0, 0, true}, // non-hex
		{"", 0, 0, 0, true},
		{"#1234567", 0, 0, 0, true}, // 7 digits
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			c, err := parseHexColor(tc.in)
			if tc.wantError {
				if err == nil {
					t.Errorf("expected error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			r, g, b := c.RGB()
			if r != tc.wantR || g != tc.wantG || b != tc.wantB {
				t.Errorf("got (%d,%d,%d) want (%d,%d,%d)", r, g, b, tc.wantR, tc.wantG, tc.wantB)
			}
		})
	}
}

func TestLoadTheme_MissingFileNoOp(t *testing.T) {
	tmp := t.TempDir()
	if err := LoadTheme(filepath.Join(tmp, "no-such-theme.toml")); err != nil {
		t.Errorf("missing file should be no-op, got %v", err)
	}
}

func TestLoadTheme_OverridesColorNames(t *testing.T) {
	// Snapshot the names we touch so the test doesn't pollute other tests.
	originals := map[string]tcell.Color{
		"yellow": tcell.ColorNames["yellow"],
		"teal":   tcell.ColorNames["teal"],
	}
	defer func() {
		for k, v := range originals {
			tcell.ColorNames[k] = v
		}
	}()

	tmp := t.TempDir()
	path := filepath.Join(tmp, "theme.toml")
	if err := os.WriteFile(path, []byte(`
[colors]
yellow = "#ffd866"
teal   = "#78dce8"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := LoadTheme(path); err != nil {
		t.Fatalf("load: %v", err)
	}

	r, g, b := tcell.ColorNames["yellow"].RGB()
	if r != 0xff || g != 0xd8 || b != 0x66 {
		t.Errorf("yellow override failed: got (%d,%d,%d)", r, g, b)
	}
	r, g, b = tcell.ColorNames["teal"].RGB()
	if r != 0x78 || g != 0xdc || b != 0xe8 {
		t.Errorf("teal override failed: got (%d,%d,%d)", r, g, b)
	}
}

func TestLoadTheme_BadHexIsError(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "theme.toml")
	if err := os.WriteFile(path, []byte(`
[colors]
yellow = "not-a-hex-string"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := LoadTheme(path); err == nil {
		t.Errorf("expected hex-parse error")
	}
}

func TestLoadTheme_NameIsLowercased(t *testing.T) {
	originals := map[string]tcell.Color{"yellow": tcell.ColorNames["yellow"]}
	defer func() {
		for k, v := range originals {
			tcell.ColorNames[k] = v
		}
	}()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "theme.toml")
	if err := os.WriteFile(path, []byte(`
[colors]
YELLOW = "#abcdef"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := LoadTheme(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	r, g, b := tcell.ColorNames["yellow"].RGB()
	if r != 0xab || g != 0xcd || b != 0xef {
		t.Errorf("uppercase key not normalised: (%d,%d,%d)", r, g, b)
	}
}
