// SPDX-License-Identifier: AGPL-3.0-or-later

// wire_protocol_test.go freezes the external bus-event wire protocol.
//
// The PascalCase strings produced by eventTypeString / Event.WireForm
// are consumed outside this process by:
//   - on_event hooks (internal/hooks — JSON {type, payload} on stdin,
//     filtered by the user's on_events allowlist of these exact strings;
//     see docs/content/config/reference.md, [hooks] section),
//   - daemon-socket subscribers (internal/daemon/server.go), and
//   - the Backend worker Channel (internal/backend/worker, host).
//
// docs/content/advanced/json-events.md documents the snake_case
// `enso run --format json` protocol (rendered by cmd/enso/run.go,
// renderJSONTo) whose names are the snake_case form of these wire
// strings. The table below pins both: renaming a wire string here
// breaks the eventTypeString assertion; renaming it in a way that no
// longer snake-cases to the documented name breaks the doc-pin
// assertion. Adding a new EventType constant without extending this
// table breaks TestEventTypeEnumExhaustive (which parses bus.go's
// const block), so new events can't silently ship an unfrozen string.
package bus

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"
)

// wireProtocolTable pins EVERY EventType constant, in declaration
// (iota) order, to its frozen wire strings and serializability.
//
//   - typeString: eventTypeString's result, and WireForm's typ when
//     wireSafe. These strings are the external protocol — FROZEN.
//   - wireSafe: WireForm returns ok=true (event crosses process
//     boundaries: daemon socket, worker Channel, on_event hooks).
//   - fromWire: FromWire(typeString, …) reconstructs the event.
//     PermissionRequest is asymmetric on purpose (WireForm yes,
//     FromWire no — see the comment in bus.go).
//   - docType: the snake_case name frozen in
//     docs/content/advanced/json-events.md for the `--format json`
//     stream ("" = not documented there). Where set, it must equal
//     snakeCase(typeString) so doc and code can't drift apart.
//     Note the doc's session_start / session_end /
//     permission_auto_deny lines are run.go-synthesized records, not
//     bus events, so they don't appear here.
var wireProtocolTable = []struct {
	et         EventType
	name       string // Go constant name in bus.go's const block
	typeString string // eventTypeString(et) — FROZEN external string
	wireSafe   bool   // WireForm ok
	fromWire   bool   // FromWire(typeString) reconstructs et
	docType    string // snake_case name in json-events.md, if documented
}{
	{EventUserMessage, "EventUserMessage", "UserMessage", true, true, "user_message"},
	{EventAssistantDelta, "EventAssistantDelta", "AssistantDelta", true, true, "assistant_delta"},
	{EventReasoningDelta, "EventReasoningDelta", "ReasoningDelta", true, true, "reasoning_delta"},
	{EventAssistantDone, "EventAssistantDone", "AssistantDone", true, true, "assistant_done"},
	{EventError, "EventError", "Error", true, true, "error"},
	{EventCancelled, "EventCancelled", "Cancelled", true, true, "cancelled"},
	{EventToolCallStart, "EventToolCallStart", "ToolCallStart", true, true, "tool_call_start"},
	// ToolCallProgress crosses the wire but is not rendered by
	// --format json and is absent from json-events.md.
	{EventToolCallProgress, "EventToolCallProgress", "ToolCallProgress", true, true, ""},
	{EventToolCallEnd, "EventToolCallEnd", "ToolCallEnd", true, true, "tool_call_end"},
	// PermissionRequest: WireForm emits it (channel/deadline are
	// json:"-" on the payload) but FromWire rejects it — in-process
	// consumers use the dedicated permission path. json-events.md
	// explicitly documents it as not emitted.
	{EventPermissionRequest, "EventPermissionRequest", "PermissionRequest", true, false, ""},
	{EventPermissionResponse, "EventPermissionResponse", "PermissionResponse", false, false, ""},
	// PermissionAuto is host-local; the doc's permission_auto_deny is
	// a separate run.go-synthesized record, NOT this event's wire form.
	{EventPermissionAuto, "EventPermissionAuto", "PermissionAuto", false, false, ""},
	{EventAgentStart, "EventAgentStart", "AgentStart", true, true, "agent_start"},
	{EventAgentEnd, "EventAgentEnd", "AgentEnd", true, true, "agent_end"},
	{EventCompacted, "EventCompacted", "Compacted", true, true, "compacted"},
	// AgentIdle and InputDiscarded cross the wire (worker Channel,
	// daemon) but are absent from json-events.md and --format json.
	{EventAgentIdle, "EventAgentIdle", "AgentIdle", true, true, ""},
	{EventInputDiscarded, "EventInputDiscarded", "InputDiscarded", true, true, ""},
	// EgressRequest is HOST-LOCAL (live Respond channel) — named in
	// eventTypeString for slow-consumer logs, never wire-safe.
	{EventEgressRequest, "EventEgressRequest", "EgressRequest", false, false, ""},
	// KNOWN GAP, pinned deliberately: EventNotice has no
	// eventTypeString case, so a slow-consumer drop of a Notice logs
	// as "Unknown". It is host-local by design (never crosses the
	// wire), so this is a diagnostics blemish, not a protocol hole.
	// If bus.go gains a "Notice" case, update this row.
	{EventNotice, "EventNotice", "Unknown", false, false, ""},
}

// TestEventTypeStringFrozen pins eventTypeString for every constant
// and rejects accidental string collisions.
func TestEventTypeStringFrozen(t *testing.T) {
	seen := make(map[string]string, len(wireProtocolTable))
	for _, tc := range wireProtocolTable {
		got := eventTypeString(tc.et)
		if got != tc.typeString {
			t.Errorf("%s: eventTypeString = %q, frozen protocol says %q — renaming a wire "+
				"string breaks every on_event hook and daemon subscriber", tc.name, got, tc.typeString)
		}
		if got == "Unknown" {
			continue // collision check is meaningless for the default string
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%s: wire string %q already used by %s", tc.name, got, prev)
		}
		seen[got] = tc.name
	}
}

// TestEventTypeStringNoUnknown future-proofs new events: exactly the
// pinned set of constants may map to the "Unknown" default. A new
// EventType added without an eventTypeString case lands here.
func TestEventTypeStringNoUnknown(t *testing.T) {
	// EventNotice is the one known gap (see table comment). Anything
	// else mapping to Unknown is a regression.
	knownUnknown := map[EventType]bool{EventNotice: true}
	for _, tc := range wireProtocolTable {
		got := eventTypeString(tc.et)
		if got == "Unknown" && !knownUnknown[tc.et] {
			t.Errorf("%s maps to the default %q string — add an eventTypeString case "+
				"and freeze it in wireProtocolTable", tc.name, got)
		}
		if got != "Unknown" && knownUnknown[tc.et] {
			t.Errorf("%s no longer maps to %q — bus.go gained a case for it; "+
				"update wireProtocolTable and the known-gap list", tc.name, "Unknown")
		}
	}
}

// TestEventTypeEnumExhaustive parses bus.go's EventType const block
// and asserts wireProtocolTable covers exactly those constants, in
// declaration order with contiguous iota values. Adding, removing, or
// reordering an event type fails here until the frozen table (and the
// docs) are deliberately updated.
func TestEventTypeEnumExhaustive(t *testing.T) {
	names := eventTypeConstNames(t)
	if len(names) != len(wireProtocolTable) {
		t.Fatalf("bus.go declares %d EventType constants, wireProtocolTable pins %d:\n"+
			"declared: %v\nnew/renamed event types must be added to the frozen table "+
			"(and docs/content/advanced/json-events.md if --format json renders them)",
			len(names), len(wireProtocolTable), names)
	}
	for i, tc := range wireProtocolTable {
		if names[i] != tc.name {
			t.Errorf("const #%d: bus.go declares %s, table pins %s (declaration order matters: iota)",
				i, names[i], tc.name)
		}
		if int(tc.et) != i {
			t.Errorf("%s: value %d, want %d — EventType values must stay contiguous from 0",
				tc.name, int(tc.et), i)
		}
	}
}

// TestWireFormMatchesTypeString asserts WireForm's serializability and
// typ string agree with the frozen table for every event type, and
// that the two string sources (WireForm switch, eventTypeString
// switch) cannot drift apart.
func TestWireFormMatchesTypeString(t *testing.T) {
	for _, tc := range wireProtocolTable {
		typ, payload, ok := Event{Type: tc.et}.WireForm()
		if ok != tc.wireSafe {
			t.Errorf("%s: WireForm ok = %v, want %v", tc.name, ok, tc.wireSafe)
			continue
		}
		if !ok {
			continue
		}
		if typ != tc.typeString {
			t.Errorf("%s: WireForm typ = %q, eventTypeString/frozen = %q — the two "+
				"switches in bus.go drifted", tc.name, typ, tc.typeString)
		}
		if string(payload) != "null" {
			t.Errorf("%s: nil payload marshaled as %s, want null", tc.name, payload)
		}
	}
}

// TestFromWireRecognizesFrozenStrings asserts FromWire reconstructs
// exactly the event types the table says it does, with the right Type,
// and rejects everything else (unknown and snake_case strings).
func TestFromWireRecognizesFrozenStrings(t *testing.T) {
	for _, tc := range wireProtocolTable {
		ev, ok := FromWire(tc.typeString, json.RawMessage(`null`))
		if ok != tc.fromWire {
			t.Errorf("%s: FromWire(%q) ok = %v, want %v", tc.name, tc.typeString, ok, tc.fromWire)
			continue
		}
		if ok && ev.Type != tc.et {
			t.Errorf("%s: FromWire(%q) Type = %d, want %d", tc.name, tc.typeString, ev.Type, tc.et)
		}
	}

	// Strings outside the protocol must be skipped, including the
	// snake_case --format json names (different layer, never valid on
	// the bus wire) and the eventTypeString default.
	for _, bogus := range []string{
		"", "Unknown", "SessionStart", "session_start", "user_message",
		"tool_call_start", "assistant_delta", "Notice", "EgressRequest",
		"PermissionResponse", "PermissionAuto",
	} {
		if _, ok := FromWire(bogus, json.RawMessage(`null`)); ok {
			t.Errorf("FromWire(%q) = ok, want skip", bogus)
		}
	}
}

// TestWireStringsPinnedToJSONEventsDoc ties the wire strings to the
// names frozen in docs/content/advanced/json-events.md: every
// documented snake_case name must be exactly the snake_case form of
// the corresponding wire string. Renaming a wire string without
// updating the doc (or vice versa) fails here.
func TestWireStringsPinnedToJSONEventsDoc(t *testing.T) {
	for _, tc := range wireProtocolTable {
		if tc.docType == "" {
			continue
		}
		if got := snakeCase(tc.typeString); got != tc.docType {
			t.Errorf("%s: wire string %q snake-cases to %q but json-events.md documents %q — "+
				"doc and code drifted; fix deliberately on both sides", tc.name, tc.typeString, got, tc.docType)
		}
	}
}

// TestWireRoundTripPayloads round-trips representative payloads for
// the major event categories through WireForm/FromWire, asserting the
// JSON wire shape and the reconstructed payload's concrete type
// (string for text events, error for Error, int for InputDiscarded,
// generic decoded JSON for structured events, nil for markers).
func TestWireRoundTripPayloads(t *testing.T) {
	cases := []struct {
		name     string
		evt      Event
		wantType string
		wantJSON string // canonical wire payload (compared structurally)
		wantBack any    // expected FromWire payload (nil for markers)
		wantErr  string // non-empty: payload must be an error with this message
	}{
		{
			name:     "user message",
			evt:      Event{Type: EventUserMessage, Payload: "run the tests"},
			wantType: "UserMessage",
			wantJSON: `"run the tests"`,
			wantBack: "run the tests",
		},
		{
			name:     "assistant delta with newline",
			evt:      Event{Type: EventAssistantDelta, Payload: "line one\nline two"},
			wantType: "AssistantDelta",
			wantJSON: `"line one\nline two"`,
			wantBack: "line one\nline two",
		},
		{
			name:     "assistant done marker",
			evt:      Event{Type: EventAssistantDone},
			wantType: "AssistantDone",
			wantJSON: `null`,
			wantBack: nil,
		},
		{
			name:     "cancelled marker",
			evt:      Event{Type: EventCancelled},
			wantType: "Cancelled",
			wantJSON: `null`,
			wantBack: nil,
		},
		{
			name:     "agent idle marker",
			evt:      Event{Type: EventAgentIdle},
			wantType: "AgentIdle",
			wantJSON: `null`,
			wantBack: nil,
		},
		{
			name: "tool call start",
			evt: Event{Type: EventToolCallStart, Payload: map[string]any{
				"id": "call_1", "name": "bash", "args": map[string]any{"command": "ls"},
			}},
			wantType: "ToolCallStart",
			wantJSON: `{"id":"call_1","name":"bash","args":{"command":"ls"}}`,
			wantBack: map[string]any{
				"id": "call_1", "name": "bash", "args": map[string]any{"command": "ls"},
			},
		},
		{
			name: "tool call progress",
			evt: Event{Type: EventToolCallProgress, Payload: map[string]any{
				"id": "call_1", "output": "partial chunk",
			}},
			wantType: "ToolCallProgress",
			wantJSON: `{"id":"call_1","output":"partial chunk"}`,
			wantBack: map[string]any{"id": "call_1", "output": "partial chunk"},
		},
		{
			name: "tool call end with error coerced to string",
			evt: Event{Type: EventToolCallEnd, Payload: map[string]any{
				"id": "call_1", "name": "bash", "result": "", "error": errors.New("exit status 1"),
			}},
			wantType: "ToolCallEnd",
			wantJSON: `{"id":"call_1","name":"bash","result":"","error":"exit status 1"}`,
			wantBack: map[string]any{
				"id": "call_1", "name": "bash", "result": "", "error": "exit status 1",
			},
		},
		{
			name: "tool call end success with null error",
			evt: Event{Type: EventToolCallEnd, Payload: map[string]any{
				"id": "call_2", "name": "glob", "result": "a.go\nb.go", "error": nil,
			}},
			wantType: "ToolCallEnd",
			wantJSON: `{"id":"call_2","name":"glob","result":"a.go\nb.go","error":null}`,
			wantBack: map[string]any{
				"id": "call_2", "name": "glob", "result": "a.go\nb.go", "error": nil,
			},
		},
		{
			name: "compacted",
			evt: Event{Type: EventCompacted, Payload: map[string]any{
				"reason": "token budget", "summary": "explored bus package",
			}},
			wantType: "Compacted",
			wantJSON: `{"reason":"token budget","summary":"explored bus package"}`,
			wantBack: map[string]any{"reason": "token budget", "summary": "explored bus package"},
		},
		{
			name: "agent start with numeric depth",
			evt: Event{Type: EventAgentStart, Payload: map[string]any{
				"id": "a1", "parent_id": "root", "depth": 1, "prompt": "review bus.go",
			}},
			wantType: "AgentStart",
			wantJSON: `{"id":"a1","parent_id":"root","depth":1,"prompt":"review bus.go"}`,
			// JSON numbers come back as float64 — consumers of the
			// generic payloads must not assert int.
			wantBack: map[string]any{
				"id": "a1", "parent_id": "root", "depth": float64(1), "prompt": "review bus.go",
			},
		},
		{
			name: "agent end",
			evt: Event{Type: EventAgentEnd, Payload: map[string]any{
				"id": "a1", "parent_id": "root", "error": "",
			}},
			wantType: "AgentEnd",
			wantJSON: `{"id":"a1","parent_id":"root","error":""}`,
			wantBack: map[string]any{"id": "a1", "parent_id": "root", "error": ""},
		},
		{
			name:     "error payload reconstructed as error",
			evt:      Event{Type: EventError, Payload: errors.New("agent loop: context deadline exceeded")},
			wantType: "Error",
			wantJSON: `"agent loop: context deadline exceeded"`,
			wantErr:  "agent loop: context deadline exceeded",
		},
		{
			name:     "input discarded count as int",
			evt:      Event{Type: EventInputDiscarded, Payload: 3},
			wantType: "InputDiscarded",
			wantJSON: `3`,
			wantBack: 3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typ, payload, ok := tc.evt.WireForm()
			if !ok {
				t.Fatalf("WireForm: ok=false for serializable event")
			}
			if typ != tc.wantType {
				t.Fatalf("WireForm typ = %q, want %q", typ, tc.wantType)
			}
			assertJSONEqual(t, payload, []byte(tc.wantJSON))

			back, ok := FromWire(typ, payload)
			if !ok {
				t.Fatalf("FromWire(%q): ok=false", typ)
			}
			if back.Type != tc.evt.Type {
				t.Fatalf("FromWire Type = %d, want %d", back.Type, tc.evt.Type)
			}
			if tc.wantErr != "" {
				err, isErr := back.Payload.(error)
				if !isErr {
					t.Fatalf("FromWire payload = %T, want error", back.Payload)
				}
				if err.Error() != tc.wantErr {
					t.Fatalf("FromWire error = %q, want %q", err.Error(), tc.wantErr)
				}
				return
			}
			if !reflect.DeepEqual(back.Payload, tc.wantBack) {
				t.Fatalf("FromWire payload = %#v (%T), want %#v (%T)",
					back.Payload, back.Payload, tc.wantBack, tc.wantBack)
			}
		})
	}
}

// assertJSONEqual compares two JSON documents structurally (key order
// in objects is not significant on the wire).
func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var gv, wv any
	if err := json.Unmarshal(got, &gv); err != nil {
		t.Fatalf("wire payload is not valid JSON: %v (payload: %s)", err, got)
	}
	if err := json.Unmarshal(want, &wv); err != nil {
		t.Fatalf("test expectation is not valid JSON: %v (%s)", err, want)
	}
	if !reflect.DeepEqual(gv, wv) {
		t.Fatalf("wire payload = %s, want %s", got, want)
	}
}

// eventTypeConstNames parses bus.go and returns the names of the
// EventType const block in declaration order. Source-level so the
// exhaustiveness check can't be fooled — Go offers no runtime
// enumeration of constants.
func eventTypeConstNames(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "bus.go", nil, 0)
	if err != nil {
		t.Fatalf("parse bus.go: %v", err)
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		var names []string
		inBlock := false
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "EventType" {
				inBlock = true
			}
			if !inBlock {
				continue
			}
			for _, n := range vs.Names {
				if n.Name != "_" {
					names = append(names, n.Name)
				}
			}
		}
		if inBlock {
			return names
		}
	}
	t.Fatal("no EventType const block found in bus.go")
	return nil
}

// snakeCase converts a PascalCase wire string to the snake_case form
// used by json-events.md ("ToolCallStart" → "tool_call_start").
func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
