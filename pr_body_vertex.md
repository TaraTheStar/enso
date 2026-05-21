## Summary

- Added a new Google Cloud Vertex AI vendor adapter to support Gemini models via the `generateContent` API.
- Expanded configuration schema to support Vertex-specific fields like `gcp_project`, `gcp_location`, and `extended_thinking`.
- Updated the onboarding wizard to include a dedicated Vertex configuration flow.

## Motivation

As the ecosystem expands, support for the Gemini family of models via Vertex AI is a high-priority requirement for users in Google Cloud environments. This adapter allows `enso` to leverage Gemini 2.5's capabilities, including reasoning/thinking modes, through the existing unified interface.

## Changes

- **`internal/config`**: Added `GCPProject`, `GCPLocation`, `ExtendedThinking`, and `ExtendedThinkingBudget` to `ProviderConfig`.
- **`internal/config/wizard.go`**: Implemented `runVertexBranch` and `buildVertexWizardTOML` for a streamlined Vertex onboarding experience.
- **`internal/llm/vertex.go`**: New implementation of `ChatClient` using `google.golang.org/genai`.
- **`internal/llm/vertex_test.go`**: Comprehensive unit tests for the Vertex adapter, covering tool-call name recovery, reasoning/thought routing, and schema preservation.
- **`internal/llm/provider.go`**: Updated factory logic to dispatch `vertex` types.
- **`docs/content/config/reference.md`**: Updated documentation with Vertex configuration examples and authentication details.

## Test Plan

- [x] Unit tests pass (verified manually in environment)
- [x] Vertex adapter handles system message hoisting correctly
- [x] Vertex adapter handles tool-call name recovery and response collapsing
- [x] Gemini thinking/reasoning parts are correctly routed to the reasoning channel
- [x] Tool schema (including `additionalProperties`) is preserved during translation

## Notes for Reviewers

The adapter leverages Google Application Default Credentials (ADC), so no GCP service account keys need to be stored in the `enso` config files. The implementation specifically handles the nuance of Gemini's requirement that tool responses be matched to calls by function name.
