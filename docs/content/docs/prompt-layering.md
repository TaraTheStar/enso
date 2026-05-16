---
title: Prompt layering
weight: 7
---

# Prompt layering

The system prompt ensō sends the model is assembled from ordered
**layers**. Every layer is *appended* by default; a layer can opt to
*replace* everything before it. This page is the complete reference for
what goes where and how to override it.

## The layers, in order

1. **Default prompt** — embedded in the binary.
2. **`## Available models`** — auto-generated, only when ≥2 providers
   are configured (see [below](#available-models-section)).
3. **`~/.config/enso/ENSO.md`** — your user-wide instructions.
4. **Closest `ENSO.md`** walking up from the cwd — project instructions.
5. **Closest `AGENTS.md`** walking up from the cwd — project instructions.
6. **Auto-memory** — facts saved via the `memory_save` tool
   ([Memory]({{< relref "memory.md" >}})).

Each file layer is added **only if the file exists and is non-empty**.
Project `ENSO.md` / `AGENTS.md` are found by walking up from the working
directory, so a file at the repo root applies to every subdirectory.

## Append by default

A file with no frontmatter is appended after everything before it. This
is almost always what you want: a user `~/.config/enso/ENSO.md` saying
"prefer terse output" *adds* that line without throwing away the 100-odd
lines of default behaviour, and a repo `ENSO.md` layers project context
on top of your personal preferences.

```markdown
Prefer terse explanations. Assume I know the codebase.
```

## `replace: true` — start from a clean slate

Add YAML frontmatter with `replace: true` to make a file **discard every
earlier layer** (default prompt, the models section, and any
higher-priority file):

```markdown
---
replace: true
---
You are operating strictly under the rules below. <your full prompt>
```

Use cases:

| You want | Where | Frontmatter |
| --- | --- | --- |
| Add a preference / routing rule | `~/.config/enso/ENSO.md` | none |
| A fully hand-written user prompt | `~/.config/enso/ENSO.md` | `replace: true` |
| Team adds project context on top of personal prefs | repo `ENSO.md` | none |
| Team pins one exact, repo-committed prompt for everyone | repo `ENSO.md` | `replace: true` |

`replace: true` at the project level discards the user layer too — that
is usually what "this repo has a canonical prompt" means.

The frontmatter itself is never sent to the model; only the body after
it is used.

## `## Available models` section {#available-models-section}

When two or more `[providers.*]` are configured, ensō injects a section
naming the model the agent is running as and listing the others, so it
can route work via `spawn_agent`'s `model:` argument:

```text
## Available models

You are running as `qwen-fast` (model: qwen3.6-27b, context: 65k, pool: latchkey-3090, swap-cost: high).

Other configured providers (delegate to one with the `spawn_agent` tool's
`model:` argument, or switch the active model with `/model <name>` in the TUI):

- `deep` (model: minimax-m2.7, context: 131k, pool: crosstie-halo) — deep reasoning, hard SWE

Models sharing a pool run on the same hardware; switching the active
model between pool-mates forces an expensive reload. …
```

- Per-provider `description` and pool/`swap-cost` come from config — see
  [`[providers]` and `[pools]`]({{< relref "../config/reference.md" >}}).
- It sits at layer 2, so a `replace: true` file discards it along with
  the default — by design.
- It is **static for the session**: a mid-session `/model` swap does not
  rewrite the "running as" line (the provider list never changes
  anyway). Suppress it entirely with `[instructions] include_providers =
  false`.

## Inspecting the result: `/prompt`

Run `/prompt` in the TUI for a per-layer breakdown — each layer's size,
which carried `replace: true`, and which were discarded by a later
replace. `/prompt --full` dumps every layer's full body. Use it whenever
a misconfigured frontmatter makes the model behave unexpectedly: you see
the *breakdown*, the model only sees the result.

## See also

- [Memory]({{< relref "memory.md" >}}) — the auto-memory layer.
- [Agents]({{< relref "agents.md" >}}) — declarative profiles append a
  further per-agent prompt.
- [Config reference]({{< relref "../config/reference.md" >}}) —
  `[instructions]`, `[providers]`, `[pools]`.
