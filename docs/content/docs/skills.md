---
title: Skills
weight: 8
---

# Skills

A *skill* is a user-defined slash command. Drop a frontmatter-headed
markdown file at `~/.enso/skills/<name>.md` (user) or
`<cwd>/.enso/skills/<name>.md` (project), and `/<name>` becomes a slash
command in the TUI that injects the rendered body as the next user
message.

Project shadows user on name collision.

## File format

```markdown
---
name: explain-this
description: read-only summariser — explains a file, function, or area of the codebase
allowed-tools: [read, grep, glob, lsp_hover, lsp_definition, lsp_references]
---

You are about to explain a piece of this codebase to the user. The
target is:

{{ .Args }}

Investigate read-only first — use `glob` / `grep` to find relevant
files, `read` to inspect them, and `lsp_*` if a language server is
available for richer signal. Then write a concise explanation in this
shape:

1. **What it is** — one paragraph, plain language. No code.
2. **How it fits in** — what calls it, what it depends on.
3. **Subtleties** — anything non-obvious.
4. **Where to look next** — 1–2 next files worth reading.
```

The body is a Go `text/template`. Available variables:

| Variable   | Value                                                              |
| ---------- | ------------------------------------------------------------------ |
| `.Args`    | Everything the user typed after the slash command name.            |

Frontmatter fields:

| Field           | Default                       | Description                                                                                       |
| --------------- | ----------------------------- | ------------------------------------------------------------------------------------------------- |
| `name`          | filename minus `.md`          | The slash command name. `/<name>` invokes it.                                                     |
| `description`   | empty                         | Shown in `/help`. Keep it short.                                                                  |
| `allowed-tools` | empty (no restriction)        | Restricts the registry to these tools for the **next turn only**. Cleared after the turn quiesces. |
| `model`         | (parsed but ignored)          | Reserved; not yet wired into the slash-command path. Per-call provider selection is available via `spawn_agent`'s `model` arg or workflow YAML's role `model:`. |

## How invocation works

When you type `/explain-this how does the bus work`, enso:

1. Looks up `explain-this` in the slash registry.
2. Renders the template with `.Args = "how does the bus work"`.
3. If `allowed-tools` is set, applies a one-shot tool restriction.
4. Submits the rendered text as if you'd typed it.

The submission goes through the same path as a normal user message —
permission rules apply, tool calls go to the bus, the agent goroutine
processes it the same way.

## Difference from agents

| Skills                                         | Agents                                                  |
| ---------------------------------------------- | ------------------------------------------------------- |
| Inject a *one-shot* prompt as the next message | Replace the *session-wide* system prompt addition       |
| Restrict tools for **one turn**                | Restrict tools for the **whole session**                |
| Triggered by typing `/<name>`                  | Selected at startup with `--agent <name>`               |
| Templated body with `.Args`                    | Static body, no templating                              |

Use a skill for "I want to do X right now"; use an agent for "I want
this whole session to behave as Y."

## Example

Shipped at `examples/skills/explain-this.md`. Copy to
`~/.enso/skills/` or `<project>/.enso/skills/` and try
`/explain-this internal/bus`.
