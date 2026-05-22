// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"encoding/base64"
	"encoding/json"
)

// Message represents a single message in the conversation.
//
// The on-the-wire shape has one quirk worth knowing: the OpenAI spec
// requires non-assistant messages (user, system, tool, developer) to carry
// a `content` field even when empty, while it allows assistant messages
// to omit `content` when `tool_calls` is present. Some OpenAI-compatible
// servers (vLLM, some llama.cpp configs) enforce this strictly and 400
// requests that drop `content` from a tool result. See MarshalJSON.
//
// Parts is the multimodal escape hatch. When non-empty it carries
// text + image + document parts; OpenAI MarshalJSON emits the multi-
// block content-array shape, and non-OpenAI adapters access Parts
// directly in their translators. Empty Parts (the common case) keeps
// the legacy string-Content path so existing flows are untouched.
//
// Persistence note: the session store schema is still TEXT-only, so a
// Message with Parts survives a process restart only via the
// downgraded `Content` summary. Adapters look at Parts first, fall
// back to Content when empty — that contract is what makes a
// degraded resume safe.
type Message struct {
	Role       string        `json:"role"`
	Content    string        `json:"-"`
	Parts      []MessagePart `json:"-"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Name       string        `json:"name,omitempty"`
}

// MessagePart is one element of a multi-block message body. Type
// names mirror the OpenAI / Anthropic vocabulary so a reader doesn't
// have to map between them. Exactly one of Text / Data / URI is
// meaningful per Type:
//
//   - Type="text"     → Text
//   - Type="image"    → Data + MIMEType, or URI
//   - Type="document" → Data + MIMEType, or URI  (PDF, mostly)
//
// Data carries the raw bytes; adapters base64-encode on the wire when
// the vendor requires it (OpenAI, Anthropic). Keeping the in-memory
// shape as []byte avoids the double-memory blow-up of caching both
// raw and encoded forms.
type MessagePart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	Data     []byte `json:"data,omitempty"`
	URI      string `json:"uri,omitempty"`
}

// Part-construction helpers — call sites read better than struct
// literals and the type-tag stays consistent without each caller
// remembering the spelling.

// NewTextPart returns a text MessagePart. Mostly useful when mixing
// text and images in a single message; pure-text messages should keep
// using Content.
func NewTextPart(text string) MessagePart {
	return MessagePart{Type: "text", Text: text}
}

// NewImagePart wraps inline image bytes. mime should be the IANA type
// the file ACTUALLY is (e.g. "image/png"), not the file extension —
// every adapter forwards this to the vendor.
func NewImagePart(mime string, data []byte) MessagePart {
	return MessagePart{Type: "image", MIMEType: mime, Data: data}
}

// NewImagePartURI references an image by URL (http(s):// or gs://).
// Not all adapters support URI inputs — Bedrock Converse needs bytes
// and will return an error if it gets a URI-only image. OpenAI and
// Vertex are URI-friendly; the Anthropic SDK is bytes-only.
func NewImagePartURI(uri string) MessagePart {
	return MessagePart{Type: "image", URI: uri}
}

// NewDocumentPart wraps inline document bytes (PDF, etc.). Anthropic
// and Vertex support these natively; OpenAI and Bedrock Converse will
// reject the message until those adapters add explicit support.
func NewDocumentPart(mime string, data []byte) MessagePart {
	return MessagePart{Type: "document", MIMEType: mime, Data: data}
}

// dataURL renders Data as a data: URL with the given MIME type. Used
// by adapters that pass images as URLs rather than separate bytes
// (notably OpenAI's image_url shape).
func (p MessagePart) dataURL() string {
	if len(p.Data) == 0 || p.MIMEType == "" {
		return ""
	}
	return "data:" + p.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(p.Data)
}

// MarshalJSON emits the OpenAI wire format. Two paths:
//
//   - Parts empty (the common case): emit `content` as a string,
//     unconditionally for non-assistant roles (the OpenAI spec
//     requires it; some compat servers 400 without it), and omit it
//     on assistant messages that delegate entirely to tool_calls.
//
//   - Parts non-empty: emit `content` as the OpenAI multimodal array
//     ([{type:"text",text:...},{type:"image_url",image_url:{url:...}}]).
//     Image parts carrying inline Data are encoded as a data: URL;
//     parts with URI are passed through as-is. Document parts emit a
//     `type:"file"` block — OpenAI's file-input shape. Adapters that
//     speak vendor-specific shapes (Anthropic, Bedrock, Vertex) walk
//     Parts directly and ignore this method.
func (m Message) MarshalJSON() ([]byte, error) {
	if len(m.Parts) > 0 {
		return m.marshalMultimodalJSON()
	}
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

// marshalMultimodalJSON emits the OpenAI multimodal content shape. The
// `content` field becomes a typed-block array; text parts gain a tiny
// {type:"text",text} envelope and image parts ride as image_url blocks
// (either a data: URL or a passthrough URI).
func (m Message) marshalMultimodalJSON() ([]byte, error) {
	type imageURL struct {
		URL string `json:"url"`
	}
	type fileBlock struct {
		Filename string `json:"filename,omitempty"`
		FileData string `json:"file_data,omitempty"` // base64 with data: prefix
		FileID   string `json:"file_id,omitempty"`   // for previously-uploaded refs
	}
	type contentBlock struct {
		Type     string     `json:"type"`
		Text     string     `json:"text,omitempty"`
		ImageURL *imageURL  `json:"image_url,omitempty"`
		File     *fileBlock `json:"file,omitempty"`
	}
	type alias struct {
		Role       string         `json:"role"`
		Content    []contentBlock `json:"content"`
		ToolCalls  []ToolCall     `json:"tool_calls,omitempty"`
		ToolCallID string         `json:"tool_call_id,omitempty"`
		Name       string         `json:"name,omitempty"`
	}

	blocks := make([]contentBlock, 0, len(m.Parts)+1)
	// Content + Parts both populated → prepend Content as a text
	// block. Lets callers attach an image to a typed prompt without
	// rebuilding the text into a Part.
	if m.Content != "" {
		blocks = append(blocks, contentBlock{Type: "text", Text: m.Content})
	}
	for _, p := range m.Parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, contentBlock{Type: "text", Text: p.Text})
		case "image":
			url := p.URI
			if url == "" {
				url = p.dataURL()
			}
			blocks = append(blocks, contentBlock{Type: "image_url", ImageURL: &imageURL{URL: url}})
		case "document":
			fb := &fileBlock{}
			if p.URI != "" {
				fb.FileID = p.URI
			} else {
				fb.FileData = p.dataURL()
			}
			blocks = append(blocks, contentBlock{Type: "file", File: fb})
		}
	}

	return json.Marshal(alias{
		Role:       m.Role,
		Content:    blocks,
		ToolCalls:  m.ToolCalls,
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
	})
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
