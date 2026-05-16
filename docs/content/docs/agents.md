---
title: Agents
weight: 6
---

# Declarative agents

A *declarative agent* is a reusable bundle of (system-prompt addition,
tool restrictions, sampler overrides, max-turns) that replaces the
default top-level configuration for a session. Pick one with `--agent
<name>` at startup; list them with `/agents` in the TUI.

## Built-in: plan mode

```bash
enso --agent plan
```

The `plan` agent is read-only. `bash`, `write`, and `edit` are removed
from the registry; only `read`, `grep`, `glob`, `web_fetch`, and `todo`
remain. The system prompt explicitly tells the model: *investigate
and produce a plan; do not modify files*. Use it for "tell me what
this codebase does" or "design a fix" sessions where you don't want
the model touching anything.

## File format

Drop a frontmatter-headed Markdown file at:

- `~/.config/enso/agents/<name>.md` — user-global, shared across projects.
- `<cwd>/.enso/agents/<name>.md` — project-scoped, committed or local.

Project shadows user; user shadows built-in. So a project's
`.enso/agents/plan.md` can override the built-in plan mode for that
repo if you want different defaults.

## Frontmatter fields

```yaml
---
name: reviewer                       # default = filename minus extension
description: read-only critic
allowed-tools:                        # registry filter; empty = no restriction
  - read
  - grep
  - glob
  - lsp_hover
  - lsp_definition
  - lsp_references
  - lsp_diagnostics
denied-tools:                         # applied after allowed-tools
  - write
  - edit
  - memory_save
temperature: 0.2                      # sampler override; nil = unchanged
top_p: 0.9                            # sampler override; nil = unchanged
top_k: 40                             # sampler override; nil = unchanged
max_turns: 30                         # 0 = inherit caller default
---

# body becomes the prompt-append
```

The body is appended to the base system prompt with a blank-line
separator. It does **not** replace — the agent still inherits ensō's
default operating instructions. Treat the body as "additional
operating instructions for this profile."

## Example: a code reviewer

`examples/agents/reviewer.md` (shipped with the source) is a complete
working example. The shape:

```markdown
---
name: reviewer
description: read-only critic — reads diffs and code and tells you what could go wrong
allowed-tools: [read, grep, glob, bash, lsp_hover, lsp_definition, lsp_references, lsp_diagnostics]
denied-tools: [write, edit, memory_save]
temperature: 0.2
---

# Reviewer mode

You are operating as a code reviewer. ... (full body in the example)
```

Run with `enso --agent reviewer`. `bash` is allowed but read-only by
convention — the system prompt tells the model to use it for `git
diff`, `git log`, linters, tests; not for modifying anything.

## How sampler overrides work

When you set `temperature`, `top_p`, or `top_k` in the frontmatter,
the host clones the LLM provider with those fields overridden. The
clone shares the same connection pool (so concurrency limits still
apply) and only changes sampler behavior for this session.

Leave a field unset (don't include it in frontmatter) to inherit the
provider default.

## What's not yet supported

- **Per-agent `model:`** — declarative agents have no `model` field
  yet. Per-provider selection is available via `/model` (session
  level), workflow YAML role `model:` (per-role), and `spawn_agent`'s
  `model` arg (per-call); declarative agent profiles haven't been
  wired into that path. File a request if you want it.
- **Mid-session agent switching** — agent is fixed at startup. Switch
  by relaunching with a different `--agent`. The TUI's `/agents`
  command lists what's available but doesn't switch live.
