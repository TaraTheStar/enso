// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sync"
	"time"

	"google.golang.org/genai"
)

// defaultVertexMaxTokens is the cap applied when ProviderConfig leaves
// max_tokens at zero. Gemini's API accepts an unset MaxOutputTokens
// (model picks its own default) but a deterministic ceiling keeps
// behaviour comparable to the Bedrock adapter.
const defaultVertexMaxTokens = 8192

// defaultVertexLocation is the region used when GCPLocation is empty.
// us-central1 hosts every Gemini variant Vertex offers; users wanting
// regional residency override via gcp_location.
const defaultVertexLocation = "us-central1"

// VertexClient routes ChatRequests through Vertex AI's generateContent
// API via the unified google.golang.org/genai SDK.
//
// Scope: Gemini family (gemini-2.5-pro, gemini-2.5-flash, gemini-2.0-*,
// gemini-1.5-*) plus the open Gemma variants reachable through the same
// endpoint. Anthropic Claude on Vertex AI uses a distinct :rawPredict
// shape — that lives on the parked anthropic adapter, not here.
//
// Authentication follows Google Application Default Credentials:
// GOOGLE_APPLICATION_CREDENTIALS env var, `gcloud auth application-
// default login` on the workstation, or the GCE/GKE/Cloud Run metadata
// server in deployed environments.
type VertexClient struct {
	Model    string
	Project  string
	Location string

	// MaxTokens maps to GenerateContentConfig.MaxOutputTokens. Zero
	// uses defaultVertexMaxTokens.
	MaxTokens int64

	// ExtendedThinking enables Gemini 2.5's thinking output. When true
	// the SDK sets IncludeThoughts=true so the model returns Thought
	// parts; we route those to EventReasoningDelta — the same channel
	// the TUI already renders for OpenAI reasoning models and Claude
	// extended-thinking on Bedrock. Silently ignored by models that
	// don't expose thinking.
	ExtendedThinking bool

	// ExtendedThinkingBudget is the thinking-token budget. Zero leaves
	// Gemini's dynamic mode in effect; positive values pin the budget.
	// No floor/ceiling clamp — Gemini accepts the full int32 range and
	// (-1) for dynamic, both unlike Anthropic's [1024, max_tokens)
	// constraint.
	ExtendedThinkingBudget int64

	// ProbeInterval is a test seam. Zero uses the package default.
	ProbeInterval time.Duration

	conn connTracker

	sdkOnce sync.Once
	sdk     vertexGenerateAPI
	sdkErr  error

	// newClient is a test seam — production leaves it nil.
	newClient func(ctx context.Context, project, location string) (vertexGenerateAPI, error)
}

// vertexGenerateAPI narrows the SDK surface to the one streaming
// operation we use. Mirrors bedrockConverseAPI in spirit so tests can
// drive the loop without standing up a real Vertex endpoint.
type vertexGenerateAPI interface {
	GenerateContentStream(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error]
}

// LLMConnState satisfies ConnStateReporter.
func (c *VertexClient) LLMConnState() ConnState { return c.conn.get() }

func (c *VertexClient) client(ctx context.Context) (vertexGenerateAPI, error) {
	c.sdkOnce.Do(func() {
		factory := c.newClient
		if factory == nil {
			factory = newVertexClient
		}
		c.sdk, c.sdkErr = factory(ctx, c.Project, c.Location)
	})
	return c.sdk, c.sdkErr
}

func newVertexClient(ctx context.Context, project, location string) (vertexGenerateAPI, error) {
	if location == "" {
		location = defaultVertexLocation
	}
	cli, err := genai.NewClient(ctx, &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  project,
		Location: location,
	})
	if err != nil {
		return nil, fmt.Errorf("genai client: %w", err)
	}
	return vertexModelsAdapter{cli.Models}, nil
}

// vertexModelsAdapter is the tiny shim that lifts the SDK's value-
// receiver GenerateContentStream onto our interface. Avoids a direct
// dependency on *genai.Models everywhere a vertexGenerateAPI is wanted.
type vertexModelsAdapter struct{ m *genai.Models }

func (a vertexModelsAdapter) GenerateContentStream(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	return a.m.GenerateContentStream(ctx, model, contents, config)
}

// Chat translates an OpenAI-shaped ChatRequest into Vertex generate-
// content inputs, opens the streaming iterator, and forwards events
// onto the adapter-agnostic Event channel.
func (c *VertexClient) Chat(ctx context.Context, req ChatRequest) (<-chan Event, error) {
	model := c.Model
	if model == "" {
		model = req.Model
	}
	if model == "" {
		return nil, errors.New("vertex: model is required")
	}

	contents, cfg, err := buildVertexRequest(req, c.MaxTokens)
	if err != nil {
		return nil, fmt.Errorf("vertex: build request: %w", err)
	}
	if c.ExtendedThinking {
		applyVertexThinking(cfg, c.ExtendedThinkingBudget)
	}

	sdk, err := c.client(ctx)
	if err != nil {
		return nil, fmt.Errorf("vertex: %w", err)
	}

	fmt.Fprintf(debugLog(), "vertex: GenerateContentStream model=%s project=%s location=%s\n", model, c.Project, c.Location)

	eventCh := make(chan Event, 32)
	c.conn.set(StateConnected)

	go func() {
		defer close(eventCh)

		stream := sdk.GenerateContentStream(ctx, model, contents, cfg)
		var streamErr error

		stream(func(resp *genai.GenerateContentResponse, err error) bool {
			if err != nil {
				streamErr = err
				return false
			}
			for _, cand := range resp.Candidates {
				if cand == nil || cand.Content == nil {
					continue
				}
				for _, part := range cand.Content.Parts {
					if part == nil {
						continue
					}
					switch {
					case part.FunctionCall != nil:
						fc := part.FunctionCall
						args, mErr := json.Marshal(fc.Args)
						if mErr != nil {
							eventCh <- Event{Type: EventError, Error: fmt.Errorf("vertex: marshal function call args: %w", mErr)}
							return false
						}
						// Gemini omits an explicit id for most calls;
						// synthesise one so downstream code can match
						// tool results back to this call.
						id := fc.ID
						if id == "" {
							id = "call_" + fc.Name
						}
						tc := ToolCall{ID: id, Type: "function"}
						tc.Function.Name = fc.Name
						tc.Function.Arguments = string(args)
						eventCh <- Event{Type: EventToolCallComplete, ToolCalls: []ToolCall{tc}}

					case part.Text != "":
						if part.Thought {
							eventCh <- Event{Type: EventReasoningDelta, Text: part.Text}
						} else {
							eventCh <- Event{Type: EventTextDelta, Text: part.Text}
						}
					}
				}
			}
			return true
		})

		if streamErr != nil {
			// First-call transport failures flip the indicator. Errors
			// surfaced mid-stream (quota, content-block, etc.) leave it
			// alone — TCP+TLS were already healthy.
			if classifyTransportError(streamErr) != "" {
				if c.conn.set(StateDisconnected) != StateDisconnected {
					c.startRecoveryProbe()
				}
			}
			eventCh <- Event{Type: EventError, Error: fmt.Errorf("vertex: %w", streamErr)}
			return
		}
		eventCh <- Event{Type: EventDone}
	}()

	return eventCh, nil
}

// startRecoveryProbe is a stub for the same reason Bedrock's is: there
// is no cheap reachability check that exercises the same IAM scope as
// GenerateContent. Hold the claim for one interval, then release; the
// next user-driven Chat decides recovery.
func (c *VertexClient) startRecoveryProbe() {
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

// buildVertexRequest maps a ChatRequest onto Vertex's generate-content
// inputs. Rules:
//
//   - role="system" → top-level SystemInstruction (multiple system
//     messages concatenate text parts).
//   - role="user" text → Content{Role: RoleUser}.
//   - role="assistant" text → Content{Role: RoleModel}.
//   - role="assistant" tool_calls → Content{Role: RoleModel,
//     Parts: [FunctionCall{Name, Args}]}.
//   - role="tool" → Content{Role: RoleUser,
//     Parts: [FunctionResponse{Name, Response}]}. The function name is
//     recovered from the preceding assistant message's tool_calls by
//     ToolCallID — Gemini matches responses to calls by name, not id.
//   - Tools → []*Tool{{FunctionDeclarations: ...}}. JSON Schema
//     parameters pass through verbatim via ParametersJsonSchema so the
//     OpenAI schema's nuances (oneOf, additionalProperties, etc.) reach
//     the model unchanged.
func buildVertexRequest(req ChatRequest, maxTokens int64) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	if maxTokens == 0 {
		maxTokens = defaultVertexMaxTokens
	}

	// First pass: index tool_call IDs → function names so role="tool"
	// messages can recover the name Gemini needs.
	idToName := map[string]string{}
	for _, m := range req.Messages {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && tc.Function.Name != "" {
				idToName[tc.ID] = tc.Function.Name
			}
		}
	}

	var sysParts []*genai.Part
	var contents []*genai.Content

	// Consecutive tool messages collapse into a single user Content
	// holding all FunctionResponse parts — mirrors how Bedrock batches
	// ToolResult blocks and matches the multi-tool-call turn the agent
	// loop emits.
	var pendingToolResponses []*genai.Part
	flushToolResponses := func() {
		if len(pendingToolResponses) == 0 {
			return
		}
		contents = append(contents, &genai.Content{
			Role:  genai.RoleUser,
			Parts: pendingToolResponses,
		})
		pendingToolResponses = nil
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if m.Content != "" {
				sysParts = append(sysParts, genai.NewPartFromText(m.Content))
			}

		case "user":
			flushToolResponses()
			if m.Content == "" {
				continue
			}
			contents = append(contents, &genai.Content{
				Role:  genai.RoleUser,
				Parts: []*genai.Part{genai.NewPartFromText(m.Content)},
			})

		case "assistant":
			flushToolResponses()
			var parts []*genai.Part
			if m.Content != "" {
				parts = append(parts, genai.NewPartFromText(m.Content))
			}
			for _, tc := range m.ToolCalls {
				args := map[string]any{}
				if tc.Function.Arguments != "" {
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
						return nil, nil, fmt.Errorf("tool call %q: arguments: %w", tc.Function.Name, err)
					}
				}
				parts = append(parts, genai.NewPartFromFunctionCall(tc.Function.Name, args))
			}
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, &genai.Content{
				Role:  genai.RoleModel,
				Parts: parts,
			})

		case "tool":
			name := idToName[m.ToolCallID]
			if name == "" {
				// Fall back to Name field if the OpenAI-shape sender
				// populated it; otherwise the API will reject the
				// orphan response. We still emit it so the error
				// surfaces with the model's reason rather than getting
				// silently dropped here.
				name = m.Name
			}
			// Gemini's FunctionResponse expects a structured payload.
			// We pass the tool's textual output under "content" so the
			// model receives the raw bytes the OpenAI flow would. If
			// the tool returned JSON, we forward it parsed; otherwise
			// wrap as a string.
			var payload map[string]any
			if m.Content != "" {
				var parsed any
				if err := json.Unmarshal([]byte(m.Content), &parsed); err == nil {
					switch v := parsed.(type) {
					case map[string]any:
						payload = v
					default:
						payload = map[string]any{"content": v}
					}
				} else {
					payload = map[string]any{"content": m.Content}
				}
			} else {
				payload = map[string]any{}
			}
			pendingToolResponses = append(pendingToolResponses, genai.NewPartFromFunctionResponse(name, payload))
		}
	}
	flushToolResponses()

	cfg := &genai.GenerateContentConfig{}
	if len(sysParts) > 0 {
		cfg.SystemInstruction = &genai.Content{Parts: sysParts}
	}

	mt := int32(maxTokens)
	cfg.MaxOutputTokens = mt

	if req.Temperature != 0 {
		t := float32(req.Temperature)
		cfg.Temperature = &t
	}
	if req.TopP != 0 {
		p := float32(req.TopP)
		cfg.TopP = &p
	}
	if req.TopK != 0 {
		k := float32(req.TopK)
		cfg.TopK = &k
	}

	if len(req.Tools) > 0 {
		decls := make([]*genai.FunctionDeclaration, 0, len(req.Tools))
		for _, t := range req.Tools {
			decls = append(decls, &genai.FunctionDeclaration{
				Name:                 t.Function.Name,
				Description:          t.Function.Description,
				ParametersJsonSchema: t.Function.Parameters,
			})
		}
		cfg.Tools = []*genai.Tool{{FunctionDeclarations: decls}}
	}

	return contents, cfg, nil
}

// applyVertexThinking enables Gemini 2.5's thinking output. Unlike
// Anthropic's extended-thinking, Gemini has no temperature/top_p
// constraints — we just turn IncludeThoughts on so Thought parts come
// back, and optionally pin ThinkingBudget. A budget of zero leaves
// Gemini's default dynamic behaviour intact.
func applyVertexThinking(cfg *genai.GenerateContentConfig, budget int64) {
	if cfg.ThinkingConfig == nil {
		cfg.ThinkingConfig = &genai.ThinkingConfig{}
	}
	cfg.ThinkingConfig.IncludeThoughts = true
	if budget > 0 {
		b := int32(budget)
		cfg.ThinkingConfig.ThinkingBudget = &b
	}
}
