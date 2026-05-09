// SPDX-License-Identifier: AGPL-3.0-or-later

package theme

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseHex(t *testing.T) {
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
			c, err := parseHex(tc.in)
			if tc.wantError {
				if err == nil {
					t.Errorf("expected error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if c.R != tc.wantR || c.G != tc.wantG || c.B != tc.wantB {
				t.Errorf("got (%d,%d,%d) want (%d,%d,%d)", c.R, c.G, c.B, tc.wantR, tc.wantG, tc.wantB)
			}
		})
	}
}

func TestColorHex(t *testing.T) {
	c := Color{R: 0xff, G: 0xd8, B: 0x66}
	if got := c.Hex(); got != "#ffd866" {
		t.Errorf("Hex() = %q, want #ffd866", got)
	}
}

func TestLoadFromFile_MissingFileReturnsEmpty(t *testing.T) {
	tmp := t.TempDir()
	p, err := LoadFromFile(filepath.Join(tmp, "no-such-theme.toml"))
	if err != nil {
		t.Errorf("missing file should be no-op, got %v", err)
	}
	if len(p) != 0 {
		t.Errorf("missing file should return empty palette, got %v", p)
	}
}

func TestLoadFromFile_ReturnsOverrides(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "theme.toml")
	if err := os.WriteFile(path, []byte(`
[colors]
yellow = "#ffd866"
teal   = "#78dce8"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c := p["yellow"]; c.R != 0xff || c.G != 0xd8 || c.B != 0x66 {
		t.Errorf("yellow override wrong: %+v", c)
	}
	if c := p["teal"]; c.R != 0x78 || c.G != 0xdc || c.B != 0xe8 {
		t.Errorf("teal override wrong: %+v", c)
	}
}

func TestLoadFromFile_BadHexIsError(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "theme.toml")
	if err := os.WriteFile(path, []byte(`
[colors]
yellow = "not-a-hex-string"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFromFile(path); err == nil {
		t.Errorf("expected hex-parse error")
	}
}

func TestLoadFromFile_LowercasesNames(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "theme.toml")
	if err := os.WriteFile(path, []byte(`
[colors]
YELLOW = "#abcdef"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := p["YELLOW"]; ok {
		t.Errorf("name should be lowercased; got upper-case key in %v", p)
	}
	if c := p["yellow"]; c.R != 0xab || c.G != 0xcd || c.B != 0xef {
		t.Errorf("yellow not normalised: %+v", c)
	}
}

func TestDefault_ContainsExpectedNames(t *testing.T) {
	p := Default()
	expected := []string{"mauve", "lavender", "comment", "dust", "sage", "yellow", "teal", "red", "gray", "green"}
	for _, name := range expected {
		if _, ok := p[name]; !ok {
			t.Errorf("default palette missing %q", name)
		}
	}
}
