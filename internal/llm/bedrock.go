// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// defaultBedrockMaxTokens is the cap applied when ProviderConfig
// leaves max_tokens at zero. Bedrock's Converse API treats MaxTokens
// as optional but most production models behave better with an
// explicit ceiling, so we set one rather than rely on the default
// each model picks.
const defaultBedrockMaxTokens = 4096

// minThinkingBudget is the floor Anthropic imposes on Claude's
// extended-thinking token budget. Smaller values are rejected with a
// 400 — we clamp silently rather than surface a mid-stream error.
const minThinkingBudget = 1024

// defaultThinkingBudget is used when ExtendedThinking is enabled but
// the user leaves ExtendedThinkingBudget at zero.
const defaultThinkingBudget = 4096

// BedrockClient routes ChatRequests through AWS Bedrock's Converse API.
// Multi-vendor by design: any model supported by Converse — Claude,
// Nova, Llama, Mistral, Cohere, AI21 — is reachable through this one
// adapter. The Model field is the Bedrock model ID (e.g.
// "anthropic.claude-3-5-sonnet-20241022-v2:0", "amazon.nova-pro-v1:0",
// "meta.llama3-1-70b-instruct-v1:0") or an inference-profile ARN.
//
// Authentication follows the standard AWS credential chain: environment
// variables, shared config (~/.aws/credentials), EC2/ECS/EKS instance
// role. Region and Profile in the config override the chain.
//
// Not yet wired (deliberate v1 scope):
//   - image / document content blocks
//   - Bedrock Guardrails
//   - additionalModelRequestFields (Claude extended-thinking on Bedrock)
//   - inference-profile ARN-specific helpers
type BedrockClient struct {
	Model     string
	Region    string
	Profile   string
	MaxTokens int64

	// ExtendedThinking + ExtendedThinkingBudget enable Claude's
	// thinking blocks via Bedrock's additionalModelRequestFields.
	// Silently ignored by non-Claude models — the request will surface
	// an "unsupported field" 400 from Bedrock if you pair it with,
	// say, Nova. Constraints are applied automatically (temperature=1,
	// top_p cleared, budget clamped).
	ExtendedThinking       bool
	ExtendedThinkingBudget int64

	// GuardrailID, GuardrailVersion, GuardrailTrace configure Amazon
	// Bedrock Guardrails — content evaluation that runs in front of
	// the model and can block / mask / rewrite either side of the
	// conversation. Empty GuardrailID disables guardrails entirely.
	// When non-empty, GuardrailVersion is required ("DRAFT" is fine);
	// GuardrailTrace defaults to "enabled" so the trace shows up in
	// CloudWatch.
	GuardrailID      string
	GuardrailVersion string
	GuardrailTrace   string

	// PromptCaching opts into Bedrock Converse's CachePoint markers.
	// When true, applyBedrockCachePoints inserts cache markers after
	// the system content and after the last tool — equivalent to the
	// cache_control:ephemeral markers on the Anthropic-native paths,
	// but expressed as Converse's structured CachePointBlock. Bedrock
	// applies the same caching rules behind the scenes; the wire
	// encoding is what differs.
	PromptCaching bool

	// ProbeInterval is a test seam. Zero uses the package default.
	ProbeInterval time.Duration

	// conn tracks the last-known reachability state, surfaced to the
	// TUI through ConnStateReporter (same indicator the OpenAI adapter
	// uses).
	conn connTracker

	// sdkOnce + sdk: lazily build the client on first Chat so config
	// changes prior to first use take effect. Subsequent calls reuse
	// the same client.
	sdkOnce sync.Once
	sdk     bedrockConverseAPI
	sdkErr  error

	// newClient is a test seam — production leaves it nil. Tests inject
	// a fake to drive the streaming loop deterministically without
	// touching AWS.
	newClient func(ctx context.Context, region, profile string) (bedrockConverseAPI, error)
}

// bedrockConverseAPI narrows the SDK surface to just the one operation
// we use. Keeps tests from having to satisfy the full *bedrockruntime.Client.
type bedrockConverseAPI interface {
	ConverseStream(ctx context.Context, params *bedrockruntime.ConverseStreamInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error)
}

// LLMConnState satisfies ConnStateReporter.
func (c *BedrockClient) LLMConnState() ConnState { return c.conn.get() }

func (c *BedrockClient) client(ctx context.Context) (bedrockConverseAPI, error) {
	c.sdkOnce.Do(func() {
		factory := c.newClient
		if factory == nil {
			factory = newBedrockClient
		}
		c.sdk, c.sdkErr = factory(ctx, c.Region, c.Profile)
	})
	return c.sdk, c.sdkErr
}

func newBedrockClient(ctx context.Context, region, profile string) (bedrockConverseAPI, error) {
	loadOpts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(region))
	}
	if profile != "" {
		loadOpts = append(loadOpts, awsconfig.WithSharedConfigProfile(profile))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return bedrockruntime.NewFromConfig(awsCfg), nil
}

// Chat translates the OpenAI-shaped ChatRequest into a Converse stream
// input, dispatches it, and translates the streamed events back to the
// adapter-agnostic Event channel.
func (c *BedrockClient) Chat(ctx context.Context, req ChatRequest) (<-chan Event, error) {
	model := c.Model
	if model == "" {
		model = req.Model
	}

	input, err := buildConverseInput(req, model, c.MaxTokens)
	if err != nil {
		return nil, fmt.Errorf("bedrock: build request: %w", err)
	}

	if c.ExtendedThinking {
		applyExtendedThinking(input, c.ExtendedThinkingBudget)
	}
	if c.GuardrailID != "" {
		if err := applyBedrockGuardrail(input, c.GuardrailID, c.GuardrailVersion, c.GuardrailTrace); err != nil {
			return nil, fmt.Errorf("bedrock: guardrail: %w", err)
		}
	}
	if c.PromptCaching {
		applyBedrockCachePoints(input)
	}

	sdk, err := c.client(ctx)
	if err != nil {
		// Credential / config errors land here. These are user errors,
		// not transport failures — surface synchronously so the TUI
		// shows the real reason instead of "disconnected".
		return nil, fmt.Errorf("bedrock: %w", err)
	}

	fmt.Fprintf(debugLog(), "bedrock: ConverseStream model=%s region=%s\n", model, c.Region)

	out, err := sdk.ConverseStream(ctx, input)
	if err != nil {
		// Transport-class errors flip the indicator; HTTP/IAM errors
		// from Bedrock arrive here as well, but those leave the
		// indicator alone (TLS+TCP succeeded).
		if classifyTransportError(err) != "" {
			if c.conn.set(StateDisconnected) != StateDisconnected {
				c.startRecoveryProbe()
			}
		}
		return nil, fmt.Errorf("bedrock: %w", err)
	}
	c.conn.set(StateConnected)

	eventCh := make(chan Event, 32)
	stream := out.GetStream()

	go func() {
		defer close(eventCh)
		defer stream.Close()

		// One accumulator per content-block index. The Converse stream
		// interleaves blocks by ContentBlockIndex, so a single message
		// can have multiple in-flight tool_use blocks.
		tools := map[int32]*bedrockToolAcc{}

		for ev := range stream.Events() {
			switch e := ev.(type) {

			case *types.ConverseStreamOutputMemberContentBlockStart:
				idx := derefInt32(e.Value.ContentBlockIndex)
				if start, ok := e.Value.Start.(*types.ContentBlockStartMemberToolUse); ok {
					tools[idx] = &bedrockToolAcc{
						id:   derefString(start.Value.ToolUseId),
						name: derefString(start.Value.Name),
					}
				}

			case *types.ConverseStreamOutputMemberContentBlockDelta:
				idx := derefInt32(e.Value.ContentBlockIndex)
				switch d := e.Value.Delta.(type) {
				case *types.ContentBlockDeltaMemberText:
					if d.Value != "" {
						eventCh <- Event{Type: EventTextDelta, Text: d.Value}
					}
				case *types.ContentBlockDeltaMemberReasoningContent:
					// Reasoning content arrives from models with
					// thinking enabled (Claude on Bedrock, Nova reasoning
					// variants). Route to the same channel the TUI
					// already renders for OpenAI reasoning models.
					if rc, ok := d.Value.(*types.ReasoningContentBlockDeltaMemberText); ok && rc.Value != "" {
						eventCh <- Event{Type: EventReasoningDelta, Text: rc.Value}
					}
				case *types.ContentBlockDeltaMemberToolUse:
					if acc, ok := tools[idx]; ok && d.Value.Input != nil {
						acc.input.WriteString(*d.Value.Input)
					}
				}

			case *types.ConverseStreamOutputMemberContentBlockStop:
				idx := derefInt32(e.Value.ContentBlockIndex)
				if acc, ok := tools[idx]; ok {
					args := acc.input.String()
					// Bedrock omits the delta for tool calls whose
					// schema is `{}` — surface an empty object so the
					// downstream agent doesn't choke on invalid JSON.
					if args == "" {
						args = "{}"
					}
					tc := ToolCall{ID: acc.id, Type: "function"}
					tc.Function.Name = acc.name
					tc.Function.Arguments = args
					eventCh <- Event{Type: EventToolCallComplete, ToolCalls: []ToolCall{tc}}
					delete(tools, idx)
				}

			case *types.ConverseStreamOutputMemberMetadata:
				// Metadata carries TokenUsage including cache hit
				// counters. Log for the debug stream and emit via the
				// Event channel so the agent can use real numbers
				// instead of the 4-char heuristic.
				if u := e.Value.Usage; u != nil {
					fmt.Fprintf(debugLog(), "bedrock usage: in=%d out=%d cache_read=%d cache_write=%d\n",
						derefInt32(u.InputTokens), derefInt32(u.OutputTokens),
						derefInt32(u.CacheReadInputTokens), derefInt32(u.CacheWriteInputTokens))
					eventCh <- Event{Type: EventUsage, Usage: bedrockUsageFrom(
						derefInt32(u.InputTokens), derefInt32(u.OutputTokens),
						derefInt32(u.CacheReadInputTokens), derefInt32(u.CacheWriteInputTokens),
					)}
				}

			case *types.ConverseStreamOutputMemberMessageStart,
				*types.ConverseStreamOutputMemberMessageStop:
				// MessageStart carries role (always assistant for our
				// usage); MessageStop carries stop_reason. Neither
				// feeds the event channel today.
			}
		}

		if err := stream.Err(); err != nil {
			// Treat stream-side errors as best-effort: emit and stop.
			// Don't flip the conn indicator — we got far enough to open
			// the stream, so the transport itself was healthy.
			eventCh <- Event{Type: EventError, Error: fmt.Errorf("bedrock stream: %w", err)}
			return
		}
		eventCh <- Event{Type: EventDone}
	}()

	return eventCh, nil
}

// bedrockToolAcc accumulates partial tool-use input across delta
// events. One per ContentBlockIndex; finalized at ContentBlockStop.
type bedrockToolAcc struct {
	id    string
	name  string
	input strings.Builder
}

// startRecoveryProbe is intentionally stub-like. Bedrock's only cheap
// reachability check (ListFoundationModels) lives on a different
// service endpoint and IAM action than Converse, so probing there
// could fail spuriously while inference works fine. Hold the claim for
// one interval, then release; the next user-driven Chat call decides
// recovery.
func (c *BedrockClient) startRecoveryProbe() {
	if !c.conn.claimProbe() {
		return
	}
	go func() {
		defer c.conn.releaseProbe()
		interval := probeInterval
		if c.ProbeInterval > 0 {
			interval = c.ProbeInterval
		}
		time.Sleep(interval)
	}()
}

// buildConverseInput maps a ChatRequest onto Converse's structured
// input. Translation rules:
//
//   - role="system" → top-level System []SystemContentBlock (the
//     Messages list never carries system entries).
//   - role="user" → user Message with a text ContentBlock.
//   - role="assistant" → assistant Message with optional text +
//     ToolUse blocks for any tool_calls.
//   - role="tool" → buffered as ToolResult blocks; flushed as a single
//     user Message (Bedrock has no "tool" role).
//   - Tools translate to ToolConfiguration; the OpenAI JSON Schema in
//     ToolDef.Function.Parameters is wrapped verbatim as a
//     ToolInputSchemaMemberJson document.
func buildConverseInput(req ChatRequest, model string, maxTokens int64) (*bedrockruntime.ConverseStreamInput, error) {
	if model == "" {
		return nil, errors.New("model is required")
	}
	if maxTokens == 0 {
		maxTokens = defaultBedrockMaxTokens
	}
	req.Messages = FilterForRequest(req.Messages)

	var system []types.SystemContentBlock
	var messages []types.Message

	// Consecutive tool messages collapse into one user Message — Bedrock
	// expects a single user message containing every ToolResult block
	// from a parallel tool-call round.
	var pendingToolResults []types.ContentBlock
	flushToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		messages = append(messages, types.Message{
			Role:    types.ConversationRoleUser,
			Content: pendingToolResults,
		})
		pendingToolResults = nil
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if m.Content != "" {
				system = append(system, &types.SystemContentBlockMemberText{Value: m.Content})
			}

		case "user":
			flushToolResults()
			content, err := bedrockUserContent(m)
			if err != nil {
				return nil, err
			}
			if len(content) == 0 {
				continue
			}
			messages = append(messages, types.Message{
				Role:    types.ConversationRoleUser,
				Content: content,
			})

		case "assistant":
			flushToolResults()
			var content []types.ContentBlock
			if m.Content != "" {
				content = append(content, &types.ContentBlockMemberText{Value: m.Content})
			}
			for _, p := range m.Parts {
				blk, err := bedrockContentBlock(p)
				if err != nil {
					return nil, err
				}
				content = append(content, blk)
			}
			for _, tc := range m.ToolCalls {
				var arg interface{}
				if tc.Function.Arguments != "" {
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &arg); err != nil {
						return nil, fmt.Errorf("tool call %q: arguments: %w", tc.Function.Name, err)
					}
				} else {
					arg = map[string]interface{}{}
				}
				id := tc.ID
				name := tc.Function.Name
				content = append(content, &types.ContentBlockMemberToolUse{
					Value: types.ToolUseBlock{
						ToolUseId: &id,
						Name:      &name,
						Input:     document.NewLazyDocument(arg),
					},
				})
			}
			if len(content) == 0 {
				continue
			}
			messages = append(messages, types.Message{
				Role:    types.ConversationRoleAssistant,
				Content: content,
			})

		case "tool":
			id := m.ToolCallID
			resultContent, err := bedrockToolResultContent(m)
			if err != nil {
				return nil, err
			}
			pendingToolResults = append(pendingToolResults, &types.ContentBlockMemberToolResult{
				Value: types.ToolResultBlock{
					ToolUseId: &id,
					Content:   resultContent,
				},
			})
		}
	}
	flushToolResults()

	input := &bedrockruntime.ConverseStreamInput{
		ModelId:  &model,
		Messages: messages,
		System:   system,
	}

	if len(req.Tools) > 0 {
		tools := make([]types.Tool, 0, len(req.Tools))
		for _, t := range req.Tools {
			name := t.Function.Name
			desc := t.Function.Description
			spec := types.ToolSpecification{
				Name:        &name,
				InputSchema: &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(t.Function.Parameters)},
			}
			if desc != "" {
				spec.Description = &desc
			}
			tools = append(tools, &types.ToolMemberToolSpec{Value: spec})
		}
		input.ToolConfig = &types.ToolConfiguration{Tools: tools}
	}

	// Inference config. Only fields the request actually provides get
	// set — Bedrock fills the rest with model-appropriate defaults.
	mt := int32(maxTokens)
	inf := &types.InferenceConfiguration{MaxTokens: &mt}
	if req.Temperature != 0 {
		t := float32(req.Temperature)
		inf.Temperature = &t
	}
	if req.TopP != 0 {
		p := float32(req.TopP)
		inf.TopP = &p
	}
	input.InferenceConfig = inf

	return input, nil
}

// applyExtendedThinking wires Claude's extended-thinking config onto
// a Converse request. Constraints (Anthropic-enforced):
//
//   - temperature must be exactly 1
//   - top_p must NOT be set
//   - budget must be ≥ 1024 and < max_tokens
//
// We clamp silently rather than fail loudly: the user's intent was
// "enable thinking", and a mid-stream 400 from a clamp violation is
// strictly worse than a successful response with a slightly adjusted
// budget. Reasoning surfaces via EventReasoningDelta — same channel
// the TUI already renders for OpenAI reasoning models, ephemeral
// (not persisted into assistant message history).
func applyExtendedThinking(input *bedrockruntime.ConverseStreamInput, budget int64) {
	if budget == 0 {
		budget = defaultThinkingBudget
	}
	if budget < minThinkingBudget {
		budget = minThinkingBudget
	}
	if input.InferenceConfig != nil && input.InferenceConfig.MaxTokens != nil {
		if max := int64(*input.InferenceConfig.MaxTokens); budget >= max {
			budget = max - 1
		}
	}

	// Force temp=1, clear top_p — Anthropic rejects anything else.
	if input.InferenceConfig == nil {
		input.InferenceConfig = &types.InferenceConfiguration{}
	}
	t := float32(1.0)
	input.InferenceConfig.Temperature = &t
	input.InferenceConfig.TopP = nil

	// Bedrock surfaces thinking via additionalModelRequestFields with
	// the same `thinking` schema as api.anthropic.com.
	input.AdditionalModelRequestFields = document.NewLazyDocument(map[string]interface{}{
		"thinking": map[string]interface{}{
			"type":          "enabled",
			"budget_tokens": budget,
		},
	})
}

// applyBedrockCachePoints appends CachePoint markers after the system
// content, after the last tool spec, and at the tail of the last 1–2
// conversation messages — same logical effect as the Anthropic-native
// cache_control markers, just expressed in Converse's shape. Bedrock
// caches the prefix up to and including each marker.
//
// No-op when there's nothing to mark. Stays within Bedrock's 4-marker
// per-request limit (we spend at most 4: system + tools + 2 messages).
func applyBedrockCachePoints(input *bedrockruntime.ConverseStreamInput) {
	const maxMarkers = 4
	used := 0

	if len(input.System) > 0 && used < maxMarkers {
		input.System = append(input.System, &types.SystemContentBlockMemberCachePoint{
			Value: types.CachePointBlock{Type: types.CachePointTypeDefault},
		})
		used++
	}
	if input.ToolConfig != nil && len(input.ToolConfig.Tools) > 0 && used < maxMarkers {
		input.ToolConfig.Tools = append(input.ToolConfig.Tools, &types.ToolMemberCachePoint{
			Value: types.CachePointBlock{Type: types.CachePointTypeDefault},
		})
		used++
	}
	// Append CachePoint blocks at the end of the last 1–2 conversation
	// messages. Same intent as the Anthropic-side trailing markers: keep
	// the prefix up through the prior turn cacheable so a small
	// follow-up still benefits from the warm cache.
	remaining := maxMarkers - used
	if remaining > 2 {
		remaining = 2
	}
	for i := 0; i < remaining; i++ {
		idx := len(input.Messages) - 1 - i
		if idx < 0 {
			break
		}
		input.Messages[idx].Content = append(input.Messages[idx].Content,
			&types.ContentBlockMemberCachePoint{
				Value: types.CachePointBlock{Type: types.CachePointTypeDefault},
			})
	}
}

// bedrockGuardrailHeaders maps the user-facing trace value
// ("enabled"/"disabled"/"enabled_full") onto the X-Amzn-Bedrock-*
// header set that Bedrock InvokeModel uses to apply a guardrail.
// Shared with AnthropicBedrockClient so the same `bedrock_guardrail_*`
// config keys behave the same across both Bedrock paths even though
// the wire shape differs (structured field for Converse, headers for
// InvokeModel).
//
// Returns nil headers when id is empty so callers can unconditionally
// call this and only iterate when there's something to add.
func bedrockGuardrailHeaders(id, version, trace string) (map[string]string, error) {
	if id == "" {
		return nil, nil
	}
	if version == "" {
		return nil, errors.New("guardrail version is required (e.g. \"DRAFT\" or a numeric version)")
	}
	t := strings.ToLower(strings.TrimSpace(trace))
	if t == "" {
		t = "enabled"
	}
	var headerTrace string
	switch t {
	case "enabled", "enabled_full":
		// InvokeModel's header only distinguishes ENABLED vs DISABLED;
		// enabled_full collapses to ENABLED (extra detail is a Converse-
		// only concept). Documented behaviour, not silent.
		headerTrace = "ENABLED"
	case "disabled":
		headerTrace = "DISABLED"
	default:
		return nil, fmt.Errorf("unknown trace %q (want enabled / disabled / enabled_full)", trace)
	}
	return map[string]string{
		"X-Amzn-Bedrock-GuardrailIdentifier": id,
		"X-Amzn-Bedrock-GuardrailVersion":    version,
		"X-Amzn-Bedrock-Trace":               headerTrace,
	}, nil
}

// applyBedrockGuardrail wires the GuardrailStreamConfiguration onto a
// ConverseStream request. Validates the trace value up front so a
// typo'd config doesn't 400 the whole turn mid-stream — same
// fail-loud-at-translate-time stance as the rest of the build path.
//
// Defaults: empty trace → "enabled" (cheap; surfaces violations in
// CloudWatch). Stream-processing mode is left unset; the SDK defaults
// to "sync", which is what users expect (the response waits for the
// guardrail evaluation rather than streaming through and back-filling
// blocked content).
func applyBedrockGuardrail(input *bedrockruntime.ConverseStreamInput, id, version, trace string) error {
	if id == "" {
		return errors.New("guardrail id is required")
	}
	if version == "" {
		return errors.New("guardrail version is required (e.g. \"DRAFT\" or a numeric version)")
	}
	t := strings.ToLower(strings.TrimSpace(trace))
	if t == "" {
		t = string(types.GuardrailTraceEnabled)
	}
	var resolved types.GuardrailTrace
	switch t {
	case "enabled":
		resolved = types.GuardrailTraceEnabled
	case "disabled":
		resolved = types.GuardrailTraceDisabled
	case "enabled_full":
		resolved = types.GuardrailTraceEnabledFull
	default:
		return fmt.Errorf("unknown trace %q (want enabled / disabled / enabled_full)", trace)
	}

	input.GuardrailConfig = &types.GuardrailStreamConfiguration{
		GuardrailIdentifier: &id,
		GuardrailVersion:    &version,
		Trace:               resolved,
	}
	return nil
}

// bedrockUserContent builds the ContentBlock slice for a user-role
// message. Empty Parts collapses to the single-text-block legacy shape
// so existing flows are byte-identical on the wire.
func bedrockUserContent(m Message) ([]types.ContentBlock, error) {
	if len(m.Parts) == 0 {
		if m.Content == "" {
			return nil, nil
		}
		return []types.ContentBlock{&types.ContentBlockMemberText{Value: m.Content}}, nil
	}
	out := make([]types.ContentBlock, 0, len(m.Parts)+1)
	if m.Content != "" {
		out = append(out, &types.ContentBlockMemberText{Value: m.Content})
	}
	for _, p := range m.Parts {
		blk, err := bedrockContentBlock(p)
		if err != nil {
			return nil, err
		}
		out = append(out, blk)
	}
	return out, nil
}

// bedrockContentBlock translates one MessagePart onto a Converse
// ContentBlock. Bedrock requires raw bytes — URI-only images return an
// error rather than getting silently dropped. Text parts fall through
// to ContentBlockMemberText for callers that mix text + image in Parts.
func bedrockContentBlock(p MessagePart) (types.ContentBlock, error) {
	switch p.Type {
	case "text":
		return &types.ContentBlockMemberText{Value: p.Text}, nil
	case "image":
		if len(p.Data) == 0 {
			return nil, fmt.Errorf("bedrock: image parts must carry inline bytes (URIs not supported by Converse)")
		}
		fmt, err := bedrockImageFormat(p.MIMEType)
		if err != nil {
			return nil, err
		}
		return &types.ContentBlockMemberImage{
			Value: types.ImageBlock{
				Format: fmt,
				Source: &types.ImageSourceMemberBytes{Value: p.Data},
			},
		}, nil
	case "document":
		return nil, fmt.Errorf("bedrock: document parts not yet supported by this adapter")
	default:
		return nil, fmt.Errorf("bedrock: unknown part type %q", p.Type)
	}
}

// bedrockToolResultContent maps a role="tool" Message onto the
// ToolResult-side content union. Same legacy fall-through: text-only
// stays single-block; Parts opt into image content for tools that
// return non-text (the read tool on a PNG, etc.).
func bedrockToolResultContent(m Message) ([]types.ToolResultContentBlock, error) {
	if len(m.Parts) == 0 {
		return []types.ToolResultContentBlock{
			&types.ToolResultContentBlockMemberText{Value: m.Content},
		}, nil
	}
	out := make([]types.ToolResultContentBlock, 0, len(m.Parts)+1)
	if m.Content != "" {
		out = append(out, &types.ToolResultContentBlockMemberText{Value: m.Content})
	}
	for _, p := range m.Parts {
		switch p.Type {
		case "text":
			out = append(out, &types.ToolResultContentBlockMemberText{Value: p.Text})
		case "image":
			if len(p.Data) == 0 {
				return nil, fmt.Errorf("bedrock: tool_result image needs inline bytes")
			}
			fmt, err := bedrockImageFormat(p.MIMEType)
			if err != nil {
				return nil, err
			}
			out = append(out, &types.ToolResultContentBlockMemberImage{
				Value: types.ImageBlock{
					Format: fmt,
					Source: &types.ImageSourceMemberBytes{Value: p.Data},
				},
			})
		default:
			return nil, fmt.Errorf("bedrock: tool_result %q parts unsupported", p.Type)
		}
	}
	return out, nil
}

// bedrockImageFormat maps an IANA MIME type to the AWS SDK's
// ImageFormat enum. Bedrock Converse accepts PNG / JPEG / GIF / WebP;
// anything else fails loud rather than getting silently dropped or
// truncated to a wrong format.
func bedrockImageFormat(mime string) (types.ImageFormat, error) {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png":
		return types.ImageFormatPng, nil
	case "image/jpeg", "image/jpg":
		return types.ImageFormatJpeg, nil
	case "image/gif":
		return types.ImageFormatGif, nil
	case "image/webp":
		return types.ImageFormatWebp, nil
	default:
		return "", fmt.Errorf("bedrock: unsupported image mime %q (want png/jpeg/gif/webp)", mime)
	}
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

// bedrockUsageFrom translates Bedrock Converse usage counts to a
// MessageUsage. Pulled out of the Metadata handler so the translation
// can be unit-tested without driving a live stream.
//
// Bedrock Converse mirrors Anthropic's semantics: InputTokens is
// fresh-only (cache reads and writes are reported separately), so
// summing all four gives the authoritative total.
func bedrockUsageFrom(input, output, cacheRead, cacheWrite int32) MessageUsage {
	u := MessageUsage{
		InputTokens:      int(input),
		OutputTokens:     int(output),
		CacheReadTokens:  int(cacheRead),
		CacheWriteTokens: int(cacheWrite),
	}
	u.TotalTokens = u.InputTokens + u.OutputTokens +
		u.CacheReadTokens + u.CacheWriteTokens
	return u
}
