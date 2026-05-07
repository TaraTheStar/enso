// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
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
	ParseSSE(strings.NewReader(stream), ch)

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
	ParseSSE(strings.NewReader(stream), ch)

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
	ParseSSE(strings.NewReader(stream), ch)

	got := <-ch
	if string(got) != `{"hello":1}` {
		t.Fatalf("got %q", got)
	}
}
