// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// EventType identifies the kind of bus event.
type EventType int

const (
	EventUserMessage EventType = iota
	EventAssistantDelta
	EventReasoningDelta
	EventAssistantDone
	EventError
	EventCancelled
	EventToolCallStart
	EventToolCallProgress
	EventToolCallEnd
	EventPermissionRequest
	EventPermissionResponse
	EventPermissionAuto
	EventAgentStart
	EventAgentEnd
	EventCompacted
	// EventAgentIdle fires when the agent's runUntilQuiescent loop returns —
	// i.e., the entire user-message → reply (including any tool-call rounds)
	// pipeline is done. EventAssistantDone fires per LLM completion, which
	// includes intermediate completions whose only output is a tool call;
	// the TUI must not treat those as "agent idle" or Ctrl-C between turns
	// silently no-ops while the agent is mid-loop.
	EventAgentIdle

	// EventInputDiscarded fires when the agent drains user-message
	// submissions that piled up in the input channel during a turn the
	// user then cancelled. Payload is the int count of discarded
	// messages. Without this drain, queued submits would land as the
	// next turn out of order — the UI shows the count so the user
	// knows their followups didn't make it through.
	EventInputDiscarded
)

// Event is a typed message sent through the bus.
type Event struct {
	Type    EventType
	Payload any
}

// WireForm returns the stable wire type-string and a JSON-safe payload
// for this event, or ok=false for internal/unserializable events that
// must not cross a process boundary as a plain event (e.g.
// PermissionRequest, whose payload carries a live response channel and
// is proxied separately; the Permission* feedback events, which are
// handled host-side). ReasoningDelta DOES cross: once enso run / the
// TUI run behind the Backend seam, the worker's agent is the only
// reasoning source, so dropping it here would silently regress live
// reasoning. Carrying it is additive for the daemon path too (attached
// clients now also see reasoning).
//
// This is the single source of truth for bus-event serialization,
// shared by every transport that carries bus events across a process
// boundary (the daemon socket and the Backend worker Channel). Keeping
// one mapping is the point of the single-execution-path design: host
// and worker cannot drift on what an event looks like on the wire.
func (e Event) WireForm() (typ string, payload json.RawMessage, ok bool) {
	switch e.Type {
	case EventUserMessage:
		typ = "UserMessage"
	case EventAssistantDelta:
		typ = "AssistantDelta"
	case EventReasoningDelta:
		typ = "ReasoningDelta"
	case EventAssistantDone:
		typ = "AssistantDone"
	case EventError:
		typ = "Error"
	case EventCancelled:
		typ = "Cancelled"
	case EventToolCallStart:
		typ = "ToolCallStart"
	case EventToolCallProgress:
		typ = "ToolCallProgress"
	case EventToolCallEnd:
		typ = "ToolCallEnd"
	case EventAgentStart:
		typ = "AgentStart"
	case EventAgentEnd:
		typ = "AgentEnd"
	case EventCompacted:
		typ = "Compacted"
	case EventAgentIdle:
		typ = "AgentIdle"
	case EventInputDiscarded:
		typ = "InputDiscarded"
	default:
		return "", nil, false
	}
	b, err := json.Marshal(simplifyPayload(e.Payload))
	if err != nil {
		b = []byte(`null`)
	}
	return typ, b, true
}

// simplifyPayload coerces non-JSON-serializable payloads (errors,
// channels) to safe representations before marshaling.
func simplifyPayload(p any) any {
	switch v := p.(type) {
	case error:
		return v.Error()
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			out[k] = simplifyPayload(val)
		}
		return out
	case nil:
		return nil
	default:
		// Strings, ints, floats, bools all marshal fine.
		return v
	}
}

// FromWire is the inverse of WireForm: it reconstructs a bus.Event
// from the stable wire type-string and JSON payload. ok=false means
// the type is not reconstructable (unknown, or one that never crosses
// the wire) and the event should be skipped.
//
// Like WireForm, this is the single source of truth shared by every
// transport that carries bus events across a process boundary — the
// daemon attach path and the Backend worker Channel reconstruct events
// identically, so a local-backend run and an attached daemon session
// render the same. Payload concrete types match what the renderers
// assert (string for text events, error for Error, int for
// InputDiscarded, generic decoded JSON for the structured events).
func FromWire(typ string, payload json.RawMessage) (Event, bool) {
	str := func() string {
		var s string
		_ = json.Unmarshal(payload, &s)
		return s
	}
	generic := func() any {
		var a any
		if len(payload) > 0 {
			_ = json.Unmarshal(payload, &a)
		}
		return a
	}
	switch typ {
	case "UserMessage":
		return Event{Type: EventUserMessage, Payload: str()}, true
	case "AssistantDelta":
		return Event{Type: EventAssistantDelta, Payload: str()}, true
	case "ReasoningDelta":
		return Event{Type: EventReasoningDelta, Payload: str()}, true
	case "AssistantDone":
		return Event{Type: EventAssistantDone}, true
	case "Error":
		return Event{Type: EventError, Payload: fmt.Errorf("%s", str())}, true
	case "Cancelled":
		return Event{Type: EventCancelled}, true
	case "ToolCallStart":
		return Event{Type: EventToolCallStart, Payload: generic()}, true
	case "ToolCallProgress":
		return Event{Type: EventToolCallProgress, Payload: generic()}, true
	case "ToolCallEnd":
		return Event{Type: EventToolCallEnd, Payload: generic()}, true
	case "AgentStart":
		return Event{Type: EventAgentStart, Payload: generic()}, true
	case "AgentEnd":
		return Event{Type: EventAgentEnd, Payload: generic()}, true
	case "Compacted":
		return Event{Type: EventCompacted, Payload: generic()}, true
	case "AgentIdle":
		return Event{Type: EventAgentIdle}, true
	case "InputDiscarded":
		// JSON numbers decode as float64; renderers assert int.
		var n float64
		_ = json.Unmarshal(payload, &n)
		return Event{Type: EventInputDiscarded, Payload: int(n)}, true
	}
	return Event{}, false
}

// Bus is a channel-fan-out hub for agent events.
type Bus struct {
	subscribers []chan Event
}

// New creates a new event bus.
func New() *Bus {
	return &Bus{
		subscribers: make([]chan Event, 0),
	}
}

// Subscribe registers a buffered channel as a subscriber.
func (b *Bus) Subscribe(capacity int) chan Event {
	ch := make(chan Event, capacity)
	b.subscribers = append(b.subscribers, ch)
	return ch
}

// Publish sends an event to all subscribers non-blocking.
// Events are dropped (with a log) if a subscriber's buffer is full.
func (b *Bus) Publish(evt Event) {
	for _, ch := range b.subscribers {
		select {
		case ch <- evt:
		default:
			slog.Warn("bus: slow consumer, event dropped", slog.String("type", eventTypeString(evt.Type)))
		}
	}
}

// Close closes all subscriber channels.
func (b *Bus) Close() {
	for _, ch := range b.subscribers {
		close(ch)
	}
}

func eventTypeString(t EventType) string {
	switch t {
	case EventUserMessage:
		return "UserMessage"
	case EventAssistantDelta:
		return "AssistantDelta"
	case EventReasoningDelta:
		return "ReasoningDelta"
	case EventAssistantDone:
		return "AssistantDone"
	case EventError:
		return "Error"
	case EventCancelled:
		return "Cancelled"
	case EventToolCallStart:
		return "ToolCallStart"
	case EventToolCallProgress:
		return "ToolCallProgress"
	case EventToolCallEnd:
		return "ToolCallEnd"
	case EventPermissionRequest:
		return "PermissionRequest"
	case EventPermissionResponse:
		return "PermissionResponse"
	case EventPermissionAuto:
		return "PermissionAuto"
	case EventAgentStart:
		return "AgentStart"
	case EventAgentEnd:
		return "AgentEnd"
	case EventCompacted:
		return "Compacted"
	case EventAgentIdle:
		return "AgentIdle"
	case EventInputDiscarded:
		return "InputDiscarded"
	default:
		return "Unknown"
	}
}
