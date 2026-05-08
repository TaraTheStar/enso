// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import "github.com/TaraTheStar/enso/internal/llm"

// fmtConnState turns a provider client's connection state into the
// pre-styled segment the status template expects. Returns "" in the
// healthy case so the template can gate the leading separator —
// degraded states are the only time anything renders.
//
// Test fakes (the llmtest.Mock chat client) don't implement
// ConnStateReporter; the type assertion fails silently and the
// segment stays empty, which is the right default for tests.
func fmtConnState(client llm.ChatClient) string {
	r, ok := client.(llm.ConnStateReporter)
	if !ok {
		return ""
	}
	switch r.LLMConnState() {
	case llm.StateReconnecting:
		return "[yellow]○ reconnecting[-]"
	case llm.StateDisconnected:
		return "[red]✘ disconnected[-]"
	default:
		return ""
	}
}
