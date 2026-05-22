// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// AnthropicClient wraps the official anthropic-sdk-go Messages API as a
// ChatClient. Translation in/out is here so the rest of enso keeps using
// the OpenAI-shaped llm.ChatRequest / llm.Event taxonomy unchanged.
//
// We let the SDK do auth + HTTP + SSE decoding, and layer our own
// connTracker + debug logging on top — the same machinery OpenAIClient
// uses, so the TUI's "reconnecting / disconnected" indicator works
// across providers.
type AnthropicClient struct {
	APIKey string
	Model  string

	// BaseURL overrides the SDK's default https://api.anthropic.com.
	// Leave empty in production; tests / proxies set this.
	BaseURL string

	// HTTPClient is the transport handed to the SDK. nil leaves the SDK
	// at its default. Tests inject a RoundTripper-backed client here to
	// drive deterministic failure modes — same seam as OpenAIClient.
	HTTPClient *http.Client

	// MaxTokens is the Messages API's required `max_tokens`. Defaults
	// to 8192 (covers Claude's typical long-form responses) if unset.
	MaxTokens int64

	// ExtendedThinking enables Claude's extended-thinking content blocks.
	// When true, the request includes a thinking config and forces
	// temperature=1 (Anthropic rejects other values with thinking on).
	ExtendedThinking bool

	// ExtendedThinkingBudget is the thinking-token budget; must be ≥1024
	// and < MaxTokens. Zero falls back to 4096.
	ExtendedThinkingBudget int64

	// PromptCaching opts into Anthropic's ephemeral-prompt-cache. When
	// true, buildAnthropicParams sets cache_control:{type:"ephemeral"}
	// on the final system block and the final tool. The system + tool
	// prefix becomes a single cacheable block; reused across turns the
	// hit ratio approaches 100% until the system or tool set changes.
	// Cost note: cache writes are billed at 1.25x input, cache reads at
	// 0.1x — break-even after roughly two reuses.
	PromptCaching bool

	// AnthropicVersion overrides the `anthropic-version` header. Empty
	// uses the SDK's pinned default.
	AnthropicVersion string

	// ProbeInterval is the recovery-probe tick — test seam. Zero falls
	// back to the package default (probeInterval).
	ProbeInterval time.Duration

	// conn tracks transport state for the TUI indicator (same contract
	// as OpenAIClient.conn). Only network-class failures move it; HTTP
	// 4xx/5xx leave it Connected because TLS+TCP succeeded.
	conn connTracker

	// sdk is lazily constructed on first Chat call so config changes
	// before the first use stick.
	sdk *anthropic.Client
}

// LLMConnState satisfies ConnStateReporter so the TUI status bar can
// surface reconnect/disconnected without caring which vendor backs the
// provider.
func (c *AnthropicClient) LLMConnState() ConnState { return c.conn.get() }

func (c *AnthropicClient) client() *anthropic.Client {
	if c.sdk != nil {
		return c.sdk
	}
	opts := []option.RequestOption{}
	if c.APIKey != "" {
		opts = append(opts, option.WithAPIKey(c.APIKey))
	}
	if c.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(c.BaseURL))
	}
	if c.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(c.HTTPClient))
	}
	if c.AnthropicVersion != "" {
		opts = append(opts, option.WithHeader("anthropic-version", c.AnthropicVersion))
	}
	// The SDK retries internally on 429/5xx, but our Pool already
	// serializes calls and we want a single source of retry policy.
	// Disable the SDK's retries; recovery probe handles long outages.
	opts = append(opts, option.WithMaxRetries(0))

	sdk := anthropic.NewClient(opts...)
	c.sdk = &sdk
	return c.sdk
}

// Chat is the ChatClient entry point. It translates the OpenAI-shaped
// ChatRequest into Anthropic's Messages API call, opens the stream, and
// republishes events on a channel using our Event taxonomy.
func (c *AnthropicClient) Chat(ctx context.Context, req ChatRequest) (<-chan Event, error) {
	maxTokens := c.MaxTokens
	if maxTokens == 0 {
		maxTokens = 8192
	}

	params, err := c.buildParams(req, maxTokens)
	if err != nil {
		return nil, fmt.Errorf("anthropic: build params: %w", err)
	}

	return streamAnthropic(ctx, c.client(), params, &c.conn, c.startRecoveryProbe, "anthropic"), nil
}

// streamAnthropic drives one Messages-API streaming turn against any
// configured anthropic.Client and emits enso's Event taxonomy. Shared
// between AnthropicClient and BedrockClient — Bedrock is the same wire
// protocol after the SDK adds its AWS SigV4 middleware.
//
// label tags debug-log lines so concurrent provider streams stay
// distinguishable in the log file. startProbe is invoked at most once
// when a transport-class failure pushes the tracker into Disconnected;
// it may be nil if the caller doesn't have a probe loop (e.g., a
// vendor with no cheap reachability ping).
func streamAnthropic(
	ctx context.Context,
	sdk *anthropic.Client,
	params anthropic.MessageNewParams,
	conn *connTracker,
	startProbe func(),
	label string,
) <-chan Event {
	if data, mErr := json.Marshal(params); mErr == nil {
		fmt.Fprintf(debugLog(), "%s POST messages\nbody: %s\n", label, string(data))
	}

	stream := sdk.Messages.NewStreaming(ctx, params)
	eventCh := make(chan Event, 32)

	go func() {
		defer close(eventCh)
		defer stream.Close()

		// inflight collects per-content-block tool-use state. Anthropic
		// streams tool input as input_json_delta fragments under a
		// content_block_start of type "tool_use"; we accumulate, then
		// emit one ToolCall when content_block_stop arrives.
		type toolAcc struct {
			id, name string
			input    strings.Builder
		}
		inflight := map[int64]*toolAcc{}

		for stream.Next() {
			ev := stream.Current()
			fmt.Fprintf(debugLog(), "%s sse: %s\n", label, ev.RawJSON())

			switch ev.Type {
			case "content_block_start":
				cb := ev.AsContentBlockStart()
				if cb.ContentBlock.Type == "tool_use" {
					tu := cb.ContentBlock.AsToolUse()
					inflight[cb.Index] = &toolAcc{id: tu.ID, name: tu.Name}
				}

			case "content_block_delta":
				cbd := ev.AsContentBlockDelta()
				delta := cbd.Delta
				switch delta.Type {
				case "text_delta":
					if t := delta.AsTextDelta(); t.Text != "" {
						eventCh <- Event{Type: EventTextDelta, Text: t.Text}
					}
				case "thinking_delta":
					if t := delta.AsThinkingDelta(); t.Thinking != "" {
						eventCh <- Event{Type: EventReasoningDelta, Text: t.Thinking}
					}
				case "input_json_delta":
					if acc, ok := inflight[cbd.Index]; ok {
						acc.input.WriteString(delta.AsInputJSONDelta().PartialJSON)
					}
				}

			case "content_block_stop":
				cbs := ev.AsContentBlockStop()
				if acc, ok := inflight[cbs.Index]; ok {
					args := acc.input.String()
					if args == "" {
						// Tool call with no arguments still needs a
						// valid JSON object body — agent code parses
						// arguments with json.Unmarshal.
						args = "{}"
					}
					call := ToolCall{
						ID:   acc.id,
						Type: "function",
					}
					call.Function.Name = acc.name
					call.Function.Arguments = args
					eventCh <- Event{Type: EventToolCallComplete, ToolCalls: []ToolCall{call}}
					delete(inflight, cbs.Index)
				}

			case "message_start":
				// Initial Usage carries input + cache_read counts.
				ms := ev.AsMessageStart()
				logAnthropicUsage(label, ms.Message.Usage.InputTokens,
					ms.Message.Usage.OutputTokens,
					ms.Message.Usage.CacheReadInputTokens,
					ms.Message.Usage.CacheCreationInputTokens)

			case "message_delta":
				// Final Usage. Output tokens land here; input + cache
				// counts mirror what message_start already reported.
				md := ev.AsMessageDelta()
				logAnthropicUsage(label, md.Usage.InputTokens,
					md.Usage.OutputTokens,
					md.Usage.CacheReadInputTokens,
					md.Usage.CacheCreationInputTokens)

			case "message_stop":
				// Real terminator; the loop will exit on next Next().
			}
		}

		if err := stream.Err(); err != nil {
			if classifyTransportError(err) != "" {
				if conn.set(StateDisconnected) != StateDisconnected && startProbe != nil {
					startProbe()
				}
			}
			eventCh <- Event{Type: EventError, Error: friendlyHTTPError(label, err)}
			return
		}

		conn.set(StateConnected)
		eventCh <- Event{Type: EventDone}
	}()

	return eventCh
}

// startRecoveryProbe spawns the at-most-one probe goroutine for c.
// Mirrors OpenAIClient's pattern: tick at ProbeInterval, exit once the
// endpoint answers (state flips to Connected) or some other code path
// has already moved the tracker out of Disconnected.
func (c *AnthropicClient) startRecoveryProbe() {
	if !c.conn.claimProbe() {
		return
	}
	go c.probeLoop()
}

func (c *AnthropicClient) probeLoop() {
	defer c.conn.releaseProbe()
	interval := probeInterval
	if c.ProbeInterval > 0 {
		interval = c.ProbeInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		if c.conn.get() != StateDisconnected {
			return
		}
		if c.probeOnce() {
			c.conn.set(StateConnected)
			return
		}
	}
}

// probeOnce returns true if the API is reachable. We hit Models.Get on
// the configured model — a cheap GET that completes TLS+TCP. Any
// non-transport error (401 bad key, 404 unknown model) still counts as
// reachable, matching OpenAIClient.probeOnce semantics.
func (c *AnthropicClient) probeOnce() bool {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	_, err := c.client().Models.Get(ctx, c.Model, anthropic.ModelGetParams{})
	if err == nil {
		return true
	}
	// Transport-class failures keep the tracker disconnected; anything
	// else (API errors with a real HTTP response) means TLS+TCP worked.
	return classifyTransportError(err) == ""
}

// buildParams is AnthropicClient's bound form of buildAnthropicParams,
// kept for symmetry with the test surface.
func (c *AnthropicClient) buildParams(req ChatRequest, maxTokens int64) (anthropic.MessageNewParams, error) {
	return buildAnthropicParams(req, c.Model, maxTokens, c.ExtendedThinking, c.ExtendedThinkingBudget, c.PromptCaching)
}

// buildAnthropicParams produces MessageNewParams ready for the Messages
// API, with the extended-thinking and prompt-caching layers applied if
// requested. Shared between all three Anthropic adapters — the wire
// protocol is identical; only the SDK construction differs.
func buildAnthropicParams(req ChatRequest, model string, maxTokens int64, thinking bool, thinkingBudget int64, promptCaching bool) (anthropic.MessageNewParams, error) {
	params, err := toAnthropicParams(req, model, maxTokens)
	if err != nil {
		return params, err
	}
	if promptCaching {
		applyAnthropicPromptCaching(&params)
	}
	if thinking {
		budget := thinkingBudget
		if budget == 0 {
			budget = 4096
		}
		// Anthropic rejects budgets < 1024 or ≥ max_tokens — clamp here
		// so a misconfigured value doesn't 400 the whole turn.
		if budget < 1024 {
			budget = 1024
		}
		if budget >= maxTokens {
			budget = maxTokens - 1
		}
		params.Thinking = anthropic.ThinkingConfigParamOfEnabled(budget)
		// Thinking requires temperature=1; silently override since the
		// alternative is a hard API rejection the user can't recover
		// from inside a streaming turn.
		params.Temperature = param.NewOpt(1.0)
		// TopP / TopK also rejected with thinking on — clear them.
		params.TopP = param.Opt[float64]{}
		params.TopK = param.Opt[int64]{}
	}
	return params, nil
}

// toAnthropicParams translates an OpenAI-shaped ChatRequest into the
// Messages API's MessageNewParams.
func toAnthropicParams(req ChatRequest, model string, maxTokens int64) (anthropic.MessageNewParams, error) {
	params := anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: maxTokens,
	}

	system, msgs, err := splitSystem(req.Messages)
	if err != nil {
		return params, err
	}
	if len(system) > 0 {
		params.System = system
	}
	params.Messages = msgs

	if len(req.Tools) > 0 {
		tools, err := toAnthropicTools(req.Tools)
		if err != nil {
			return params, err
		}
		params.Tools = tools
	}

	if req.Temperature != 0 {
		params.Temperature = param.NewOpt(req.Temperature)
	}
	if req.TopP != 0 {
		params.TopP = param.NewOpt(req.TopP)
	}
	if req.TopK != 0 {
		params.TopK = param.NewOpt(int64(req.TopK))
	}
	// MinP and PresencePenalty have no Anthropic equivalent; dropped silently.

	return params, nil
}

// splitSystem pulls all role="system" messages out into a top-level
// system prompt (concatenated, Anthropic-style) and returns the rest
// translated to MessageParams.
//
// When a Message has non-empty Parts, the multimodal path runs: text
// parts plus image / document blocks. Empty Parts falls back to the
// legacy single-string Content path so existing flows are untouched.
func splitSystem(in []Message) ([]anthropic.TextBlockParam, []anthropic.MessageParam, error) {
	var systemBlocks []anthropic.TextBlockParam
	var out []anthropic.MessageParam

	for _, m := range in {
		switch m.Role {
		case "system":
			// System messages are text-only in Anthropic's shape; ignore
			// any non-text parts a caller might have set.
			if m.Content != "" {
				systemBlocks = append(systemBlocks, anthropic.TextBlockParam{Text: m.Content})
			}
			for _, p := range m.Parts {
				if p.Type == "text" && p.Text != "" {
					systemBlocks = append(systemBlocks, anthropic.TextBlockParam{Text: p.Text})
				}
			}

		case "user":
			blocks, err := userMessageBlocks(m)
			if err != nil {
				return nil, nil, err
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleUser,
				Content: blocks,
			})

		case "assistant":
			blocks, err := assistantBlocks(m)
			if err != nil {
				return nil, nil, err
			}
			if len(blocks) == 0 {
				// Anthropic rejects empty-content turns; skip.
				continue
			}
			out = append(out, anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleAssistant,
				Content: blocks,
			})

		case "tool":
			// OpenAI's tool-result message: role="tool", ToolCallID,
			// Content (+ optional Parts). Anthropic models this as a
			// user turn carrying a tool_result block; the block itself
			// can carry text + image content for tools that return
			// images (read tool on a PNG, etc.).
			result, err := toolResultBlock(m)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, anthropic.NewUserMessage(result))

		default:
			return nil, nil, fmt.Errorf("anthropic: unsupported message role %q", m.Role)
		}
	}

	return systemBlocks, out, nil
}

// userMessageBlocks builds the content slice for a user-role message,
// honouring Parts when populated. A plain string Content with no Parts
// reduces to one NewTextBlock — same wire shape as before.
func userMessageBlocks(m Message) ([]anthropic.ContentBlockParamUnion, error) {
	if len(m.Parts) == 0 {
		if m.Content == "" {
			return nil, nil
		}
		return []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(m.Content)}, nil
	}

	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.Parts)+1)
	if m.Content != "" {
		// Content + Parts both populated: prepend Content as a text
		// block. Lets a caller attach an image to a text prompt
		// without rebuilding the text into a Part.
		blocks = append(blocks, anthropic.NewTextBlock(m.Content))
	}
	for _, p := range m.Parts {
		blk, err := anthropicContentBlock(p)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, blk)
	}
	return blocks, nil
}

func assistantBlocks(m Message) ([]anthropic.ContentBlockParamUnion, error) {
	var blocks []anthropic.ContentBlockParamUnion
	// Assistant messages from the model never carry inline media in
	// today's flow (Claude can return images via tools, not directly).
	// Parts on an assistant message are still threaded through so a
	// caller building a fake history can include them.
	if m.Content != "" {
		blocks = append(blocks, anthropic.NewTextBlock(m.Content))
	}
	for _, p := range m.Parts {
		blk, err := anthropicContentBlock(p)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, blk)
	}
	for _, tc := range m.ToolCalls {
		var input any
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" {
			input = map[string]any{}
		} else if err := json.Unmarshal([]byte(args), &input); err != nil {
			return nil, fmt.Errorf("anthropic: tool call %q arguments not JSON: %w", tc.Function.Name, err)
		}
		blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, input, tc.Function.Name))
	}
	return blocks, nil
}

// anthropicContentBlock translates one MessagePart onto the SDK's
// ContentBlockParamUnion. Used by user + assistant paths. Tool-result
// blocks need a different union (toolResultBlock handles that).
func anthropicContentBlock(p MessagePart) (anthropic.ContentBlockParamUnion, error) {
	switch p.Type {
	case "text":
		return anthropic.NewTextBlock(p.Text), nil
	case "image":
		if len(p.Data) > 0 {
			return anthropic.NewImageBlockBase64(p.MIMEType, base64.StdEncoding.EncodeToString(p.Data)), nil
		}
		if p.URI != "" {
			return anthropic.NewImageBlock(anthropic.URLImageSourceParam{URL: p.URI}), nil
		}
		return anthropic.ContentBlockParamUnion{}, fmt.Errorf("anthropic: image part has no data or uri")
	case "document":
		if len(p.Data) > 0 {
			return anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{
				Data:      base64.StdEncoding.EncodeToString(p.Data),
				MediaType: "application/pdf",
			}), nil
		}
		if p.URI != "" {
			return anthropic.NewDocumentBlock(anthropic.URLPDFSourceParam{URL: p.URI}), nil
		}
		return anthropic.ContentBlockParamUnion{}, fmt.Errorf("anthropic: document part has no data or uri")
	default:
		return anthropic.ContentBlockParamUnion{}, fmt.Errorf("anthropic: unknown part type %q", p.Type)
	}
}

// toolResultBlock builds the tool_result block for a role="tool"
// message. The block can carry text + image content when the tool
// returned an image (e.g. read tool on a PNG); falls back to the
// legacy single-text body when only Content is set.
func toolResultBlock(m Message) (anthropic.ContentBlockParamUnion, error) {
	if len(m.Parts) == 0 {
		return anthropic.NewToolResultBlock(m.ToolCallID, m.Content, false), nil
	}

	var content []anthropic.ToolResultBlockParamContentUnion
	if m.Content != "" {
		content = append(content, anthropic.ToolResultBlockParamContentUnion{
			OfText: &anthropic.TextBlockParam{Text: m.Content},
		})
	}
	for _, p := range m.Parts {
		switch p.Type {
		case "text":
			content = append(content, anthropic.ToolResultBlockParamContentUnion{
				OfText: &anthropic.TextBlockParam{Text: p.Text},
			})
		case "image":
			if len(p.Data) == 0 {
				return anthropic.ContentBlockParamUnion{}, fmt.Errorf("anthropic: tool_result image part has no data")
			}
			content = append(content, anthropic.ToolResultBlockParamContentUnion{
				OfImage: &anthropic.ImageBlockParam{
					Source: anthropic.ImageBlockParamSourceUnion{
						OfBase64: &anthropic.Base64ImageSourceParam{
							Data:      base64.StdEncoding.EncodeToString(p.Data),
							MediaType: anthropic.Base64ImageSourceMediaType(p.MIMEType),
						},
					},
				},
			})
		default:
			return anthropic.ContentBlockParamUnion{}, fmt.Errorf("anthropic: tool_result %q parts unsupported", p.Type)
		}
	}
	return anthropic.ContentBlockParamUnion{
		OfToolResult: &anthropic.ToolResultBlockParam{
			ToolUseID: m.ToolCallID,
			Content:   content,
		},
	}, nil
}

func toAnthropicTools(in []ToolDef) ([]anthropic.ToolUnionParam, error) {
	out := make([]anthropic.ToolUnionParam, 0, len(in))
	for _, t := range in {
		tp := anthropic.ToolParam{Name: t.Function.Name}
		if t.Function.Description != "" {
			tp.Description = param.NewOpt(t.Function.Description)
		}
		schema, err := toAnthropicSchema(t.Function.Parameters)
		if err != nil {
			return nil, fmt.Errorf("anthropic: tool %q schema: %w", t.Function.Name, err)
		}
		tp.InputSchema = schema
		out = append(out, anthropic.ToolUnionParam{OfTool: &tp})
	}
	return out, nil
}

// toAnthropicSchema lifts an OpenAI-style JSON-schema map (the whole
// thing — `type`, `properties`, `required`, plus any extras) into
// Anthropic's ToolInputSchemaParam, which has dedicated fields for
// Properties + Required and an ExtraFields catch-all for the rest.
func toAnthropicSchema(params map[string]interface{}) (anthropic.ToolInputSchemaParam, error) {
	var schema anthropic.ToolInputSchemaParam
	if params == nil {
		return schema, nil
	}
	for k, v := range params {
		switch k {
		case "type":
			// Anthropic hardcodes "object" via the constant.Object
			// default; non-object schemas at the top level aren't
			// supported by the tool-use API anyway.
		case "properties":
			schema.Properties = v
		case "required":
			req, err := toStringSlice(v)
			if err != nil {
				return schema, fmt.Errorf("required: %w", err)
			}
			schema.Required = req
		default:
			if schema.ExtraFields == nil {
				schema.ExtraFields = map[string]any{}
			}
			schema.ExtraFields[k] = v
		}
	}
	return schema, nil
}

func toStringSlice(v interface{}) ([]string, error) {
	switch s := v.(type) {
	case nil:
		return nil, nil
	case []string:
		return s, nil
	case []interface{}:
		out := make([]string, 0, len(s))
		for i, item := range s {
			str, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("element %d not a string", i)
			}
			out = append(out, str)
		}
		return out, nil
	default:
		return nil, errors.New("not a string array")
	}
}

// logAnthropicUsage emits a debug-log line summarising input / output
// / cache token counts. We don't surface this through the Event channel
// yet — the agent's token accounting still uses local estimation —
// but logging means cache effectiveness shows up in the debug stream
// without having to crack open the cloud-side dashboard.
func logAnthropicUsage(label string, in, out, cacheRead, cacheCreate int64) {
	if in == 0 && out == 0 && cacheRead == 0 && cacheCreate == 0 {
		return
	}
	fmt.Fprintf(debugLog(), "%s usage: in=%d out=%d cache_read=%d cache_create=%d\n",
		label, in, out, cacheRead, cacheCreate)
}

// applyAnthropicPromptCaching inserts ephemeral cache_control markers
// on the boundaries that change rarely: the final system block and
// the final tool. Anthropic caches the entire prefix up to and
// including each marker, so one marker after system gives the system
// prompt + tool definitions a stable cacheable prefix on every turn
// that doesn't touch them. Adding a second marker on tools doubles
// the chance the cache survives a system-prompt edit that left tools
// alone — Anthropic permits up to 4 markers per request, so spending
// 2 of them on the stable layers is cheap.
//
// No-op when there's nothing to cache (no system blocks, no tools).
func applyAnthropicPromptCaching(params *anthropic.MessageNewParams) {
	if n := len(params.System); n > 0 {
		params.System[n-1].CacheControl = anthropic.NewCacheControlEphemeralParam()
	}
	if n := len(params.Tools); n > 0 {
		// Tools is a union slice; cache_control lives on the
		// ToolParam variant (OfTool). Only mark function-tool entries;
		// other kinds (computer-use, bash, etc.) are not yet emitted
		// by the translator but adding a guard now avoids a future
		// nil panic.
		if t := params.Tools[n-1].OfTool; t != nil {
			t.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
	}
}
