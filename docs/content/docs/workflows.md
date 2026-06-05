---
title: Workflows
weight: 10
---

# Workflows

A *workflow* is a declarative agent pipeline — multiple roles with
their own prompts and tool restrictions, wired together by edges. The
canonical example is **planner → coder → reviewer**: one agent
investigates and produces a plan, the next implements it, the third
critiques the result.

## File format

`~/.config/enso/workflows/<name>.md` (user) or `<cwd>/.enso/workflows/<name>.md`
(project), frontmatter + body. One `## <role>` section per agent.

```markdown
---
roles:
  planner:
    tools: [read, grep, glob]
  coder:
    tools: [read, write, edit, bash, grep, glob]
  reviewer:
    tools: [read, grep, glob]
edges:
  - planner -> coder
  - coder -> reviewer
---

## planner

You are the planner. Read the relevant code and produce a concrete,
file-scoped plan for the following request:

{{ .Args }}

Output a numbered list of changes, each naming the file and the
specific modification. Do not write code yet. End with a short list
of risks and open questions.

## coder

You are the coder. Implement the plan below using read/write/edit/bash:

{{ .planner.output }}

Do not deviate from the plan; if you find the plan is wrong, stop and
explain rather than improvise.

## reviewer

You are the reviewer. The implementation is below; the original plan
preceded it:

Plan:
{{ .planner.output }}

Implementation summary:
{{ .coder.output }}

Read the actual changed files and report:
- What's correct.
- What's wrong (severity-ordered).
- What tests would catch regressions.
```

## Frontmatter fields

```yaml
roles:                       # role-name → role config
  <name>:
    tools: [...]             # registry filter; empty = full registry
    model: <name>            # provider name from [providers.X]; empty = inherit default
edges:                       # explicit dependencies — runner topo-sorts
  - planner -> coder
  - coder -> reviewer
  - reviewer -> ship if '...'  # optional `if` guard — see Conditional edges
```

`edges` declares dependency direction (`a -> b` means b waits for a).
Roles with no incoming edges run first. Sibling roles (no dependency
between them) run in parallel. An edge may carry an optional `if`
predicate that gates whether it fires — see [Conditional edges](#conditional-edges).

## Body templating

The body has one `## <role>` section per declared role. Each section
is a Go `text/template` that runs with these variables:

| Variable             | Value                                                            |
| -------------------- | --------------------------------------------------------------- |
| `.Args`              | The argument string passed to the workflow.                     |
| `.<role>.output`     | The raw final text of `<role>` (only available *after* it runs).|
| `.<role>.<field>`    | A structured field parsed from `<role>`'s output (see below).    |

A role can reference any previous role by name. The runner enforces
edge ordering so `{{ .planner.output }}` is populated by the time
`coder` renders.

## Structured outputs

Besides the raw `.output`, each role can expose **named fields** that
downstream templates read as `.<role>.<field>`. Fields are parsed from
the role's final message, in this precedence:

1. The **last** fenced ` ```json ` block in the message, decoded as a
   flat JSON object. This is the recommended form:

   ````
   I reviewed the diff and it looks good.

   ```json
   {"verdict": "LGTM", "score": 9, "blocking": false}
   ```
   ````

   A downstream role can then reference `{{ .review.verdict }}`,
   `{{ .review.score }}`, etc. Numbers render without a trailing `.0`
   (`9`, not `9.0`), booleans as `true`/`false`, and nested
   arrays/objects as compact JSON.

2. If there's no JSON block, contiguous trailing `KEY: value` lines are
   used instead:

   ```
   verdict: LGTM
   reason: all tests pass
   ```

   Scanning stops at the first blank or non-`KEY: value` line, so prose
   above the block is ignored.

If neither form is present, a role simply has no fields — `.output`
still carries the full raw text, so existing workflows are unaffected.

Structured fields are also what conditional edges (below) gate on.

Notes:

- **Reserved field names.** `output` and `skipped` are reserved; a role
  emitting a field with one of those names is shadowed by the built-in
  meaning (`.<role>.output` always means the raw text). Avoid them.
- **Missing fields render empty.** A reference to a field a role didn't
  emit renders the empty string (Go's `<no value>`). Guard optional
  fields with `{{ with .review.reason }}…{{ end }}` so the literal
  placeholder never leaks into a prompt.
- **Malformed JSON yields no fields.** If the last ` ```json ` block
  fails to parse, the role gets empty fields (it does *not* fall back to
  `KEY: value`). `.output` is still the raw text.

## Conditional edges

An edge can carry an `if` guard — a single-quoted predicate that decides
at runtime whether the edge **fires**:

```yaml
edges:
  - build  -> review
  - review -> ship      if '{{ eq .review.verdict "LGTM" }}'
  - review -> escalate  if '{{ ne .review.verdict "LGTM" }}'
```

A node runs only if **all** of its incoming edges fire (strict AND). If
any incoming edge does not fire, the node is **skipped** — and skips
propagate: a skipped node's outgoing edges never fire, so its dependents
skip too. Above, `review` runs once and exactly one of `ship` /
`escalate` runs; the other is skipped. A skipped role produces no LLM
call, prints `role: (skipped)`, and exposes `.<role>.skipped == true`.

### Predicate language

A predicate is a Go `text/template` evaluated against the **same context
as role bodies** (`.Args`, `.<role>.output`, `.<role>.<field>`). It is
truthy unless it renders to an empty string, `false`, `0`, `no`, or
`<no value>` (case-insensitive). Available helpers:

| Helper                         | Meaning                                         |
| ------------------------------ | ----------------------------------------------- |
| `eq` / `ne` / `and` / `or` / `not` | stdlib template builtins                     |
| `contains haystack needle`     | case-insensitive substring (subject first)      |
| `matches subject pattern`      | regexp match (subject first, pattern second)    |

`contains` and `matches` are available in role bodies too.

### Missing fields are lenient — but mind the eq/ne asymmetry

A predicate that references a field the role never emitted evaluates as
**not satisfied** rather than aborting the run. Concretely, with the
field absent:

- `'{{ eq .review.verdict "LGTM" }}'` → **false** (the gate does *not*
  fire — the node stays put).
- `'{{ ne .review.verdict "LGTM" }}'` → **true** (the gate *fires*).

So in the ship/escalate pair above, an unparseable or field-less review
makes `ship` skip and `escalate` run — the safe direction. Choose `eq`
vs `ne` deliberately for which branch should win when a field is missing.

A worked example ships at `examples/workflows/gated-ship.md`.

> **Branch-then-merge is not supported.** Because the join is strict-AND,
> a diamond where only one branch fires will *skip* the merge node. Join
> modes (`any`) are not implemented.

## Running

In the TUI:

```
/workflow build-feature add OAuth login flow
```

From the CLI (single-shot):

```bash
enso run --workflow build-feature "add OAuth login flow"
```

The workflow runs to completion, streaming each role's output to
stdout. Each role gets its own AgentID; their transcripts are
captured for inspection via `/transcript` (list) or
`/transcript <id-or-prefix>` (show one). The session-inspector
overlay (`Ctrl-Space`) shows in-flight agent state.

## Parallel siblings

Roles with no edge between them run concurrently:

```yaml
roles:
  read-backend:
    tools: [read, grep, glob]
  read-frontend:
    tools: [read, grep, glob]
  synth:
    tools: [read, write]
edges:
  - read-backend -> synth
  - read-frontend -> synth
```

`read-backend` and `read-frontend` run in parallel; `synth` waits for
both. The runner uses goroutines + a topological scheduler. Sibling
parallelism is goroutine-correct but not load-tested at large
fan-outs (10+); if you hit weird ordering, file a bug.

## Example workflows

Shipped under `examples/workflows/` — copy any as a starting point:

- `build-feature.md` — the linear planner→coder→reviewer pipeline.
- `plan-deep-execute-fast.md` — the same shape across two providers
  (deep model plans/reviews, fast model executes).
- `gated-ship.md` — conditional routing: a reviewer's structured verdict
  gates ship-vs-escalate via `if` edges.

## What's not yet supported

- **Loops** — DAG only; cycles are rejected at parse time.
- **Join modes** — the conditional-edge join is strict-AND only; there's
  no `any` join, so branch-then-merge diamonds skip the merge node.
