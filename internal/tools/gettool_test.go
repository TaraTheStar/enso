// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

// getTool is a single-value form of Registry.Get for tests that look a tool up
// and use it directly (the registry now returns (Tool, bool) via azoth/tools).
// Returns nil when absent, so existing `== nil` / chained-call assertions keep
// working.
func getTool(r *Registry, name string) Tool {
	t, _ := r.Get(name)
	return t
}
