// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/vertex"
)

// AnthropicVertexClient routes Anthropic Messages API calls through GCP
// Vertex AI's `:rawPredict` endpoint instead of api.anthropic.com. Same
// wire protocol once the anthropic-sdk-go vertex middleware swaps the
// base URL and signs requests with Google Application Default
// Credentials, so it reuses every translator + the streaming loop from
// AnthropicClient.
//
// Distinct from VertexClient (`type = "vertex"`), which talks the
// generateContent API and is Gemini-only. `type = "anthropic-vertex"`
// is for users running Claude on GCP and who want the Anthropic-shape
// features (prompt caching, computer-use, server tools) that
// generateContent doesn't model.
//
// Auth follows Google Application Default Credentials:
// GOOGLE_APPLICATION_CREDENTIALS env var, `gcloud auth application-
// default login` on a workstation, or the GCE/GKE/Cloud Run metadata
// server in deployed environments. Project + Region are required —
// unlike VertexClient there is no env-var fallback (the SDK's vertex
// middleware panics if region is empty).
type AnthropicVertexClient struct {
	// Model is the Vertex-side model name (e.g.
	// "claude-3-5-sonnet-v2@20241022"). Note the `@` versioning suffix
	// — different from api.anthropic.com (`-latest`) and Bedrock
	// (`anthropic.claude-...`) names.
	Model string

	// Region is the GCP region (e.g. "us-east5"). Required by the
	// anthropic-sdk-go vertex middleware — empty will panic during
	// option-construction, so we validate up front and surface a
	// real error.
	Region string

	// Project is the GCP project ID. Required.
	Project string

	// MaxTokens caps response length (Messages-API required). Zero
	// defaults to 8192.
	MaxTokens int64

	// ExtendedThinking + Budget — same semantics as AnthropicClient.
	// Not every Vertex-hosted Claude model supports thinking; the API
	// will reject the request if the chosen model can't handle it.
	ExtendedThinking       bool
	ExtendedThinkingBudget int64

	// PromptCaching — same semantics as AnthropicClient.PromptCaching.
	// Vertex-hosted Claude honours the same cache_control markers.
	PromptCaching bool

	// HTTPClient overrides the SDK's transport. Tests inject a custom
	// RoundTripper here; production leaves nil.
	HTTPClient *http.Client

	// ProbeInterval — test seam. Zero uses the package default.
	ProbeInterval time.Duration

	conn connTracker

	// sdk is built lazily on first Chat call so config changes before
	// first use take effect.
	sdk *anthropic.Client
}

// LLMConnState lets the TUI render this provider's connection state
// through the same ConnStateReporter interface every other provider
// uses.
func (c *AnthropicVertexClient) LLMConnState() ConnState { return c.conn.get() }

func (c *AnthropicVertexClient) client(ctx context.Context) (*anthropic.Client, error) {
	if c.sdk != nil {
		return c.sdk, nil
	}
	if c.Region == "" {
		return nil, errors.New("region is required (e.g. us-east5)")
	}
	if c.Project == "" {
		return nil, errors.New("project is required")
	}

	// WithGoogleAuth panics if region is empty — gated above. It returns
	// a request-option that wires ADC + the vertex base URL onto the
	// SDK at construction time.
	opts := []option.RequestOption{
		vertex.WithGoogleAuth(ctx, c.Region, c.Project),
		option.WithMaxRetries(0), // single source of retry policy
	}
	if c.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(c.HTTPClient))
	}

	sdk := anthropic.NewClient(opts...)
	c.sdk = &sdk
	return c.sdk, nil
}

// Chat translates the ChatRequest to Messages-API params and streams
// the result through the vertex-wrapped SDK client.
func (c *AnthropicVertexClient) Chat(ctx context.Context, req ChatRequest) (<-chan Event, error) {
	maxTokens := c.MaxTokens
	if maxTokens == 0 {
		maxTokens = 8192
	}
	params, err := buildAnthropicParams(req, c.Model, maxTokens, c.ExtendedThinking, c.ExtendedThinkingBudget, c.PromptCaching)
	if err != nil {
		return nil, fmt.Errorf("anthropic-vertex: build params: %w", err)
	}

	sdk, err := c.client(ctx)
	if err != nil {
		return nil, fmt.Errorf("anthropic-vertex: %w", err)
	}

	return streamAnthropic(ctx, sdk, params, &c.conn, c.startRecoveryProbe, "anthropic-vertex"), nil
}

// startRecoveryProbe holds the claim for one interval and releases;
// next Chat drives recovery. Same reasoning as AnthropicBedrockClient
// — there's no cheap reachability check sharing the same IAM scope as
// the inference path on Vertex.
func (c *AnthropicVertexClient) startRecoveryProbe() {
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
