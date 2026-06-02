// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestMessageMarshal_ToolMessageWithEmptyContent locks in the fix for
// the HTTP 400 surfaced by the eval harness: tool result messages must
// carry a `content` field even when empty, because some OpenAI-compatible
// servers reject requests where non-assistant messages drop content.
func TestMessageMarshal_ToolMessageWithEmptyContent(t *testing.T) {
	m := Message{Role: "tool", ToolCallID: "c1", Name: "bash", Content: ""}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"content":""`) {
		t.Errorf("tool message must emit content even when empty; got %s", b)
	}
}

func TestMessageMarshal_UserEmptyContent(t *testing.T) {
	m := Message{Role: "user", Content: ""}
	b, _ := json.Marshal(m)
	if !strings.Contains(string(b), `"content":""`) {
		t.Errorf("user message must emit content even when empty; got %s", b)
	}
}

// TestMessageMarshal_AssistantWithToolCallsOmitsContent — the spec's
// only ergonomic concession: an assistant message that delegates to tool
// calls may omit content. We follow that to avoid sending an empty
// string where the model didn't speak.
func TestMessageMarshal_AssistantWithToolCallsOmitsContent(t *testing.T) {
	m := Message{
		Role: "assistant",
		ToolCalls: []ToolCall{{
			ID:   "c1",
			Type: "function",
		}},
	}
	b, _ := json.Marshal(m)
	if strings.Contains(string(b), `"content"`) {
		t.Errorf("assistant-with-tool_calls should omit content; got %s", b)
	}
}

// TestMessageMarshal_AssistantTextOnly — when an assistant message has
// text but no tool calls, content is sent (even if empty).
func TestMessageMarshal_AssistantTextOnly(t *testing.T) {
	m := Message{Role: "assistant", Content: "ok"}
	b, _ := json.Marshal(m)
	if !strings.Contains(string(b), `"content":"ok"`) {
		t.Errorf("assistant-text content missing; got %s", b)
	}
}

func TestMessageRoundTrip(t *testing.T) {
	in := Message{
		Role:       "tool",
		Content:    "(exit 0, no output)",
		ToolCallID: "c1",
		Name:       "bash",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Message
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("roundtrip mismatch:\nin:  %+v\nout: %+v", in, out)
	}
}

// TestMessageMarshal_PartsEmitsOpenAIMultimodal verifies the
// multimodal branch: when Parts is populated, content becomes an
// array of {type,text|image_url|file} blocks rather than a string.
// Test exercises text + inline-image (data URL) + URI passthrough.
func TestMessageMarshal_PartsEmitsOpenAIMultimodal(t *testing.T) {
	pngBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	in := Message{
		Role:    "user",
		Content: "what is in this image?",
		Parts: []MessagePart{
			NewImagePart("image/png", pngBytes),
			NewImagePartURI("https://example.com/cat.jpg"),
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)
	// Content should NOT be a string — must be an array of blocks.
	if !strings.Contains(js, `"content":[`) {
		t.Fatalf("expected multimodal array content, got: %s", js)
	}
	// Text block carrying Content.
	if !strings.Contains(js, `"type":"text"`) || !strings.Contains(js, `"text":"what is in this image?"`) {
		t.Fatalf("text block missing: %s", js)
	}
	// Inline image becomes a data URL with the right MIME.
	if !strings.Contains(js, `"type":"image_url"`) {
		t.Fatalf("image_url block missing: %s", js)
	}
	if !strings.Contains(js, `"url":"data:image/png;base64,iVBORw0KGgo=`) {
		t.Fatalf("data URL with base64 not present: %s", js)
	}
	// URI image passes through untouched.
	if !strings.Contains(js, `"url":"https://example.com/cat.jpg"`) {
		t.Fatalf("URI image not preserved: %s", js)
	}
}

// TestMessageMarshal_PartsEmpty_KeepsLegacyShape pins the
// back-compat contract: a Message with no Parts marshals exactly as
// before this refactor. Existing flows MUST stay byte-identical.
func TestMessageMarshal_PartsEmpty_KeepsLegacyShape(t *testing.T) {
	in := Message{Role: "user", Content: "hi"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"role":"user","content":"hi"}` {
		t.Fatalf("legacy single-text shape broken: %s", b)
	}
}

// TestMessageMarshal_PartsWithoutContent works the multimodal path
// with Content empty — pure image+text mix in Parts. Confirms the
// content array still emits the message-spec-required `content` key.
func TestMessageMarshal_PartsWithoutContent(t *testing.T) {
	in := Message{
		Role: "user",
		Parts: []MessagePart{
			NewTextPart("describe this"),
			NewImagePart("image/jpeg", []byte("jpegbytes")),
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)
	if !strings.Contains(js, `"text":"describe this"`) {
		t.Fatalf("text part missing: %s", js)
	}
	if !strings.Contains(js, `"data:image/jpeg;base64,`) {
		t.Fatalf("image data URL missing: %s", js)
	}
}

// TestMessagePart_DataURL covers the helper directly — the part is
// re-used across adapters, so the encoding must be stable.
func TestMessagePart_DataURL(t *testing.T) {
	p := NewImagePart("image/png", []byte{0x00, 0x01, 0x02})
	got := p.dataURL()
	want := "data:image/png;base64,AAEC"
	if got != want {
		t.Fatalf("dataURL=%q, want %q", got, want)
	}
	// Empty data → empty result (caller uses as a guard).
	if (MessagePart{Type: "image", MIMEType: "image/png"}).dataURL() != "" {
		t.Fatalf("empty data must yield empty data URL")
	}
}

// TestNewHelpers covers the constructors so a typo in the type tag
// never sneaks past code review.
func TestNewHelpers(t *testing.T) {
	if NewTextPart("hi").Type != "text" {
		t.Fatal("text part type")
	}
	if NewImagePart("image/png", []byte{1}).Type != "image" {
		t.Fatal("image part type")
	}
	if NewImagePartURI("u").Type != "image" {
		t.Fatal("uri image part type")
	}
	if NewDocumentPart("application/pdf", []byte{1}).Type != "document" {
		t.Fatal("document part type")
	}
}

// TestEffectiveInputTokens_PerProvider guards C3: the prompt-side token
// count must be normalized across providers' divergent cache accounting so
// the OpenAI/Gemini "InputTokens already includes cached reads" shape isn't
// double-counted (which prematurely triggers compaction on warm caches).
func TestEffectiveInputTokens_PerProvider(t *testing.T) {
	cases := []struct {
		name string
		u    MessageUsage
		want int
	}{
		{
			// OpenAI: prompt_tokens(1000) includes cached(800); total = prompt+completion.
			name: "openai_cached_not_additive",
			u:    MessageUsage{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 800, TotalTokens: 1200},
			want: 1000, // NOT 1800
		},
		{
			// Gemini: same shape as OpenAI (cached is a sub-line of prompt).
			name: "gemini_cached_not_additive",
			u:    MessageUsage{InputTokens: 500, OutputTokens: 50, CacheReadTokens: 400, TotalTokens: 550},
			want: 500,
		},
		{
			// Anthropic: input is fresh-only; total = in+out+cacheR+cacheW.
			name: "anthropic_fresh_only",
			u:    MessageUsage{InputTokens: 100, OutputTokens: 60, CacheReadTokens: 900, CacheWriteTokens: 40, TotalTokens: 1100},
			want: 1040, // in + cacheR + cacheW = 100+900+40
		},
		{
			// No TotalTokens (some llama.cpp builds): fall back to summing.
			name: "no_total_fallback",
			u:    MessageUsage{InputTokens: 300, OutputTokens: 30, CacheReadTokens: 0},
			want: 300,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.u.EffectiveInputTokens(); got != tc.want {
				t.Errorf("EffectiveInputTokens() = %d, want %d", got, tc.want)
			}
		})
	}
}
