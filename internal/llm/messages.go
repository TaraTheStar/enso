// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import "encoding/json"

// Message represents a single message in the conversation.
//
// The on-the-wire shape has one quirk worth knowing: the OpenAI spec
// requires non-assistant messages (user, system, tool, developer) to carry
// a `content` field even when empty, while it allows assistant messages
// to omit `content` when `tool_calls` is present. Some OpenAI-compatible
// servers (vLLM, some llama.cpp configs) enforce this strictly and 400
// requests that drop `content` from a tool result. See MarshalJSON.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"-"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// MarshalJSON emits `content` unconditionally for non-assistant roles
// (even when empty) and omits it on assistant messages that carry
// tool_calls without text. The Go field is `json:"-"` so the default
// encoder ignores it; this method is the single source of truth.
func (m Message) MarshalJSON() ([]byte, error) {
	type alias struct {
		Role       string     `json:"role"`
		Content    *string    `json:"content,omitempty"`
		ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
		ToolCallID string     `json:"tool_call_id,omitempty"`
		Name       string     `json:"name,omitempty"`
	}
	a := alias{
		Role:       m.Role,
		ToolCalls:  m.ToolCalls,
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
	}
	if m.Role == "assistant" && m.Content == "" && len(m.ToolCalls) > 0 {
		// Assistant message that delegates entirely to tool calls:
		// omit content per the spec.
	} else {
		c := m.Content
		a.Content = &c
	}
	return json.Marshal(a)
}

// UnmarshalJSON keeps the symmetric shape so a session re-loaded from
// the store comes back with Content populated as expected.
func (m *Message) UnmarshalJSON(data []byte) error {
	type alias struct {
		Role       string     `json:"role"`
		Content    string     `json:"content"`
		ToolCalls  []ToolCall `json:"tool_calls"`
		ToolCallID string     `json:"tool_call_id"`
		Name       string     `json:"name"`
	}
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	m.Role = a.Role
	m.Content = a.Content
	m.ToolCalls = a.ToolCalls
	m.ToolCallID = a.ToolCallID
	m.Name = a.Name
	return nil
}

// ToolCall represents a single tool invocation request from the model.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ChatRequest is the OpenAI-compatible chat completion request body.
type ChatRequest struct {
	Model           string    `json:"model"`
	Messages        []Message `json:"messages"`
	Stream          bool      `json:"stream"`
	Tools           []ToolDef `json:"tools,omitempty"`
	Temperature     float64   `json:"temperature,omitempty"`
	TopK            int       `json:"top_k,omitempty"`
	TopP            float64   `json:"top_p,omitempty"`
	MinP            float64   `json:"min_p,omitempty"`
	PresencePenalty float64   `json:"presence_penalty,omitempty"`
}

// ToolDef is the JSON Schema definition sent to the model.
type ToolDef struct {
	Type     string          `json:"type"`
	Function ToolFunctionDef `json:"function"`
}

// ToolFunctionDef describes a tool's name, description, and parameters.
type ToolFunctionDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// ChatResponseDelta is the per-choice `delta` object inside a streaming
// chat-completion chunk.
//
// `ReasoningContent` is the Qwen3 / DeepSeek-R1 / llama.cpp `--reasoning-budget`
// channel that carries chain-of-thought text separately from the visible
// answer. We surface it as a tagged TextDelta so the user can see thinking
// progress, but it is NOT appended to the assistant message in history (the
// model is supposed to re-derive reasoning each turn).
type ChatResponseDelta struct {
	Role             string   `json:"role,omitempty"`
	Content          string   `json:"content,omitempty"`
	ReasoningContent string   `json:"reasoning_content,omitempty"`
	ToolCalls        []DTCall `json:"tool_calls,omitempty"`
}

// ChatCompletionChunk is the OpenAI-compatible SSE chunk envelope. Servers
// (llama.cpp, vLLM, OpenAI itself) emit one per token group.
type ChatCompletionChunk struct {
	Choices []ChunkChoice `json:"choices"`
}

// ChunkChoice carries the streaming delta for one choice index.
type ChunkChoice struct {
	Index        int               `json:"index"`
	Delta        ChatResponseDelta `json:"delta"`
	FinishReason string            `json:"finish_reason,omitempty"`
}

// DTCall is a tool-call delta within a streamed chunk.
type DTCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string      `json:"name,omitempty"`
		Arguments interface{} `json:"arguments,omitempty"` // string or JSON object (llama.cpp quirk)
	} `json:"function,omitempty"`
}
