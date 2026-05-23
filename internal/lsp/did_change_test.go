// SPDX-License-Identifier: AGPL-3.0-or-later

package lsp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

// parseSentMessages walks the framed wire output a Conn produced and
// returns the parsed JSON-RPC envelopes in send order. The frame is
// always "Content-Length: N\r\n\r\n<body>" so we read headers up to
// the blank line, slurp N bytes, repeat.
func parseSentMessages(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	r := bytes.NewReader(raw)
	for {
		// Read until "\r\n\r\n".
		var hdr bytes.Buffer
		for {
			b, err := r.ReadByte()
			if err == io.EOF {
				return out
			}
			if err != nil {
				t.Fatalf("read header byte: %v", err)
			}
			hdr.WriteByte(b)
			if strings.HasSuffix(hdr.String(), "\r\n\r\n") {
				break
			}
		}
		var contentLen int
		for line := range strings.SplitSeq(hdr.String(), "\r\n") {
			if strings.HasPrefix(line, "Content-Length:") {
				_, _ = fmt.Sscanf(line, "Content-Length: %d", &contentLen)
			}
		}
		if contentLen == 0 {
			t.Fatalf("missing/zero Content-Length in %q", hdr.String())
		}
		body := make([]byte, contentLen)
		if _, err := io.ReadFull(r, body); err != nil {
			t.Fatalf("read body: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("decode body %q: %v", body, err)
		}
		out = append(out, m)
	}
}

// newClientWithCapture wires a Client over a bytes.Buffer so outgoing
// notifications can be observed without standing up a real LSP server.
// The reader side is an empty buffer; Conn.Run is never invoked so the
// EOF doesn't matter.
func newClientWithCapture() (*Client, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	conn := NewConn(buf, &bytes.Buffer{})
	return NewClient(conn), buf
}

// TestDidChange_VersionMonotonic confirms the wire version increments
// 2, 3, 4 across three didChange calls after the initial didOpen
// (which carries version 1).
func TestDidChange_VersionMonotonic(t *testing.T) {
	c, buf := newClientWithCapture()
	uri := "file:///a.go"
	if err := c.DidOpen(uri, "go", "v1"); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := c.DidChange(uri, fmt.Sprintf("v%d", i+2)); err != nil {
			t.Fatal(err)
		}
	}

	msgs := parseSentMessages(t, buf.Bytes())
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4 (1 didOpen + 3 didChange)", len(msgs))
	}
	if msgs[0]["method"] != "textDocument/didOpen" {
		t.Errorf("msg[0] method: got %v, want textDocument/didOpen", msgs[0]["method"])
	}
	for i := 1; i <= 3; i++ {
		if msgs[i]["method"] != "textDocument/didChange" {
			t.Errorf("msg[%d] method: got %v, want textDocument/didChange", i, msgs[i]["method"])
		}
		params := msgs[i]["params"].(map[string]any)
		td := params["textDocument"].(map[string]any)
		gotVersion := int(td["version"].(float64))
		if gotVersion != i+1 {
			t.Errorf("msg[%d] version: got %d, want %d", i, gotVersion, i+1)
		}
		changes := params["contentChanges"].([]any)
		if len(changes) != 1 {
			t.Errorf("msg[%d] contentChanges len: got %d, want 1", i, len(changes))
		}
		change := changes[0].(map[string]any)
		wantText := fmt.Sprintf("v%d", i+1)
		if change["text"].(string) != wantText {
			t.Errorf("msg[%d] text: got %q, want %q", i, change["text"], wantText)
		}
	}
}

// TestDidChange_NoOpWhenNotOpen pins the safety guard: a didChange
// against an unopened URI silently no-ops rather than sending bogus
// wire traffic the server would reject.
func TestDidChange_NoOpWhenNotOpen(t *testing.T) {
	c, buf := newClientWithCapture()
	if err := c.DidChange("file:///never-opened.go", "stuff"); err != nil {
		t.Fatalf("DidChange: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("notification leaked: %q", buf.String())
	}
}

// TestDidSave_FiresWhenOpen exercises the DidSave path: it must send
// textDocument/didSave with the right URI when the document is open,
// and silently no-op otherwise.
func TestDidSave_FiresWhenOpen(t *testing.T) {
	c, buf := newClientWithCapture()
	uri := "file:///b.go"
	_ = c.DidOpen(uri, "go", "x")
	_ = c.DidSave(uri)

	msgs := parseSentMessages(t, buf.Bytes())
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (didOpen + didSave): %+v", len(msgs), msgs)
	}
	if msgs[1]["method"] != "textDocument/didSave" {
		t.Errorf("msg[1] method: got %v, want textDocument/didSave", msgs[1]["method"])
	}
	td := msgs[1]["params"].(map[string]any)["textDocument"].(map[string]any)
	if td["uri"] != uri {
		t.Errorf("didSave uri: got %v, want %s", td["uri"], uri)
	}
}

// TestRefreshBuffer_RoutesByOpenState confirms the notifier's
// refreshBuffer chooses DidOpen for unknown URIs (version=1) and
// DidChange+DidSave for already-open ones (no second open).
func TestRefreshBuffer_RoutesByOpenState(t *testing.T) {
	c, buf := newClientWithCapture()
	uri := "file:///c.go"

	// First touch: should be didOpen, no didSave yet.
	if err := refreshBuffer(c, uri, "go", "initial"); err != nil {
		t.Fatal(err)
	}
	msgs := parseSentMessages(t, buf.Bytes())
	if len(msgs) != 1 || msgs[0]["method"] != "textDocument/didOpen" {
		t.Fatalf("first call: got %+v, want one didOpen", msgs)
	}

	// Second touch: file is open, so didChange + didSave (no didOpen).
	buf.Reset()
	if err := refreshBuffer(c, uri, "go", "edited"); err != nil {
		t.Fatal(err)
	}
	msgs = parseSentMessages(t, buf.Bytes())
	if len(msgs) != 2 {
		t.Fatalf("second call: got %d messages, want 2 (didChange + didSave)", len(msgs))
	}
	if msgs[0]["method"] != "textDocument/didChange" {
		t.Errorf("msg[0]: got %v, want didChange", msgs[0]["method"])
	}
	if msgs[1]["method"] != "textDocument/didSave" {
		t.Errorf("msg[1]: got %v, want didSave", msgs[1]["method"])
	}
}
