// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/bedrock"
	"github.com/anthropics/anthropic-sdk-go/option"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// AnthropicBedrockClient routes Anthropic Messages API calls through
// Amazon Bedrock instead of api.anthropic.com. Same wire protocol once
// the anthropic-sdk-go bedrock middleware swaps the base URL and signs
// requests with AWS SigV4, so it reuses every translator + the
// streaming loop from AnthropicClient.
//
// Distinct from BedrockClient (`type = "bedrock"`), which talks the
// multi-vendor Converse API. The Anthropic-native path here is opt-in
// because Converse covers Claude already and is the better default;
// `type = "anthropic-bedrock"` is for users who specifically want the
// Anthropic-shape features (prompt caching, computer-use, server tools)
// that don't fit Converse's lowest-common-denominator schema.
//
// Auth follows the standard AWS credential chain (env vars, profile,
// EC2/ECS/EKS instance role) — see Region and Profile to override.
type AnthropicBedrockClient struct {
	// Model is the Bedrock-side model ID (e.g.
	// "anthropic.claude-3-5-sonnet-20241022-v2:0") or an inference
	// profile ARN. Different from api.anthropic.com model names.
	Model string

	// Region is the AWS region for the Bedrock control plane. Empty
	// falls through to the SDK's default region resolution.
	Region string

	// Profile selects a named entry from ~/.aws/credentials. Empty
	// uses the default credential chain.
	Profile string

	// MaxTokens caps response length (Messages-API required). Zero
	// defaults to 8192.
	MaxTokens int64

	// ExtendedThinking + Budget — same semantics as AnthropicClient.
	// Not every Bedrock-hosted Claude model supports thinking; the API
	// will reject the request if the chosen model can't handle it.
	ExtendedThinking       bool
	ExtendedThinkingBudget int64

	// GuardrailID / GuardrailVersion / GuardrailTrace mirror BedrockClient's
	// guardrail fields. The Anthropic-native path on Bedrock uses
	// :invoke-model rather than :converse, so guardrails get applied via
	// the X-Amzn-Bedrock-Guardrail* HTTP headers instead of a structured
	// request field. Same AWS Guardrails resource either way.
	GuardrailID      string
	GuardrailVersion string
	GuardrailTrace   string

	// PromptCaching — same semantics as AnthropicClient.PromptCaching.
	// Bedrock-hosted Claude honours the same cache_control markers
	// since the SDK speaks the Messages API on this path.
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

// LLMConnState lets the TUI render Bedrock's connection state through
// the same ConnStateReporter interface every other provider uses.
func (c *AnthropicBedrockClient) LLMConnState() ConnState { return c.conn.get() }

func (c *AnthropicBedrockClient) client(ctx context.Context) (*anthropic.Client, error) {
	if c.sdk != nil {
		return c.sdk, nil
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{}
	if c.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(c.Region))
	}
	if c.Profile != "" {
		loadOpts = append(loadOpts, awsconfig.WithSharedConfigProfile(c.Profile))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}

	opts := []option.RequestOption{
		bedrock.WithConfig(awsCfg),
		option.WithMaxRetries(0), // single source of retry policy
	}
	if c.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(c.HTTPClient))
	}
	if c.GuardrailID != "" {
		// Bedrock evaluates the guardrail when these headers ride along
		// on InvokeModel. Validation matches the Converse path so a
		// typo'd trace value fails fast instead of mid-stream.
		hdrs, err := bedrockGuardrailHeaders(c.GuardrailID, c.GuardrailVersion, c.GuardrailTrace)
		if err != nil {
			return nil, fmt.Errorf("guardrail: %w", err)
		}
		for k, v := range hdrs {
			opts = append(opts, option.WithHeader(k, v))
		}
	}

	sdk := anthropic.NewClient(opts...)
	c.sdk = &sdk
	return c.sdk, nil
}

// Chat translates the ChatRequest to Messages-API params and streams
// the result through the bedrock-wrapped SDK client.
func (c *AnthropicBedrockClient) Chat(ctx context.Context, req ChatRequest) (<-chan Event, error) {
	maxTokens := c.MaxTokens
	if maxTokens == 0 {
		maxTokens = 8192
	}
	params, err := buildAnthropicParams(req, c.Model, maxTokens, c.ExtendedThinking, c.ExtendedThinkingBudget, c.PromptCaching)
	if err != nil {
		return nil, fmt.Errorf("anthropic-bedrock: build params: %w", err)
	}

	sdk, err := c.client(ctx)
	if err != nil {
		// Credential-load failures are user errors, not transport
		// problems — surface synchronously so the TUI shows the real
		// reason instead of a generic "disconnected" flicker.
		return nil, fmt.Errorf("anthropic-bedrock: %w", err)
	}

	return streamAnthropic(ctx, sdk, params, &c.conn, c.startRecoveryProbe, "anthropic-bedrock"), nil
}

// startRecoveryProbe mirrors AnthropicClient but skips the reachability
// ping: Bedrock's Models endpoint is region-scoped and charges different
// IAM permissions than the inference path, so a probe could spuriously
// fail while inference works. Hold the claim for one interval and
// release; the next Chat call drives recovery.
func (c *AnthropicBedrockClient) startRecoveryProbe() {
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
