// SPDX-License-Identifier: AGPL-3.0-or-later

package lsp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// newTestClient constructs a Client without any backing Conn so the
// notification path can be driven synthetically. handleNotification
// uses Conn only on outbound calls, so leaving it nil is fine for the
// WaitForDiagnostics state-machine tests.
func newTestClient() *Client {
	return &Client{
		opened:      map[string]bool{},
		docVersions: map[string]int{},
		diagsByURI:  map[string][]Diagnostic{},
		diagWaiters: map[string]chan struct{}{},
	}
}

func mkDiagPayload(uri string, diags []Diagnostic) json.RawMessage {
	type publishParams struct {
		URI         string       `json:"uri"`
		Diagnostics []Diagnostic `json:"diagnostics"`
	}
	b, _ := json.Marshal(publishParams{URI: uri, Diagnostics: diags})
	return b
}

// TestWaitForDiagnostics_ReceivesNextPublication confirms the channel
// fires only on publications that arrive AFTER the wait starts —
// cached diagnostics from an earlier publication are not enough.
func TestWaitForDiagnostics_ReceivesNextPublication(t *testing.T) {
	c := newTestClient()
	uri := "file:///a.go"

	// Pre-seed the cache with a stale publication; WaitForDiagnostics
	// must NOT short-circuit on this.
	c.handleNotification("textDocument/publishDiagnostics",
		mkDiagPayload(uri, []Diagnostic{{Message: "old"}}))

	got := make(chan []Diagnostic, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		got <- c.WaitForDiagnostics(ctx, uri, 0)
	}()

	// Give the goroutine a moment to register its waiter, then publish.
	time.Sleep(20 * time.Millisecond)
	c.handleNotification("textDocument/publishDiagnostics",
		mkDiagPayload(uri, []Diagnostic{{Message: "fresh"}}))

	select {
	case d := <-got:
		if len(d) != 1 || d[0].Message != "fresh" {
			t.Errorf("got %+v, want one fresh diagnostic", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForDiagnostics didn't return after publication")
	}
}

// TestWaitForDiagnostics_CtxCancel exercises the cancellation path:
// when the context fires before any publication, WaitForDiagnostics
// returns nil and tidies its waiter slot (so a subsequent publication
// for the same URI doesn't crash trying to close an absent channel).
func TestWaitForDiagnostics_CtxCancel(t *testing.T) {
	c := newTestClient()
	uri := "file:///b.go"

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	got := c.WaitForDiagnostics(ctx, uri, 0)
	if got != nil {
		t.Errorf("expected nil on ctx timeout, got %+v", got)
	}

	// Publication AFTER ctx fired must not panic; the waiter slot is
	// gone, so this is just a plain cache write.
	c.handleNotification("textDocument/publishDiagnostics",
		mkDiagPayload(uri, []Diagnostic{{Message: "late"}}))
}

// TestWaitForDiagnostics_DedupWindow checks the second-publication
// behaviour: when dedup > 0, a server that emits "interim" diagnostics
// followed by a "real" set in quick succession has the FINAL state
// observed.
func TestWaitForDiagnostics_DedupWindow(t *testing.T) {
	c := newTestClient()
	uri := "file:///c.go"

	got := make(chan []Diagnostic, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		got <- c.WaitForDiagnostics(ctx, uri, 80*time.Millisecond)
	}()

	time.Sleep(20 * time.Millisecond)
	// First publication: empty "analysing..." style.
	c.handleNotification("textDocument/publishDiagnostics",
		mkDiagPayload(uri, []Diagnostic{}))
	// Second publication arrives within the dedup window with the
	// real answer.
	time.Sleep(20 * time.Millisecond)
	c.handleNotification("textDocument/publishDiagnostics",
		mkDiagPayload(uri, []Diagnostic{{Message: "real error"}}))

	select {
	case d := <-got:
		if len(d) != 1 || d[0].Message != "real error" {
			t.Errorf("dedup window did not surface follow-up: got %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForDiagnostics did not return")
	}
}

// TestWaitForDiagnostics_MultipleWaitersSameURI verifies the broadcast
// behaviour: two concurrent callers on the same URI both wake on a
// single publication.
func TestWaitForDiagnostics_MultipleWaitersSameURI(t *testing.T) {
	c := newTestClient()
	uri := "file:///d.go"

	done1 := make(chan struct{})
	done2 := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.WaitForDiagnostics(ctx, uri, 0)
		close(done1)
	}()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.WaitForDiagnostics(ctx, uri, 0)
		close(done2)
	}()

	time.Sleep(20 * time.Millisecond)
	c.handleNotification("textDocument/publishDiagnostics",
		mkDiagPayload(uri, []Diagnostic{{Message: "shared"}}))

	for i, ch := range []chan struct{}{done1, done2} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d didn't wake on single publication", i+1)
		}
	}
}

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
