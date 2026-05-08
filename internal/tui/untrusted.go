// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

// untrustedContentTools is the set of tool names whose results carry
// text the user didn't write — language-server output, bash stdout,
// file/web reads, grep/glob hits. Per AGENTS.md, that text is a
// prompt-injection surface regardless of whether the workspace passed
// the startup trust gate; a docstring inside `node_modules/some-lib/`
// can plant text shaped like instructions and the model may follow
// them. We mark these tool blocks in the chat so users have a visible
// cue that the rendered output is external content, not enso's own.
//
// Excluded: `write` and `edit` (the user supplies the content),
// `web_search` (results are summaries from a controlled backend, not
// raw page bodies), `todo` / `memory_save` (internal state).
var untrustedContentTools = map[string]bool{
	"bash":            true,
	"read":            true,
	"grep":            true,
	"glob":            true,
	"web_fetch":       true,
	"lsp_hover":       true,
	"lsp_definition":  true,
	"lsp_references":  true,
	"lsp_diagnostics": true,
}

// isUntrustedContentTool reports whether `name` is one of the tools
// whose output should carry the external-content marker. Returns
// false for unknown names so MCP tools (`mcp__server__tool`) and
// future additions don't accidentally pick up the marker — those
// have their own threat profile and need an explicit decision.
func isUntrustedContentTool(name string) bool {
	return untrustedContentTools[name]
}

// untrustedMarker is the dim flag prepended to a content-bearing tool
// call. Subtle by design: long sessions are mostly tool calls and
// most of them are content-bearing, so a loud marker would fade into
// noise. The flag glyph is universally rendered and copy-paste safe.
const untrustedMarker = "[comment]⚑[-] "
