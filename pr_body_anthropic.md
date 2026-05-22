## Summary

- Added support for Anthropic-native paths (opt-in) to access advanced Claude features.
- Implemented three new vendor adapters: `anthropic` (direct), `anthropic-bedrock`, and `anthropic-vertex`.
- Shared the translation and streaming logic across all Anthropic-variant adapters to ensure consistency.

## Motivation

While `bedrock` and `vertex` adapters provide universal compatibility via Converse/generateContent APIs, they lack support for certain Anthropic-specific features like prompt caching, computer-use, and server tools. These new opt-in paths allow users to use the full Anthropic Messages API while still leveraging their existing AWS or GCP infrastructure.

## Changes

- **`internal/llm/anthropic.go`**: New core adapter for direct Anthropic API access.
- **`internal/llm/anthropic_bedrock.go`**: New adapter for Claude on AWS Bedrock using the Anthropic Messages API.
- **`internal/llm/anthropic_vertex.go`**: New adapter for Claude on GCP Vertex AI using the Anthropic `:rawPredict` endpoint.
- **`internal/llm/provider.go`**: Updated factory to support `anthropic`, `anthropic-bedrock`, and `anthropic-vertex` types.
- **`docs/content/config/reference.md`**: Updated documentation to explain the opt-in paths and how to choose between them.
- **Comprehensive Testing**: Added `anthropic_test.go`, `anthropic_bedrock_test.go`, and `anthropic_vertex_test.go` covering schema translation, tool-call round-trips, and parameter enforcement.

## Test Plan

- [x] Unit tests pass (verified manually)
- [x] Tool-call round-trip translation (OpenAI shape $\to$ Anthropic shape)
- [x] System message hoisting
- [x] Extended thinking parameter enforcement (temperature/top_p clamping)
- [x] JSON schema lifting for tool definitions
