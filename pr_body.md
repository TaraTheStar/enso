## Summary

- Implements multi-line soft-wrapping for the TUI input component.
- Enables vertical scrolling for the input window to keep the cursor visible.
- Adds support for multi-line input via `shift+enter`, `alt+enter`, and `ctrl+j`.
- Fixes the issue where typing past the terminal edge would cause the line to run off-screen.

## Motivation

The previous single-line input implementation was limited to horizontal scrolling, making it difficult to manage and view long, multi-line commands or prompts. This change allows for a much more natural multi-line editing experience within the TUI.

## Changes

- **`internal/ui/bubble/input.go`**: Replaced horizontal scrolling logic with a vertical soft-wrap and scroll mechanism. Added `maxInputLines` constraint.
- **`internal/ui/bubble/model.go`**: Added key bindings for multi-line input and updated slash command handling to refresh the model name after a provider switch.
- **`internal/ui/bubble/model_test.go`**: Updated tests to validate the new multi-line and vertical scroll constraints.

## Test Plan

- [x] Unit tests pass (`go test ./internal/ui/bubble/...`)
- [x] Manual testing of multi-line input and vertical scrolling in the TUI.
- [x] Verified that `shift+enter` / `alt+enter` / `ctrl+j` correctly insert newlines.

## Notes for Reviewers

The `render` function in `input.go` has been heavily refactored. Please pay close attention to the manual segment calculation and cursor positioning logic to ensure no regressions in character width or ANSI state.
