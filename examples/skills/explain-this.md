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
2. **How it fits in** — what calls it, what it depends on, what
   surrounds it. Reference paths with `file:line` so the user can
   click through.
3. **Subtleties** — anything non-obvious. Edge cases the code
   handles, conventions it follows, gotchas a reader would miss on
   first pass.
4. **Where to look next** — 1–2 next files or functions worth reading
   to understand the larger picture.

Do not modify anything. If you can't find the target, say so plainly
and ask the user to clarify rather than guessing.

Save this skill at `~/.config/enso/skills/explain-this.md` (or
`<project>/.enso/skills/explain-this.md` for project-local) and invoke
with `/explain-this <thing>` in the TUI.
