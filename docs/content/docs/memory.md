---
title: Memory
weight: 7
---

# Auto-memory

Sessions don't share state by default — each new session sees the
same system prompt and starts cold. Auto-memory is the mechanism for
making certain facts persistent across sessions.

## How it works

Two things, working together:

1. **The `memory_save` tool**. The agent calls it with a name and
   content; ensō writes a markdown file at
   `<cwd>/.enso/memory/<slug>.md`.
2. **The instruction loader**. At session start, every `*.md` under
   `~/.enso/memory/` (user) and `<cwd>/.enso/memory/` (project) is
   concatenated and appended to the system prompt under a `## Auto-memory`
   header. Project files shadow user files on name collision.

A future session in the same project automatically inherits everything
the agent saved. No re-explaining the database constraints, no
re-stating the team's lint policy.

## What to save

The tool's description tells the model:

> Save: corrections the user has explicitly given, project-specific
> policies ("integration tests must hit a real database"), durable
> preferences. Don't save: in-progress work, ephemeral state, or
> things obvious from reading the code or git history.

In practice the agent saves things like:

- "User prefers terse output — no headers/sections for simple
  questions."
- "Database integration tests must hit a real Postgres, not a mock.
  Reason: prior incident where mock/prod divergence masked a broken
  migration."
- "Don't run `make check` after every edit — too slow on this repo;
  prefer `go test ./internal/...` for feedback."
- "Auth middleware was rewritten for compliance reasons (legal flagged
  session-token storage). Scope decisions favor compliance over
  ergonomics."

## Format

A memory file is plain markdown. The agent's saves typically follow
this shape (lifted from the `memory_save` tool's prompt):

```markdown
---
name: db-test-policy
description: integration tests must hit a real database
type: feedback
---

Integration tests must hit a real Postgres, not a mock.

**Why:** prior incident where the mock's assertions diverged from
production behavior and a broken migration shipped undetected.

**How to apply:** anywhere you'd reach for a mock in `*_integration_test.go`,
spin up the real database via `docker compose up postgres-test` instead.
```

The frontmatter is optional and informational — only the body content
matters at load time.

## Inspecting memories

```bash
ls ~/.enso/memory/                   # user-global
ls .enso/memory/                     # project
cat .enso/memory/db-test-policy.md
```

You can also delete or hand-edit them:

```bash
rm .enso/memory/some-stale-fact.md
```

## Project vs user

Project memories (`<cwd>/.enso/memory/`) describe project-specific
facts and travel with the repo if you commit them. They're the right
place for "this project does X."

User memories (`~/.enso/memory/`) describe you and travel across
projects. Right for "I'm a senior Go dev — frame frontend
explanations in terms of Go analogues" or "I prefer terse output."

When both exist with the same filename, project wins.

## Limitations

- The `memory_save` tool only writes to the **project** directory
  (`<cwd>/.enso/memory/`). User-global memories must be created or
  moved by hand.
- There's no automatic memory aging or compaction. Files accumulate
  until you trim them. `enso stats` doesn't track memory file count,
  but `ls` does.
- All memory files are concatenated into the system prompt every
  session, costing tokens. Keep them concise — a memory worth saving
  is worth saying in one paragraph.
