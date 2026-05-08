// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
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
