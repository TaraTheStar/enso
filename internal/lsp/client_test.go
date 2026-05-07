// SPDX-License-Identifier: AGPL-3.0-or-later

package lsp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestFlattenHoverContents covers the four shapes hover responses come
// in: MarkupContent, plain string, MarkedString, and array of mixed.
func TestFlattenHoverContents(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"markup", `{"kind":"markdown","value":"# T\nfoo"}`, "# T\nfoo"},
		{"string", `"plain text"`, "plain text"},
		{"marked", `{"language":"go","value":"func Foo()"}`, "func Foo()"},
		{"list-mixed", `[{"language":"go","value":"sig"}, "details"]`, "sig\ndetails"},
		{"empty", `null`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := flattenHoverContents(json.RawMessage(tc.json))
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDecodeLocations covers the three shapes definition/references
// responses come in: single Location, array of Location, array of
// LocationLink.
func TestDecodeLocations(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		wantURI string
		wantN   int
	}{
		{"null", `null`, "", 0},
		{
			"single",
			`{"uri":"file:///a.go","range":{"start":{"line":1,"character":2},"end":{"line":1,"character":5}}}`,
			"file:///a.go", 1,
		},
		{
			"array-locations",
			`[{"uri":"file:///a.go","range":{"start":{"line":1,"character":0},"end":{"line":1,"character":1}}},
			  {"uri":"file:///b.go","range":{"start":{"line":2,"character":0},"end":{"line":2,"character":1}}}]`,
			"file:///a.go", 2,
		},
		{
			"array-links",
			`[{"targetUri":"file:///c.go","targetRange":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"targetSelectionRange":{"start":{"line":3,"character":4},"end":{"line":3,"character":7}}}]`,
			"file:///c.go", 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeLocations(json.RawMessage(tc.json))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.wantN {
				t.Fatalf("len = %d, want %d", len(got), tc.wantN)
			}
			if tc.wantN > 0 && got[0].URI != tc.wantURI {
				t.Errorf("first URI = %q, want %q", got[0].URI, tc.wantURI)
			}
		})
	}
}

// TestDiagnosticsCacheRoundtrip wires a fixture, fires a synthetic
// publishDiagnostics notification, and confirms Client.Diagnostics
// returns it.
func TestDiagnosticsCacheRoundtrip(t *testing.T) {
	f := newFixture(t)
	defer f.stop()
	cl := NewClient(f.client)

	go func() {
		writeServerMessage(t, f.serverOut, &Message{
			JSONRPC: "2.0",
			Method:  "textDocument/publishDiagnostics",
			Params: json.RawMessage(`{
				"uri": "file:///x.go",
				"diagnostics": [
					{"range":{"start":{"line":3,"character":0},"end":{"line":3,"character":10}},
					 "severity":1, "message":"undefined: Foo"}
				]
			}`),
		})
	}()

	// Poll for the notification to land — the dispatch goroutine runs
	// asynchronously to the test, so a small grace window is needed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		diags := cl.Diagnostics("file:///x.go")
		if len(diags) > 0 {
			if !strings.Contains(diags[0].Message, "undefined") {
				t.Errorf("unexpected diag: %+v", diags[0])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("publishDiagnostics never landed in the cache")
}
