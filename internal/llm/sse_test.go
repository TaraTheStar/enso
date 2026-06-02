// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"io"
	"strings"
	"testing"
)

func TestParseSSE_BasicEventsAndDone(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"a":1}`,
		``,
		`: comment line`,
		`data: {"a":2}`,
		``,
		`data: [DONE]`,
		`data: {"a":3}`, // must be ignored after [DONE]
	}, "\n")

	ch := make(chan []byte, 8)
	ParseSSE(strings.NewReader(stream), ch, nil)

	var got []string
	for b := range ch {
		got = append(got, string(b))
	}
	if len(got) != 2 || got[0] != `{"a":1}` || got[1] != `{"a":2}` {
		t.Fatalf("got %v, want [{a:1}, {a:2}]", got)
	}
}

func TestParseSSE_EmptyAndCommentLinesSkipped(t *testing.T) {
	stream := "\n\n: heartbeat\n\ndata: {\"x\":true}\n\n"
	ch := make(chan []byte, 4)
	ParseSSE(strings.NewReader(stream), ch, nil)

	got := <-ch
	if string(got) != `{"x":true}` {
		t.Fatalf("got %q", got)
	}
	if _, ok := <-ch; ok {
		t.Fatalf("channel should be closed at EOF")
	}
}

func TestParseSSE_NonDataLinesIgnored(t *testing.T) {
	// Lines like `event: foo` or `id: 123` are not currently handled and must
	// simply be ignored (not break parsing of subsequent data lines).
	stream := "event: ping\nid: 17\ndata: {\"hello\":1}\n\n"
	ch := make(chan []byte, 4)
	ParseSSE(strings.NewReader(stream), ch, nil)

	got := <-ch
	if string(got) != `{"hello":1}` {
		t.Fatalf("got %q", got)
	}
}

// errReader returns some data then a non-EOF error, simulating a truncated
// stream (dropped connection mid-response).
type errReader struct {
	data []byte
	err  error
	done bool
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	n := copy(p, r.data)
	return n, nil
}

// TestParseSSE_ReadErrorSurfaced guards H1: a read error mid-stream must be
// reported via errOut, not swallowed into a clean channel close (which the
// caller would treat as a normal finish).
func TestParseSSE_ReadErrorSurfaced(t *testing.T) {
	r := &errReader{data: []byte("data: {\"a\":1}\n"), err: io.ErrUnexpectedEOF}
	ch := make(chan []byte, 4)
	var sseErr error
	ParseSSE(r, ch, &sseErr)

	var got []string
	for b := range ch {
		got = append(got, string(b))
	}
	if len(got) != 1 || got[0] != `{"a":1}` {
		t.Fatalf("payloads = %v, want [{a:1}]", got)
	}
	if sseErr == nil {
		t.Fatal("read error must be surfaced via errOut, got nil")
	}
}

// TestParseSSE_CleanFinishNoError confirms EOF / [DONE] leave errOut unset.
func TestParseSSE_CleanFinishNoError(t *testing.T) {
	ch := make(chan []byte, 4)
	var sseErr error
	ParseSSE(strings.NewReader("data: {\"a\":1}\n\ndata: [DONE]\n"), ch, &sseErr)
	for range ch {
	}
	if sseErr != nil {
		t.Fatalf("clean finish should leave errOut nil, got %v", sseErr)
	}
}
