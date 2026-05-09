# AGENTS.md — Operating instructions for any agent working in this repo

If you're an LLM assistant picking up work in this repo, this file
tells you *how to operate*. The user-facing docs live in `README.md`.
What's intentionally not built and where surprises hide live in this
file (see [Non-goals](#non-goals) and [Soak-test risks](#soak-test-risks)
below). The implementation is mature; you're maintaining/extending,
not building from scratch.

## Workflow

1. **Skim [Non-goals](#non-goals)** before suggesting a feature —
   most obvious gaps were considered and rejected for stated reasons.
2. **Run the verify commands below after every meaningful change.**
   Format / vet / build / test all need to pass.
3. **Stay scoped** to the files relevant to the current task. Don't
   drive-by-edit unrelated code.
4. **Single tool call per turn** is the default if you're a quantized
   small-MoE model with thinking enabled. Parallel function calls are
   unreliable on those configs.
5. If you finish a step and realize the spec is wrong or ambiguous,
   **stop and surface it** rather than guessing.

## Verify

```bash
gofmt -l . | tee /dev/stderr | (! grep .)   # diff means run gofmt
go vet ./...
CGO_ENABLED=0 go build ./...                # no-CGO is non-negotiable
go test ./...
```

`make check` runs all four.

## Project conventions

- **Go version**: target `1.23+`. Keep `go.mod` aligned.
- **Module path**: `github.com/TaraTheStar/enso`.
- **No CGO**: every dependency must build with `CGO_ENABLED=0`. SQLite uses `modernc.org/sqlite` (pure Go), not `mattn/go-sqlite3`.
- **Package layout**: one package per directory. Don't reach into `internal/tui` from anything except `cmd/enso`.
- **Imports**: stdlib first, then third-party, then internal — separated by blank lines. `gofmt` sorts.
- **Errors**: wrap with `fmt.Errorf("doing X: %w", err)`. Don't `panic` outside `cmd/enso/main.go`. Return errors up; the agent loop is the recovery point.
- **Logging**: `log/slog` only. Default text handler writing to `~/.enso/enso.log` (set up in `cmd/enso/main.go:initLogging`). Add structured fields (`slog.String("session", id)`) instead of formatting strings. Stderr is never written to from inside the TUI — it'd corrupt the screen.
- **Concurrency**: prefer `context.Context` for cancellation. No naked goroutines without a way to stop them. Channels for events; mutexes only when channels would be awkward.
- **Tests**: table-driven where it fits. The gnarly bits already have tests; add tests for new gnarly logic but don't bother testing thin glue or tview wiring.
- **Comments**: write very few. Identifiers carry the meaning. A comment is for explaining a non-obvious *why* — a hidden constraint, a workaround for an upstream bug, an invariant. If removing it wouldn't confuse a reader, don't write it.

## File-creation discipline

- Create files only when a task asks for them. Don't pre-stub speculative files.
- Don't generate documentation files unless asked. The canonical docs are this file (maintainer reference) and `README.md` (user-facing). Anything else is rot.

## Things to avoid

- **Don't add features that aren't asked for.** If you think something's missing, surface it instead of building it. [Non-goals](#non-goals) below lists what's deliberately out of scope.
- **Don't introduce abstractions speculatively.** Three similar lines is fine; build a helper only when there are real callers asking for it.
- **Don't add error handling for impossible cases.** Internal callers can be trusted; validate at boundaries (user input, network, files) only.
- **Don't add backwards-compatibility shims.** Just change the code.
- **Don't `git push` or commit** unless explicitly asked. Stage and report what's ready, let the human commit.
- **Don't run destructive commands** (`rm -rf`, `git reset --hard`, etc.) without confirmation.
- **Don't pull additional dependencies** without surfacing the choice first. Current deps are in `go.mod`. The load-bearing ones are `tview`/`tcell` (TUI), `spf13/cobra` (CLI), `mark3labs/mcp-go` (MCP client), `modernc.org/sqlite` (no-CGO SQLite), `bmatcuk/doublestar` (path patterns), `pelletier/go-toml/v2` (config), `adrg/frontmatter` (workflow parsing).

## Known model-side quirks

If you're running on llama.cpp with a quantized Qwen3.6-style model:

1. **Thinking + tool calls is fragile.** If a tool call fails to parse server-side, retry once with a fresh assistant message. Don't loop.
2. **Parallel tool calls are unreliable.** Default sequential.
3. **Endless reasoning loops** happen on vague tasks. If you find yourself "thinking" without making progress, the prompt is too open — re-read the task and target named files only.

## When to ask vs. proceed

Auto-mode is active. Proceed without asking on:

- Routine code structure choices that align with the conventions above.
- Adding tests, refactoring within a single file, fixing your own mistakes from earlier in the session.
- Choosing local variable names, helper function names, error wrapping wording.

Stop and ask on:

- Anything that contradicts the conventions or non-goals here (architectural deviation, package layout change, dep substitution).
- Adding a new third-party dependency.
- Anything that touches files well outside the current task's scope.
- Any destructive shell command, any `git push`, any external network call beyond `go mod download`.

## Non-goals

These won't ship without a strong real-world case. Surface, don't
build:

- **Native cloud APIs** (Anthropic / Gemini / OpenAI). enso is
  local-first; the OpenAI-compat SSE protocol is the universal
  interface, and any cloud provider that fronts it (or fronts a
  translator) works today. Native clients would be code paths nobody
  local-first can exercise.
- **Multimodal / image input.** Depends heavily on which provider
  lands; SSE chat with text is the lowest common denominator.
- **`enso serve` HTTP API** and **session-share URLs.** Different
  threat model from the unix-socket daemon (auth, CORS, TLS,
  hosting). The daemon's POSIX socket already covers the
  local-headless use case.
- **TypeScript / JS extension SDK.** MCP is the supported extension
  surface — adding a JS runtime is a bigger philosophy shift than
  the value justifies.
- **Tree-branching session navigation, `/undo`/`/redo`,
  ML-classifier permission mode.** High churn for unclear daily use;
  git already handles edit-undo.
- **GitHub Actions / cloud channels / Slack ingress.** If you want
  CI-driven enso, wire `enso run --format json` into your own
  action.
- **Cross-platform daemon (Windows named pipes).** Daemon is
  `!windows` build-tagged. Worth doing only if Windows users
  actually show up.
- **ACP / RPC over stdin/stdout.** Audience narrow; revisit if Zed
  or similar ACP hosts gain traction.
- **TUI tabs/splits.** Subagents already cover the
  parallel-conversation case via the agents pane.

*Bash sandboxing, vim-mode, and the hooks subset were originally
non-goals but shipped post-v1 once the daily-use case was concrete.
Anything on the list above moves the same way if real usage shows
the pain.*

## Soak-test risks

Areas where surprises are most likely. Look here first when
something feels off:

- **Qwen3 chat-template tool-call extraction** is fragile across
  llama.cpp versions. Client-side guards (the `<think>` tag-state
  machine, empty-assistant-message protection) catch most of it but
  new template breakage will surface as model output going to the
  wrong lane or as HTTP 400 on the *next* turn ("Assistant message
  must contain either 'content' or 'tool_calls'"). Guards log to
  `~/.enso/enso.log`; check there first.
- **Tool results are untrusted text — LSP results in particular.**
  `lsp_hover` / `lsp_diagnostics` / `lsp_definition` / `lsp_references`
  ship the language server's response back to the model verbatim as
  tool-call content. A docstring in a hostile dependency, or a hostile
  LSP binary itself, can plant text shaped like instructions, system
  prompts, or commands; the model may follow them. C1's workspace-trust
  gate blocks the install vector for `[lsp.*].command`, but the
  *content* vector (a docstring inside `node_modules/some-lib/`) is
  not gated. The same risk applies to `read` / `web_fetch` / `bash`
  output of attacker-controlled content — LSP isn't uniquely bad, it's
  just an easy-to-forget surface. Mitigations today: keep
  `bash.sandbox = "auto"` for hostile-code-review sessions, prefer
  `read` (whose contents the user sees rendered) over `lsp_*` for
  security-sensitive inspection, and don't grant unsupervised tool
  access to subagents that handle untrusted code. Fence-wrapping
  individual tool results was considered and rejected: it'd imply the
  unfenced tools were safe (they aren't), and a real fix would need
  per-call nonces around *every* tool result with system-prompt
  awareness — a prompt-engineering project on its own.
- **Bash deny-rule bypass via command substitution.** Deny rules are
  now segment-aware: `bash(rm -rf *)` correctly catches `do_evil; rm -rf /`,
  `cd / && rm -rf *`, `cd / || rm -rf *`, `ls | rm -rf *`, and newline-
  separated chains by splitting on top-level separators
  (`internal/permissions/allowlist.go` `bashSplitTopLevel`). The
  remaining bypasses — `$(rm -rf /)`, backticks, `eval "$cmd"`, here-
  docs, and anything inside shell control flow — are NOT closed,
  because closing them properly needs a full shell parser
  (`mvdan.cc/sh`-class) that we've punted on. **Deny rules are
  guardrails, not walls.** For adversarial inputs (hostile model,
  hostile dependency code being reviewed), `bash.sandbox = "auto"` is
  the boundary — the documented residual bypass classes are caught
  there because the entire shell session runs inside a container.
- **Workflow sibling parallelism** is goroutine-correct but not
  load-tested. Three-role pipelines work; large fan-outs (10+
  siblings) with shared output state under mutex are unexplored.
- **Slow-consumer drops on streaming deltas.** The chat events
  subscriber coalesces deltas into 16ms (~60fps) batches before
  redrawing — that should keep `bus: slow consumer` warnings at bay
  even on >100 t/s streams. If you see them in `~/.enso/enso.log`,
  the renderer is back-pressured for some other reason; bump the
  subscriber buffer or shorten the flush interval in
  `internal/tui/app.go`.
## Reference

- `README.md` — user-facing quickstart + feature overview (also covers the layered config search paths).
- `~/.enso/config.toml` (or layered equivalents — see `README.md` "Configuration") — runtime config.
- `~/.enso/enso.db` — SQLite session store.
- `~/.enso/enso.log` — slog output.
- `~/.enso/debug.log` — raw SSE chunks when `--debug`/`ENSO_DEBUG`.
