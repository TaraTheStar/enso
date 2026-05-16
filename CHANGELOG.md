# Changelog

All notable changes to ensō are documented here. The format is based
on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [2.2.0] - 2026-05-09

### Added
- **Provider pools (`[pools.<name>]`).** Providers behind the same
  endpoint now share one concurrency semaphore by default (one
  llama-swap = one pool, zero config), fixing the shared-hardware
  thrash where parallel calls to co-located models fought over a GPU.
  Override grouping with a per-provider `pool = "<name>"` and tune a
  pool with `[pools.<name>]` `concurrency` / `queue_timeout` (default
  300s; a request waits this long for a slot before erroring). Waiters
  are granted in FIFO order. Pools coordinate across **every session
  hosted by one `enso daemon`** (and a standalone process's sub-agents);
  separate daemon-less processes still don't coordinate — run the daemon
  if you need that. A `swap_cost` hint plus pool membership
  are surfaced to the model in the "## Available models" section so it
  stops ping-ponging between co-located models. The reserved cloud-limit
  keys `rpm` / `tpm` / `daily_budget` are parsed but not yet enforced
  (they warn once if set). See the Pools doc.
- **Auto-rendered "## Available models" prompt section.** When two or
  more `[providers.*]` are configured, ensō injects a section into the
  system prompt naming the model it's running as and listing the others
  with an optional new per-provider `description` hint plus pool
  membership and `swap-cost`, so the model can route work via
  `spawn_agent`'s `model:` arg and avoid expensive same-pool swaps.
  Slotted between the
  embedded default and `ENSO.md`, so a `replace: true` ENSO.md discards
  it too. Static for the session (a mid-session `/model` swap doesn't
  rewrite it). Opt out with `[instructions] include_providers = false`.
  See the Prompt layering doc.

### Changed
- **enso now follows the XDG Base Directory layout instead of `~/.enso`.**
  User-editable files (`ENSO.md`, `agents/`, `skills/`, `workflows/`)
  live under `$XDG_CONFIG_HOME/enso` (default `~/.config/enso`),
  app-managed data (`enso.db`, `memory/`) under `$XDG_DATA_HOME/enso`
  (default `~/.local/share/enso`), logs and `trust.json` under
  `$XDG_STATE_HOME/enso` (default `~/.local/state/enso`), and the daemon
  socket/pidfile under `$XDG_RUNTIME_DIR/enso`. Existing `~/.enso`
  installs must be moved into the new dirs by hand (prompt, agents,
  skills, workflows → config; `enso.db`, `memory/` → data; `trust.json`,
  logs → state).
- **Prompt layering is now append-by-default at every level.**
  `~/.config/enso/ENSO.md` previously replaced the embedded default
  system prompt entirely; it now appends to it, matching how project-
  level `<cwd>/ENSO.md` and `<cwd>/AGENTS.md` have always behaved. To
  restore the old replace semantics, add `--- replace: true ---`
  frontmatter at the top of the file. The same frontmatter works on
  project-level `ENSO.md` / `AGENTS.md` and discards every earlier
  layer (useful for team-shared canonical prompts). See the Prompt
  layering doc.

## [2.1.0] - 2026-05-09

### Changed
- TUI upgraded to Bubble Tea v2 with refreshed input handling and
  improved markdown rendering (semantic block colouring, diff-aware
  tool-result colouring).
- Session-inspector overlay now binds to `Ctrl-Space` (or `Ctrl-@`)
  instead of `Ctrl-A`; `Ctrl-A` is the readline-style "move to start of
  line" inside the input again.
- Status line gained a streaming-only `.TokensPerSec` template
  variable; the default template hides the segment when idle.

## [2.0.0] - 2026-05-09

Major release: the TUI was migrated off `rivo/tview` onto
`charmbracelet/bubbletea` + `lipgloss`. Completed messages now live in
your real terminal scrollback (so `tmux` highlight + middle-click copy
works exactly the way it does in any other pane), with a small live
region at the bottom for streaming output, the status line, and the
input. Overlays (file picker, session inspector, permission prompt,
recent-sessions list) take over the alt-screen only while open.

### Added
- Scrollback-native chat rendering via Bubble Tea / Lipgloss
  (`internal/ui/bubble`). The `internal/ui` surface is framework-agnostic;
  nothing outside `internal/ui/bubble/` imports Bubble Tea directly.
- Live tool-call status indicators with elapsed-time badges
  (`thought for 1.2s`, etc.) and live spinner.
- Semantic chat lanes: yellow bar for user input, plain assistant
  text, teal bar for tool calls, gray recede for reasoning, red `✘`
  for errors, teal parentheticals for system notes.

### Changed
- Default `enso` (and `enso tui`) now run the Bubble Tea backend; the
  old tview backend has been removed.

## [1.2.0] - 2026-05-09

Substantial post-v1 release focused on resilience, observability, and
operator ergonomics.

### Added
- Interactive `enso config init --wizard` flow for first-run
  onboarding (pick a provider preset, model, and optional API key).
- Slash commands `/export`, `/stats`, `/fork`, `/find`, `/rename`,
  `/info`, `/lsp`, `/mcp`, `/git`, `/cost`, `/transcript` for inline
  access to features previously only reachable from the CLI.
- `/find` overlay (and `Ctrl-F` in tview) for searching the current
  session's transcript; substring or `-e` regex.
- Auto-derived and manual session labels (rendered in the
  recent-sessions overlay; settable via `/rename <label>`).
- Live elapsed-time badges on in-flight tool calls.
- Cumulative token / cost tracking surfaced in the status bar and
  `/cost`; `.TokensPerSec` template variable for the status line.
- MCP server health tracking with sidebar error reporting (failed
  servers and their last error are visible at a glance).
- LLM connection state tracking with a background recovery probe — the
  status bar reflects connect / disconnect transitions, and the
  daemon / TUI keeps re-probing rather than failing the next turn
  silently.
- Daemon-side permission timeouts with TUI countdown indicators (the
  daemon auto-denies after the configured window if no client is
  attached, and the TUI shows the remaining seconds).
- Turn-scoped permission grants — the modal now offers a `t` decision
  ("allow for the rest of this turn") in addition to allow / remember /
  deny, so a chained tool call doesn't require a second prompt or a
  permanent rule.
- Untrusted-content marking in the TUI for tool results that came from
  external systems (LSP, web_fetch, hostile-code review) so the
  reviewer can spot prompt-injection vectors faster.
- Classified error reporting with retry countdowns (transport vs.
  protocol vs. cancellation each render distinctly).
- Workflow validation at parse time (cycles, dangling edges, duplicate
  role names) with clear error messages.
- Hook observability — `on_file_edit` / `on_session_end` failures and
  timeouts now surface as inline TUI notices instead of slog-only.

### Changed
- Bash deny rules are now segment-aware: `bash(rm -rf *)` correctly
  catches chained variants like `do_evil; rm -rf /`,
  `cd / && rm -rf *`, `ls | rm -rf *`, and newline-separated chains.
  Command-substitution / backtick / `eval` bypasses are still open by
  design — deny rules are guardrails, not walls; use
  `[bash] sandbox = "auto"` for adversarial isolation.
- System prompt now injects sandbox state and file-confinement details
  so the model knows which paths are reachable; sandbox-mode prompts
  include explicit Do/Don't path examples.
- Crash recovery overhaul: tool-call backfill is inline (synthetic
  "interrupted" results are inserted at load time) and shutdown
  ordering is deterministic — `kill -9` mid-tool-call now resumes
  cleanly without orphaned tool sequences.
- Input-discard handling: cancelling a turn flushes any queued user
  messages on the input channel and surfaces them as
  `(discarded N queued messages)` instead of replaying them out of
  order on the next turn.
- `Ctrl-C` is now gated on activity state — pressing it during a tool
  call cancels the turn; pressing it idle is a no-op (it no longer
  silently exits the app or no-ops mid-stream).
- The tview built-in undo/redo path is no longer intercepted by ensō's
  key handler.

### Fixed
- Several edge cases around bash patterns where prepending an allowed
  command segment (`do_evil; git status`) bypassed deny rules.
- Hook timeouts no longer leak goroutines on slow user scripts.

### Security
- Segment-aware bash deny rules close the most-common deny-rule
  bypass class. The remaining classes (command substitution, eval,
  here-docs) are documented as out-of-scope; sandbox mode is the
  recommended boundary for hostile-input sessions.

## [1.1.1] - 2026-05-07

### Fixed
- `Ctrl-C` handling moved up to the application level so it no longer
  exits ensō by accident when the focused widget happened to ignore
  the keypress.

## [1.1.0] - 2026-05-07

### Added
- `web_search` tool with two backends: DuckDuckGo's HTML endpoint by
  default (no signup, works anywhere with internet) and SearXNG when
  `[search.searxng] endpoint` is set. Backend selectable via
  `[search] provider` (`""` auto / `"searxng"` / `"duckduckgo"` /
  `"none"`).
- GitHub Actions release workflow that builds reproducible
  cross-platform binaries (Linux, macOS, Windows × amd64/arm64) on tag
  push and attaches them to the GitHub release.

## [1.0.0] - 2026-05-07

First public release.

### Added
- TUI agent (`enso tui`) and one-shot mode (`enso run`) for any
  OpenAI-compatible chat endpoint; default config targets a local
  `llama-server` running Qwen3.6-35B-A3B.
- Built-in tools: `read`, `write`, `edit` (with diff prompt),
  `bash`, `grep`, `glob`, `web_fetch`, `todo`, `memory_save`.
- Sandboxed `bash` tool with docker/podman backends, configurable
  per-project via `.enso/config.toml`.
- Permissions system with allow/ask/deny lists, per-user "Allow +
  Remember" persistence (`.enso/config.local.toml`, gitignored), and
  always-prompt overrides for high-blast-radius commands.
- Session persistence to SQLite with crash-safe resume.
- Pluggable LSP integration; `gopls` wired in by default for this
  repo.
- `enso config` (show / init / path) and `enso version` subcommands;
  `version` reports `runtime/debug.ReadBuildInfo()` for `go install`
  builds and a `git describe` string for `make build`.
- First-run welcome flow when no config exists, plus friendlier
  transport-level error messages naming the configured endpoint.
- Hugo documentation site (`docs/`) published to GitHub Pages.

### Security
- Private vulnerability reporting via GitHub Security Advisories;
  see [`SECURITY.md`](SECURITY.md).

[2.1.0]: https://github.com/TaraTheStar/enso/compare/v2.0.0...v2.1.0
[2.0.0]: https://github.com/TaraTheStar/enso/compare/v1.2.0...v2.0.0
[1.2.0]: https://github.com/TaraTheStar/enso/compare/v1.1.1...v1.2.0
[1.1.1]: https://github.com/TaraTheStar/enso/compare/v1.1.0...v1.1.1
[1.1.0]: https://github.com/TaraTheStar/enso/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/TaraTheStar/enso/releases/tag/v1.0.0
