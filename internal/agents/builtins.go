// SPDX-License-Identifier: AGPL-3.0-or-later

package agents

// Builtins returns the agent specs compiled into the binary. They are
// shadowed by user and project agents on name collision so a project can
// retune the prompt or tool set without forking the source.
func Builtins() []*Spec {
	return []*Spec{
		planAgent(),
	}
}

func planAgent() *Spec {
	return &Spec{
		Name:        "plan",
		Description: "read-only investigation: produce a written plan, do not modify files",
		AllowedTools: []string{
			"read", "grep", "glob", "web_fetch", "todo",
		},
		PromptAppend: `# Plan mode

You are operating in **plan mode**. Your job is to investigate and produce
a written plan, not to make changes.

- The bash, write, and edit tools are unavailable in this mode. Don't try
  to call them; they will not appear in the tool list.
- Use read / grep / glob / web_fetch to gather facts. Use todo to track
  open questions for the user.
- When you have enough information, deliver a concrete plan: bullet list of
  steps, named files, the risk you see, and what you'd want answered before
  executing. Do not start "implementing" by writing prose that pretends to
  be code edits — explicitly say "I would change X in file Y to do Z" and
  stop there.

The user will switch you back to a normal agent (or copy the plan into a
fresh session) when they're ready to act on it.`,
	}
}
