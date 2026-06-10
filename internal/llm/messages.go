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

	// Synthetic marks a message injected programmatically — compaction
	// summaries, environment reminders, contextual-instruction
	// injections. Still sent to the model verbatim; the agent uses this
	// flag to detect a prior compaction summary on resume (so a second
	// compaction pass can UPDATE it rather than re-summarize lossily)
	// and the TUI may render these turns dimmed. Not serialized on the
	// wire — internal-only metadata.
	Synthetic bool `json:"-"`

	// Ignored marks a message present in History for display/audit but
	// excluded from the outgoing ChatRequest.Messages payload sent to
	// providers. Useful for "operator left a comment" rows or similar
	// audit traces. Adapters MUST filter these out during serialization;
	// FilterForRequest centralises that walk.
	Ignored bool `json:"-"`

	// Reasoning holds the assistant turn's chain-of-thought, captured for
	// REPLAY ONLY. It is `json:"-"` and deliberately absent from every
	// MarshalJSON path (the explicit alias structs never reference it), so
	// it is NEVER sent back to a provider — the model re-derives its
	// reasoning each turn, and resending bloats context. Persisted to the
	// session store so a resumed session / /transcript can show the
	// thinking the user saw live; empty on every non-assistant message and
	// on providers that don't surface a reasoning channel.
	Reasoning string `json:"-"`
}

// FilterForRequest returns msgs with Ignored entries removed. Provider
// adapters call this immediately before serializing so audit-only
// rows never reach the wire. Synthetic rows are preserved — they're
// real wire-level messages, just programmatically generated.
//
// Returns the input slice unchanged when no Ignored entries are
// present (the common case), avoiding allocation in the hot path.
func FilterForRequest(msgs []Message) []Message {
	keep := true
	for _, m := range msgs {
		if m.Ignored {
			keep = false
			break
		}
	}
	if keep {
		return msgs
	}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Ignored {
			continue
		}
		out = append(out, m)
	}
	return out
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
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
	Tools    []ToolDef `json:"tools,omitempty"`
	// MaxTokens caps a single generation (llama.cpp's n_predict). The
	// OpenAIClient sets it from resolved config; omitted on the wire when
	// zero so servers without a cap keep their own default. This is the
	// hard backstop against runaway / degeneration loops — a model that
	// stops emitting EOS can't stream past this many output tokens.
	MaxTokens int `json:"max_tokens,omitempty"`
	// Stop is the optional list of stop strings. Empty on the local path
	// today; reserved for callers that want server-side termination.
	Stop            []string `json:"stop,omitempty"`
	Temperature     float64  `json:"temperature,omitempty"`
	TopK            int      `json:"top_k,omitempty"`
	TopP            float64  `json:"top_p,omitempty"`
	MinP            float64  `json:"min_p,omitempty"`
	PresencePenalty float64  `json:"presence_penalty,omitempty"`
	// FrequencyPenalty is OpenAI's repeat-discouragement knob;
	// RepetitionPenalty maps to llama.cpp's repeat_penalty. Both omitted on
	// the wire when zero so a server keeps its own default.
	FrequencyPenalty  float64 `json:"frequency_penalty,omitempty"`
	RepetitionPenalty float64 `json:"repetition_penalty,omitempty"`
}

// ToolDef is the JSON Schema definition sent to the model.
type ToolDef struct {
	Type     string          `json:"type"`
	Function ToolFunctionDef `json:"function"`
}

// ToolFunctionDef describes a tool's name, description, and parameters.
type ToolFunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
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
//
// Usage is populated only in the trailing chunk and only when the request
// asked for it via stream_options.include_usage = true. Servers that
// don't implement that extension (some llama.cpp builds) leave it nil
// and the chunk's Choices array is the only meaningful field.
type ChatCompletionChunk struct {
	Choices []ChunkChoice `json:"choices"`
	Usage   *ChunkUsage   `json:"usage,omitempty"`
}

// ChunkUsage carries OpenAI's per-turn token counts. prompt_tokens is
// the total-including-cached; prompt_tokens_details.cached_tokens is a
// sub-line of prompt_tokens, not additive. Translated to MessageUsage
// in the adapter; downstream code should not see this type.
type ChunkUsage struct {
	PromptTokens        int                       `json:"prompt_tokens"`
	CompletionTokens    int                       `json:"completion_tokens"`
	TotalTokens         int                       `json:"total_tokens"`
	PromptTokensDetails *ChunkPromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

// ChunkPromptTokensDetails is the cached_tokens sub-block on
// ChunkUsage. Separated for nil-safe access on servers that omit the
// details object.
type ChunkPromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
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
		Name      string `json:"name,omitempty"`
		Arguments any    `json:"arguments,omitempty"` // string or JSON object (llama.cpp quirk)
	} `json:"function,omitempty"`
}

// MessageUsage carries provider-reported token counts for a single
// assistant turn. Populated from the adapter's final usage event;
// zero-valued when the provider didn't supply numbers (e.g. some
// llama.cpp builds, mid-stream failures, or backends that don't
// implement usage reporting yet).
//
// Cache accounting differs per provider:
//   - Anthropic: InputTokens is fresh-only; cache reads/writes are
//     reported separately in CacheReadTokens / CacheWriteTokens.
//   - OpenAI: InputTokens (prompt_tokens) is total-including-cached;
//     CacheReadTokens (prompt_tokens_details.cached_tokens) is a
//     sub-line of InputTokens, not additive.
//   - Gemini: TotalTokens is authoritative; do not recompute.
//   - Bedrock Converse: same shape as Anthropic.
//
// Callers comparing against the model's context window should read
// TotalTokens rather than summing fields manually.
type MessageUsage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	ReasoningTokens  int
	TotalTokens      int
}

// EffectiveInputTokens returns the prompt-side token count for this turn —
// the tokens that count toward the context window and the cumulative input
// spend — normalized across providers' divergent cache accounting.
//
// The trap: OpenAI/Gemini report InputTokens as the FULL prompt already
// including cached reads (CacheReadTokens is a sub-line, not additive), so
// summing InputTokens + CacheReadTokens double-counts the cache and can
// roughly 2× the figure on a warm local cache. Anthropic/Bedrock instead
// report InputTokens as fresh-only with cache reads/writes broken out, so
// for them the prompt size genuinely is the sum.
//
// TotalTokens - OutputTokens collapses both shapes to the same answer
// (prompt side only): Anthropic Total = in+out+cacheR+cacheW so the
// subtraction yields in+cacheR+cacheW; OpenAI/Gemini Total = prompt+output
// so it yields prompt. When TotalTokens is absent (some llama.cpp builds),
// fall back to summing — correct for the Anthropic shape and the best
// available without a total.
func (u MessageUsage) EffectiveInputTokens() int {
	if u.TotalTokens > 0 && u.TotalTokens >= u.OutputTokens {
		return u.TotalTokens - u.OutputTokens
	}
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// Empty reports whether no field has been populated. Used as the
// "fall back to heuristic" signal — a fully zero MessageUsage means
// the provider didn't supply usage data for this turn.
func (u MessageUsage) Empty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheReadTokens == 0 && u.CacheWriteTokens == 0 &&
		u.ReasoningTokens == 0 && u.TotalTokens == 0
}
