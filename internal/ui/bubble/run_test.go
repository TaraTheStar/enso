// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/session"
)

// TestCoalesceDeltas pins forwardBus's per-flush merge: runs of
// CONSECUTIVE same-type streaming deltas collapse into one event with
// the concatenated payload, while non-delta events interleaved between
// runs keep their position (adjacent-only merging, never reordering).
func TestCoalesceDeltas(t *testing.T) {
	ad := func(s string) bus.Event { return bus.Event{Type: bus.EventAssistantDelta, Payload: s} }
	rd := func(s string) bus.Event { return bus.Event{Type: bus.EventReasoningDelta, Payload: s} }
	toolStart := bus.Event{Type: bus.EventToolCallStart, Payload: map[string]any{"id": "t1", "name": "bash"}}
	done := bus.Event{Type: bus.EventAssistantDone}

	cases := []struct {
		name string
		in   []bus.Event
		want []bus.Event
	}{
		{
			name: "empty",
			in:   nil,
			want: nil,
		},
		{
			name: "consecutive assistant deltas merge",
			in:   []bus.Event{ad("he"), ad("llo"), ad(" world")},
			want: []bus.Event{ad("hello world")},
		},
		{
			name: "consecutive reasoning deltas merge",
			in:   []bus.Event{rd("hm"), rd("m...")},
			want: []bus.Event{rd("hmm...")},
		},
		{
			name: "non-delta between runs preserves order",
			in:   []bus.Event{ad("a"), ad("b"), toolStart, ad("c"), ad("d"), done},
			want: []bus.Event{ad("ab"), toolStart, ad("cd"), done},
		},
		{
			name: "different delta types never merge across each other",
			in:   []bus.Event{rd("think"), rd("ing"), ad("an"), ad("swer"), rd("more")},
			want: []bus.Event{rd("thinking"), ad("answer"), rd("more")},
		},
		{
			name: "single delta passes through unchanged",
			in:   []bus.Event{ad("solo")},
			want: []bus.Event{ad("solo")},
		},
		{
			name: "non-delta only passes through",
			in:   []bus.Event{toolStart, done},
			want: []bus.Event{toolStart, done},
		},
		{
			name: "non-string delta payload is not merged",
			in:   []bus.Event{ad("a"), {Type: bus.EventAssistantDelta, Payload: 42}, ad("b")},
			want: []bus.Event{ad("a"), {Type: bus.EventAssistantDelta, Payload: 42}, ad("b")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := append([]bus.Event(nil), tc.in...)
			got := coalesceDeltas(in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d events, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i].Type != tc.want[i].Type || !reflect.DeepEqual(got[i].Payload, tc.want[i].Payload) {
					t.Errorf("event %d: got {%v %v}, want {%v %v}",
						i, got[i].Type, got[i].Payload, tc.want[i].Type, tc.want[i].Payload)
				}
			}
		})
	}
}

// TestForwardBusFuncMergesPerFlush drives the actual forward loop: a
// pre-filled, closed subscription drains in one flush, so the program
// receives the merged events in order — one Update per delta run, not
// one per token.
func TestForwardBusFuncMergesPerFlush(t *testing.T) {
	sub := make(chan bus.Event, 16)
	sub <- bus.Event{Type: bus.EventReasoningDelta, Payload: "th"}
	sub <- bus.Event{Type: bus.EventReasoningDelta, Payload: "ink"}
	sub <- bus.Event{Type: bus.EventAssistantDelta, Payload: "hel"}
	sub <- bus.Event{Type: bus.EventAssistantDelta, Payload: "lo"}
	sub <- bus.Event{Type: bus.EventToolCallStart, Payload: map[string]any{"id": "t1"}}
	sub <- bus.Event{Type: bus.EventAssistantDelta, Payload: "bye"}
	close(sub)

	var got []bus.Event
	forwardBusFunc(func(ev bus.Event) { got = append(got, ev) }, sub)

	want := []bus.Event{
		{Type: bus.EventReasoningDelta, Payload: "think"},
		{Type: bus.EventAssistantDelta, Payload: "hello"},
		{Type: bus.EventToolCallStart, Payload: map[string]any{"id": "t1"}},
		{Type: bus.EventAssistantDelta, Payload: "bye"},
	}
	if len(got) != len(want) {
		t.Fatalf("sent %d events, want %d: %+v", len(got), len(want), got)
	}
	for i := range got {
		if got[i].Type != want[i].Type || !reflect.DeepEqual(got[i].Payload, want[i].Payload) {
			t.Errorf("event %d: got {%v %v}, want {%v %v}",
				i, got[i].Type, got[i].Payload, want[i].Type, want[i].Payload)
		}
	}
}

// TestAppendAuditEventTruncatesToolCallEnd verifies the audit path caps
// the bulky result/display strings of a ToolCallEnd before persisting
// (the full output already lives in the messages and tool_calls
// tables), and — critically — that it does so on a COPY: the shared bus
// payload other subscribers (the renderer) receive must stay intact.
func TestAppendAuditEventTruncatesToolCallEnd(t *testing.T) {
	store, err := session.OpenAt(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	w, err := session.NewSession(store, "model-x", "prov-y", "/some/cwd")
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	bigResult := strings.Repeat("r", 10*1024)
	bigDisplay := strings.Repeat("d", 4*1024)
	payload := map[string]any{
		"id":      "call_1",
		"name":    "bash",
		"result":  bigResult,
		"display": bigDisplay,
		"error":   nil,
	}
	evt := bus.Event{Type: bus.EventToolCallEnd, Payload: payload}

	appendAuditEvent(w, evt)

	// The shared event payload is untouched — the renderer still sees
	// the full strings.
	if payload["result"].(string) != bigResult {
		t.Errorf("shared payload result mutated: len=%d, want %d", len(payload["result"].(string)), len(bigResult))
	}
	if payload["display"].(string) != bigDisplay {
		t.Errorf("shared payload display mutated: len=%d, want %d", len(payload["display"].(string)), len(bigDisplay))
	}

	// The persisted audit row is truncated and marked.
	var raw []byte
	if err := store.DB.QueryRow(
		`SELECT payload FROM events WHERE type = 'ToolCallEnd'`,
	).Scan(&raw); err != nil {
		t.Fatalf("query event: %v", err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("unmarshal persisted payload: %v", err)
	}
	for field, total := range map[string]int{"result": len(bigResult), "display": len(bigDisplay)} {
		s, _ := persisted[field].(string)
		marker := fmt.Sprintf("...[truncated, %d bytes total]", total)
		if !strings.HasSuffix(s, marker) {
			t.Errorf("%s: missing truncation marker %q (got tail %q)", field, marker, s[max(0, len(s)-48):])
		}
		if len(s) > auditToolOutputCap+len(marker) {
			t.Errorf("%s: persisted %d bytes, want <= %d", field, len(s), auditToolOutputCap+len(marker))
		}
	}
	// Non-bulky fields survive.
	if persisted["id"] != "call_1" || persisted["name"] != "bash" {
		t.Errorf("id/name fields lost: %+v", persisted)
	}
}

// TestTruncateAuditedToolEnd_ShortAndNonMap pins the no-op paths: short
// strings and non-map payloads pass through unchanged, and a multi-byte
// rune straddling the cap is never split.
func TestTruncateAuditedToolEnd_ShortAndNonMap(t *testing.T) {
	m := map[string]any{"result": "short", "display": "tiny"}
	got := truncateAuditedToolEnd(m).(map[string]any)
	if got["result"] != "short" || got["display"] != "tiny" {
		t.Errorf("short strings changed: %+v", got)
	}
	if out := truncateAuditedToolEnd("denied"); out != "denied" {
		t.Errorf("non-map payload changed: %v", out)
	}

	// Place a 3-byte rune across the cap boundary; the cut must back
	// off to the rune start, keeping the prefix valid UTF-8.
	s := strings.Repeat("a", auditToolOutputCap-1) + "世" + strings.Repeat("b", 600)
	out := truncateAuditedToolEnd(map[string]any{"result": s}).(map[string]any)
	rs := out["result"].(string)
	prefix := strings.TrimSuffix(rs, fmt.Sprintf("...[truncated, %d bytes total]", len(s)))
	if prefix == rs {
		t.Fatalf("missing truncation marker: %q", rs[max(0, len(rs)-48):])
	}
	if prefix != strings.Repeat("a", auditToolOutputCap-1) {
		t.Errorf("rune split or wrong cut: prefix len=%d, tail=%q", len(prefix), prefix[max(0, len(prefix)-8):])
	}
}
