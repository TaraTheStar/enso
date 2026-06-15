// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
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

	// Safety pins per-category HarmBlockThreshold values on every
	// request. Keys are short category names ("hate_speech",
	// "harassment", "dangerous_content", "sexually_explicit",
	// "civic_integrity"); values are the threshold enums (
	// "BLOCK_NONE", "BLOCK_LOW_AND_ABOVE", "BLOCK_MEDIUM_AND_ABOVE",
	// "BLOCK_ONLY_HIGH", "OFF"). Empty map leaves Gemini's defaults
	// in effect; unknown keys/values fail at translate-time.
	Safety map[string]string

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
	if len(c.Safety) > 0 {
		if err := applyVertexSafety(cfg, c.Safety); err != nil {
			return nil, fmt.Errorf("vertex: safety: %w", err)
		}
	}

	sdk, err := c.client(ctx)
	if err != nil {
		return nil, fmt.Errorf("vertex: %w", err)
	}

	if debugEnabled.Load() {
		fmt.Fprintf(debugLog(), "vertex: GenerateContentStream model=%s project=%s location=%s\n", model, c.Project, c.Location)
	}

	eventCh := make(chan Event, 32)
	c.conn.set(StateConnected)

	go func() {
		defer close(eventCh)

		stream := sdk.GenerateContentStream(ctx, model, contents, cfg)
		var streamErr error
		// Gemini reports a running UsageMetadata on each chunk; the
		// final chunk carries the authoritative numbers. Keep the
		// latest seen and emit once at end-of-stream.
		var lastUsage MessageUsage
		var sawUsage bool
		// Monotonic index across the whole stream, used to disambiguate
		// synthesised tool-call IDs. Gemini omits ids and two parallel
		// calls to the SAME tool would otherwise both get `call_<name>`,
		// colliding so a tool result can't be matched to the right call
		// (and breaking compaction-boundary matching / cross-provider
		// /model swap). Suffixing with this index keeps them distinct.
		toolCallIdx := 0
		// Track the terminal finish/block reasons so a blocked or
		// truncated turn doesn't end as a silent, empty EventDone. The
		// candidate's FinishReason is authoritative; PromptFeedback's
		// BlockReason fires when the *prompt itself* was rejected (no
		// candidate at all).
		var finishReason genai.FinishReason
		var blockReason genai.BlockedReason

		stream(func(resp *genai.GenerateContentResponse, err error) bool {
			if err != nil {
				streamErr = err
				return false
			}
			if pf := resp.PromptFeedback; pf != nil && pf.BlockReason != "" {
				blockReason = pf.BlockReason
			}
			if u := resp.UsageMetadata; u != nil && (u.PromptTokenCount > 0 || u.CachedContentTokenCount > 0) {
				// Gemini 2.5+ reports CachedContentTokenCount when
				// implicit caching hits; log for the debug stream and
				// stash for emission below.
				if debugEnabled.Load() {
					fmt.Fprintf(debugLog(), "vertex usage: prompt=%d candidates=%d cached=%d total=%d\n",
						u.PromptTokenCount, u.CandidatesTokenCount,
						u.CachedContentTokenCount, u.TotalTokenCount)
				}
				lastUsage = vertexUsageFrom(
					u.PromptTokenCount, u.CandidatesTokenCount,
					u.CachedContentTokenCount, u.TotalTokenCount,
				)
				sawUsage = true
			}
			for _, cand := range resp.Candidates {
				if cand == nil {
					continue
				}
				if cand.FinishReason != "" {
					finishReason = cand.FinishReason
				}
				if cand.Content == nil {
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
						// tool results back to this call. The index suffix
						// keeps parallel same-tool calls distinct (see
						// toolCallIdx above).
						id := synthVertexToolCallID(fc.ID, fc.Name, toolCallIdx)
						toolCallIdx++
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
		if sawUsage {
			eventCh <- Event{Type: EventUsage, Usage: lastUsage}
		}
		// A prompt-level block (no candidate) or a content-block finish
		// reason (SAFETY/RECITATION/…) must surface as an error — otherwise
		// the turn ends as a clean but empty EventDone and the agent loops
		// on an assistant message it can't explain.
		if msg := vertexBlockMessage(finishReason, blockReason); msg != "" {
			eventCh <- Event{Type: EventError, Error: fmt.Errorf("vertex: %s", msg)}
			return
		}
		eventCh <- Event{Type: EventDone, FinishReason: mapVertexFinishReason(finishReason)}
	}()

	return eventCh, nil
}

// vertexBlockMessage returns a human-readable reason when the turn was
// blocked (by content safety, recitation, a blocklist, etc.) and so
// produced no usable output, or "" when it finished normally (STOP,
// MAX_TOKENS, tool call, …). Prompt-level blocks (BlockReason, no
// candidate) take precedence over the candidate finish reason.
func vertexBlockMessage(fr genai.FinishReason, br genai.BlockedReason) string {
	switch br {
	case "", genai.BlockedReasonUnspecified:
		// no prompt-level block; fall through to the finish reason
	default:
		return "prompt blocked (" + string(br) + ")"
	}
	switch fr {
	case genai.FinishReasonSafety:
		return "response blocked by safety filters"
	case genai.FinishReasonRecitation:
		return "response blocked for recitation (cited/copyrighted content)"
	case genai.FinishReasonBlocklist:
		return "response blocked by a terminology blocklist"
	case genai.FinishReasonProhibitedContent:
		return "response blocked: prohibited content"
	case genai.FinishReasonSPII:
		return "response blocked: sensitive personally identifiable information"
	case genai.FinishReasonImageSafety:
		return "response blocked: image safety"
	case genai.FinishReasonMalformedFunctionCall:
		return "model emitted a malformed function call"
	}
	return ""
}

// mapVertexFinishReason maps a Gemini FinishReason onto the adapter's
// shared finish-reason vocabulary (see openai.go) so the agent's
// auto-recovery can engage. Most relevant: MAX_TOKENS → FinishLength,
// which lets a truncated Vertex turn be continued just like an OpenAI
// one. Block reasons are handled by vertexBlockMessage (EventError), not
// here. Anything else maps to the empty string ("normal stop").
func mapVertexFinishReason(fr genai.FinishReason) string {
	if fr == genai.FinishReasonMaxTokens {
		return FinishLength
	}
	return ""
}

// synthVertexToolCallID returns the tool-call ID to emit for a Gemini
// function-call part. Gemini supplies an explicit id only rarely; when
// it's empty we synthesise `call_<name>_<idx>` where idx is a per-stream
// monotonic counter. The idx is what makes two PARALLEL calls to the
// same tool distinct — without it both would be `call_<name>` and the
// tool results couldn't be matched back to the right call. A provided
// id is always trusted verbatim.
func synthVertexToolCallID(provided, name string, idx int) string {
	if provided != "" {
		return provided
	}
	return fmt.Sprintf("call_%s_%d", name, idx)
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
	req.Messages = FilterForRequest(req.Messages)

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
			parts, err := vertexUserParts(m)
			if err != nil {
				return nil, nil, err
			}
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, &genai.Content{
				Role:  genai.RoleUser,
				Parts: parts,
			})

		case "assistant":
			flushToolResponses()
			var parts []*genai.Part
			if m.Content != "" {
				parts = append(parts, genai.NewPartFromText(m.Content))
			}
			for _, p := range m.Parts {
				vp, err := vertexPart(p)
				if err != nil {
					return nil, nil, err
				}
				parts = append(parts, vp)
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
			frPart := genai.NewPartFromFunctionResponse(name, payload)

			// When the tool returned image bytes, attach them as
			// FunctionResponse.Parts. Gemini routes those through to
			// the model alongside the textual payload — that's the
			// equivalent of Bedrock's ToolResultContentBlockMemberImage
			// and Anthropic's tool_result-with-image.
			imageParts, err := vertexFunctionResponseImageParts(m)
			if err != nil {
				return nil, nil, err
			}
			if len(imageParts) > 0 && frPart.FunctionResponse != nil {
				frPart.FunctionResponse.Parts = imageParts
			}
			pendingToolResponses = append(pendingToolResponses, frPart)
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

// vertexUserParts builds the Part slice for a user-role message.
// Empty Parts collapses to the legacy single-text-part path so the
// wire shape stays identical when no multimodal content is involved.
func vertexUserParts(m Message) ([]*genai.Part, error) {
	if len(m.Parts) == 0 {
		if m.Content == "" {
			return nil, nil
		}
		return []*genai.Part{genai.NewPartFromText(m.Content)}, nil
	}
	out := make([]*genai.Part, 0, len(m.Parts)+1)
	if m.Content != "" {
		out = append(out, genai.NewPartFromText(m.Content))
	}
	for _, p := range m.Parts {
		vp, err := vertexPart(p)
		if err != nil {
			return nil, err
		}
		out = append(out, vp)
	}
	return out, nil
}

// vertexPart translates one MessagePart onto a Gemini Part. Inline
// data uses NewPartFromBytes; URIs use NewPartFromURI (which Gemini
// resolves server-side, including gs:// paths). Documents share the
// same shape since Gemini doesn't separate image/document at the
// part level — just by MIME type.
func vertexPart(p MessagePart) (*genai.Part, error) {
	switch p.Type {
	case "text":
		return genai.NewPartFromText(p.Text), nil
	case "image", "document":
		if len(p.Data) > 0 {
			if p.MIMEType == "" {
				return nil, fmt.Errorf("vertex: %s part has data but no mime_type", p.Type)
			}
			return genai.NewPartFromBytes(p.Data, p.MIMEType), nil
		}
		if p.URI != "" {
			return genai.NewPartFromURI(p.URI, p.MIMEType), nil
		}
		return nil, fmt.Errorf("vertex: %s part has no data or uri", p.Type)
	default:
		return nil, fmt.Errorf("vertex: unknown part type %q", p.Type)
	}
}

// vertexFunctionResponseImageParts extracts image / document Parts
// from a tool-result Message and packages them as the
// FunctionResponsePart slice Gemini expects. Text Parts are NOT
// included here — they remain in the FunctionResponse.Response map
// via the existing payload path. Returns nil when no media parts are
// present (the common case), so the caller can skip the assignment.
func vertexFunctionResponseImageParts(m Message) ([]*genai.FunctionResponsePart, error) {
	if len(m.Parts) == 0 {
		return nil, nil
	}
	var out []*genai.FunctionResponsePart
	for _, p := range m.Parts {
		if p.Type != "image" && p.Type != "document" {
			continue
		}
		if len(p.Data) == 0 {
			return nil, fmt.Errorf("vertex: tool_result media part has no inline bytes")
		}
		if p.MIMEType == "" {
			return nil, fmt.Errorf("vertex: tool_result media part missing mime_type")
		}
		out = append(out, &genai.FunctionResponsePart{
			InlineData: &genai.FunctionResponseBlob{
				Data:     p.Data,
				MIMEType: p.MIMEType,
			},
		})
	}
	return out, nil
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

// vertexSafetyCategories is the user-facing → SDK enum mapping for
// Gemini's safety knobs. Listed in the order Gemini docs use; only
// these four categories are addressable per-request (image-modality
// variants exist but are output-only and not user-configurable;
// civic_integrity was removed when Google dropped the election
// filter — the SDK enum is deprecated and the API ignores it).
var vertexSafetyCategories = map[string]genai.HarmCategory{
	"hate_speech":       genai.HarmCategoryHateSpeech,
	"harassment":        genai.HarmCategoryHarassment,
	"dangerous_content": genai.HarmCategoryDangerousContent,
	"sexually_explicit": genai.HarmCategorySexuallyExplicit,
}

// vertexSafetyThresholds is the user-facing threshold value → SDK enum
// mapping. Accept the AWS-style uppercase plus lowercase aliases so the
// config doesn't require shouting. "OFF" disables the filter entirely;
// "BLOCK_NONE" still runs the classifier but never blocks (useful for
// trace / audit pipelines).
var vertexSafetyThresholds = map[string]genai.HarmBlockThreshold{
	"BLOCK_NONE":             genai.HarmBlockThresholdBlockNone,
	"BLOCK_LOW_AND_ABOVE":    genai.HarmBlockThresholdBlockLowAndAbove,
	"BLOCK_MEDIUM_AND_ABOVE": genai.HarmBlockThresholdBlockMediumAndAbove,
	"BLOCK_ONLY_HIGH":        genai.HarmBlockThresholdBlockOnlyHigh,
	"OFF":                    genai.HarmBlockThresholdOff,
}

// applyVertexSafety translates the user's short-name map onto the
// SDK's SafetySettings slice. Fails loud on unknown keys/values so a
// typo doesn't ship a silently-permissive config.
func applyVertexSafety(cfg *genai.GenerateContentConfig, settings map[string]string) error {
	if len(settings) == 0 {
		return nil
	}
	out := make([]*genai.SafetySetting, 0, len(settings))
	for k, v := range settings {
		cat, ok := vertexSafetyCategories[strings.ToLower(strings.TrimSpace(k))]
		if !ok {
			return fmt.Errorf("unknown category %q (want hate_speech / harassment / dangerous_content / sexually_explicit / civic_integrity)", k)
		}
		threshold, ok := vertexSafetyThresholds[strings.ToUpper(strings.TrimSpace(v))]
		if !ok {
			return fmt.Errorf("category %q: unknown threshold %q (want BLOCK_NONE / BLOCK_LOW_AND_ABOVE / BLOCK_MEDIUM_AND_ABOVE / BLOCK_ONLY_HIGH / OFF)", k, v)
		}
		out = append(out, &genai.SafetySetting{Category: cat, Threshold: threshold})
	}
	cfg.SafetySettings = out
	return nil
}

// vertexUsageFrom translates Vertex/Gemini UsageMetadata counts to a
// MessageUsage. Pulled out of the stream callback so the translation
// can be unit-tested without a live SDK iterator.
//
// Gemini semantics: TotalTokenCount is authoritative (do not
// recompute). CachedContentTokenCount is a sub-line of
// PromptTokenCount, not additive — same shape as OpenAI's
// prompt_tokens_details.cached_tokens.
func vertexUsageFrom(prompt, candidates, cached, total int32) MessageUsage {
	return MessageUsage{
		InputTokens:     int(prompt),
		OutputTokens:    int(candidates),
		CacheReadTokens: int(cached),
		TotalTokens:     int(total),
	}
}
