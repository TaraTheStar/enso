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

			case *types.ConverseStreamOutputMemberMessageStart,
				*types.ConverseStreamOutputMemberMessageStop,
				*types.ConverseStreamOutputMemberMetadata:
				// MessageStart carries role (always assistant for our
				// usage); MessageStop carries stop_reason; Metadata
				// carries token usage. None feed the event channel
				// today — usage accounting hooks would land here.
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
			if m.Content == "" {
				continue
			}
			messages = append(messages, types.Message{
				Role:    types.ConversationRoleUser,
				Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: m.Content}},
			})

		case "assistant":
			flushToolResults()
			var content []types.ContentBlock
			if m.Content != "" {
				content = append(content, &types.ContentBlockMemberText{Value: m.Content})
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
			pendingToolResults = append(pendingToolResults, &types.ContentBlockMemberToolResult{
				Value: types.ToolResultBlock{
					ToolUseId: &id,
					Content: []types.ToolResultContentBlock{
						&types.ToolResultContentBlockMemberText{Value: m.Content},
					},
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
