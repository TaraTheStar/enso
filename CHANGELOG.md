# Changelog

All notable changes to ensō are documented here. The format is based
on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v2.5.0] - 2026-05-18

### Changed (BREAKING — config migration required)
- **Unified backend configuration.** Every backend's environment now
  lives under its own `[backend.<name>]` sub-table, selected by the
  single `[backend] type` switch. Switching backends is now a one-line,
  lossless change with a shared vocabulary (`init`, `extra_mounts`)
  across podman and lima. There is **no backward compatibility**: the
  old keys are silently ignored by the TOML decoder, so a stale config
  will run with *default* settings (e.g. the `alpine:latest` image and
  no `init`) until migrated.

  **Config scoping is now enforced.** `[backend] type` and
  `[backend] workspace` are selection/safety knobs and may be set in any
  config layer (system, user, or project — the user config is typical).
  The per-backend *environment* sub-tables (`[backend.podman]`,
  `[backend.lima]`, `[backend.egress]`) describe what the project needs
  and must be reproducible from the repo, so they are honored **only**
  from project-scoped config (`<repo>/.enso/config.toml`,
  `.enso/config.local.toml`, or an explicit `-c` file). Set in the user
  or system config they are **stripped with a one-time warning** — there
  is deliberately no user-global backend environment, so a personal
  `init` can never silently collide with a repo's.

  **Required migration actions:**
  - In your **user** config (`~/.config/enso/config.toml`) keep only
    `[backend] type` / `workspace`; move any `[backend.podman|lima|
    egress]` blocks out (they will now be ignored there).
  - In each **repo's** `.enso/config.toml`, set the environment:
    - `[bash.sandbox_options]` → split into:
      - `[backend.podman]` for `image`, `init`, `network`,
        `extra_mounts`, `env`, `name`, `workdir_mount`, `uid`,
        `hardening`
      - `[backend.podman] runtime` ← was `[backend] runtime`
      - `[backend.egress]` for `allow` (← `egress`) and `credentials`
    - `[lima]` → `[backend.lima]` (`template`, `cpus`, `memory`, `disk`,
      `extra_mounts`, and the new `init`)
  - `[backend] workspace` ← was `[bash.sandbox_options] workspace`
    (now backend-agnostic; user-settable).

  Example — **user** config:
  ```toml
  [backend]
  type      = "lima"
  workspace = "overlay"
  ```

  Example — **repo** `.enso/config.toml`:
  ```toml
  [backend.egress]
  allow = ["github.com"]

  [backend.podman]
  image = "golang:1.22"
  init  = ["apk add --no-cache git"]

  [backend.lima]
  template = "alpine"          # guest image distro (default)
  init     = ["apk add --no-cache git"]
  ```

### Added
- **Backend bring-up is no longer silent.** A cold lima run
  (cloud-image download + VM boot + provisioning + egress seal) used to
  show a blank terminal for minutes because `limactl` output was
  captured and discarded on success. Lima's native progress now streams
  live to stderr, plus concise enso phase lines (`preparing the
  workspace overlay copy…`, `creating/resuming Lima VM…`, `sealing the
  guest network…`, `Lima VM ready — starting the agent…`). stderr-only
  so `--format json` stdout stays clean; a bounded tail is still kept
  for actionable errors. Podman already streamed its pull progress and
  gains the workspace-copy framing.
- **Lima provisioning (`[backend.lima] init`).** Shell lines that run
  once during VM provisioning (rendered into the generated Lima
  instance YAML's `provision:` block, `mode: system`, with `set -e`).
  The podman `init` analogue — install toolchains the base template
  lacks without baking a custom image.
- **Colored post-session overlay diff.** The end-of-task
  commit/discard/keep prompt now renders the agent's diff with the same
  git-style syntax coloring used in the TUI scrollback (interactive
  path only).
- **Podman `[backend.podman] init` is now honored.** It was previously
  consumed only by the (now-removed) legacy sandbox manager. Each
  per-task `podman run` wraps the entrypoint: init runs in-container
  (output redirected to stderr so it can't corrupt the worker Channel;
  `set -e` so a failed init fails the task), then `exec`s the worker.
  Because podman containers are per-task `--rm`, init re-runs each
  task — keep it cheap (package installs) or bake heavy tooling into
  the image.
- **lima VM auto-recreate on config drift.** A persistent per-project
  lima VM is also rebuilt automatically when the generated instance
  config no longer matches the effective config (workspace toggled,
  lima settings changed, or an enso upgrade changed the mount layout) —
  complementary to the inode fix, for cases where the mount path or
  image itself changes. Safe: project state lives in the host workspace
  overlay; the VM is reproducible from `init`/provisioning.

### Removed
- **Legacy per-project sandbox.** The `internal/sandbox` package and
  the `enso sandbox list|stop|rm|prune` commands are gone. They drove a
  *persistent* per-project podman/docker container that predated the
  Backend seam; podman is now strictly per-task (`podman run --rm`),
  and that container model's only remaining value (init amortization)
  is moot now that `init` is package-install-cheap. **`enso sandbox
  prune` is replaced by `enso prune`** (same `--older-than`): it
  reclaims terminal podman task workers + volumes, enso lima VMs, and
  accumulated workspace review copies. The dead in-process
  `SandboxRunner`/`runBashSandboxed` bash path was removed too —
  isolation is the Backend, not an in-process branch.

### Security
- **Lima no longer mounts host `$HOME` into the guest.** Previously the
  lima backend inherited `template:default`, whose base chain
  (`template:_default/mounts`) binds the host home directory read-only
  into the VM — so the agent could read `~/.ssh`, `~/.aws`,
  `~/.config/enso` (provider API keys), and sibling repos. enso now
  inherits an **image-only** base (`template:_images/<distro>`, default
  **Alpine**) and mounts only the project copy (writable) and the
  read-only enso binary. With no `~` mount there is also no parent
  mount to shadow the workspace, so the prior read-only-workspace bug's
  cause is removed (not worked around).
  - `[backend.lima] template` now selects the guest **image distro**
    (`alpine` default, or `debian`/`ubuntu`/…), not an arbitrary Lima
    template; a path/URL is still used verbatim (you then own the mount
    posture). Extra guest packages go in `[backend.lima] init`.
  - On the default Alpine image enso auto-installs `iptables` (the
    cloud image omits it; the egress seal requires it) as a
    provisioning step ahead of your `init`.
  - Persistent per-project VMs created before this release still mount
    `$HOME`, but enso now **auto-recreates** any lima VM whose
    generated config no longer matches (see "lima VM auto-recreate" in
    Added) — the stale `$HOME`-mounting VM is rebuilt on the next run
    with no action required.

### Fixed
- **TUI text overflowed the right edge of the terminal.** Streaming
  model output (assistant + reasoning) and wide tool output were
  emitted raw until they graduated to the (glamour-wrapped) scrollback,
  and the single-line input had no width awareness — so typing past the
  terminal edge ran off-screen and you couldn't see what you were
  typing. Live blocks now hard-wrap to the terminal width (no markdown
  parse — cheap per delta; assistant continuation lines hang-indent
  under the prefix; unified diffs left raw, as git does). The input
  line is now a width-bounded, horizontally-scrolling window that keeps
  the cursor visible (ANSI/display-width aware via
  `github.com/charmbracelet/x/ansi`, so wide/CJK runes are handled).
  Regression tests assert no rendered line ever exceeds the terminal
  width.
- **lima overlay write-back / empty-project / missing commit prompt
  (persistent-VM inode stability).** The workspace overlay's `merged`
  directory now keeps a stable inode for the life of the per-project
  VM. Previously `workspace.NewAt` rotated its inode every run
  (rename-aside / `RemoveAll` + `MkdirAll`), but a persistent Lima VM
  9p-exports `merged` by the inode it had at boot (qemu virtfs doesn't
  re-resolve the path). So on any run that reused the VM, the guest
  mount was stranded on the orphaned old inode: the project showed
  empty/stale, agent writes never reached the host, and the end-of-task
  commit/discard/keep prompt never appeared (zero changes detected →
  silent discard) — most visibly right after a `commit`, whose
  `Cleanup()` removed `merged` outright. Now contents are refreshed in
  place and `Cleanup()` clears the lima copy without rmdir'ing it.
  Covered by a real-VM regression test exercising the reuse path
  (`TestOverlayReuseAndDrift_RealVM`).
- **`fatal error: invalid runtime symbol table` mid-session
  (lima/podman).** The isolated backends 9p/bind-mounted the host's
  *live* enso binary into the guest and exec'd it 1:1; a persistent
  lima VM keeps that mount across tasks. Rebuilding enso in place
  (`make build`/`make install`/`go install` — the Go toolchain
  O_TRUNC-overwrites the output) while a guest worker had it mmap'd
  made 9p fault in a mix of old+new pages, corrupting the Go runtime's
  pclntab → the fatal error, surfacing "every so often" (only when the
  runtime needs the pctab during a turn). enso now execs an
  **immutable, content-addressed snapshot** of the binary under
  `$XDG_STATE_HOME/enso/exe/<hash>/` (`internal/backend/exestage`),
  copied at most once: a rebuilt binary hashes differently → new path
  → the lima drift-recreate rebuilds the VM cleanly while any in-flight
  worker keeps running its own untouched copy. This also narrows the
  lima mount to a dir containing only enso (previously the whole
  `~/go/bin` was exposed read-only into the guest). Snapshots are
  reclaimed by `enso prune` (honoring `--older-than`).
- **Paste into the TUI did nothing (Ctrl-Shift-V / Cmd-V / middle-click
  X11 PRIMARY).** bubbletea v2 enables bracketed paste by default, so a
  terminal paste arrives as `tea.PasteMsg` rather than keystrokes — and
  the model's `Update` had no case for it, so the content was silently
  dropped. Added a `tea.PasteMsg` case that inserts at the cursor,
  gated exactly like typed text (ignored while a picker/permission/
  egress/overlay modal or vim-normal owns the keyboard). The input is
  single-line by design (Enter submits), so pasted newlines are
  flattened to spaces — a multi-line snippet stays usable without
  corrupting the single-line cursor/render. Mouse reporting stays off,
  so native terminal selection/copy is unaffected. (Plain Ctrl-V is
  not a terminal paste in raw mode and intentionally still does
  nothing — use the terminal's paste binding.)
- **Sessions not saved under podman/lima (no `--continue`, absent from
  the picker).** The agent Worker runs *inside* the VM/container, so
  its `session.Open()` resolved to the guest's filesystem: the host
  created the session row + audit rows but the `messages` rows were
  written to a throwaway guest DB the host never sees. The picker
  (`HAVING msg_count > 0`) hid such sessions and `--continue` loaded an
  empty history. Fixed by streaming persistence over the Channel: an
  isolated worker (`Isolation.Kind != ""` — now also set for podman)
  ships each append as `MsgPersistMessage`/`MsgPersistToolCall`; the
  host applies them to the host DB via a new `host.WithWriter`. Resume
  history is shipped to the worker via `spec.ResumeHistory` (the guest
  DB is empty so it can't `session.Load`). The **local backend is
  unchanged** (shared FS, direct writes — no double-write, since the
  host only applies persist envelopes, which the local worker never
  sends). Proven by a real-sealed-container e2e test
  (`TestPodmanRemoteSessionPersistence`).
- **lima worker not reaped on terminal close (SIGHUP).** Teardown was
  reached only via `defer sess.Close()`. bubbletea handles SIGINT/
  SIGTERM (returns + restores the terminal, defers run), but **SIGHUP**
  (terminal/tab closed, parent shell exited) is unhandled and its
  default action killed enso before the defer could run. The backend
  worker's `limactl`+ssh process group is deliberately `Setpgid`'d off
  enso's terminal so a terminal SIGHUP can't kill it mid-task — but
  with Teardown skipped, nothing reaped it, so lima kept the SSH
  session into the VM open ("lima still holding something open"). Both
  the TUI and `enso run` now catch SIGHUP and run the (idempotent)
  Teardown before exiting; SIGINT/SIGTERM are still left to bubbletea
  so its terminal restore is not regressed.

## [v2.4.0] - 2026-05-18

### Added
- **Unification of Egress Broker & Lima Isolation.** Finalized the
  integration of the egress broker logic across all sealed backends
  (podman and lima), ensuring consistent behavior for network
  isolation and interactive egress prompting.
- **Interactive Egress Prompting.** Implemented a new TUI interaction
  for managing outbound network requests in attended sessions. Users
  can now choose to `[y]es once`, `[t] this task` (memoize for the
  session), or `[n]o` (refuse) when a sandboxed command attempts to
  reach a non-allowlisted target.

### Changed
- **Lima Backend Network Sealing.** Upgraded Lima isolation from
  "inference-only" protection to genuine network sealing. The guest
  egress is now firewalled by default to prevent uncontrolled
  outbound traffic, using a host-side proxy for any authorized
  connections.
- **Egress Proxy Architecture.** Introduced a unified `EgressProxy`
  and `EgressBroker` system. This allows for a central, observable,
  and policy-enforced gateway for all outbound traffic from sealed
  sandboxes.
- **Enhanced Documentation.** Updated `docs/content/docs/sandbox.md`
  to include detailed information on the new Egress, Static
  Allowlist, Interactive Prompting, and `--yolo` modes.

### Fixed
- **Lima Backend Shell Injection.** Fixed the worker launch process
  to correctly inject `HTTPS_PROXY` and `HTTP_PROXY` environment
  variables via `env`, enabling communication through the host
  egress proxy.

## [v2.3.0] - 2026-05-16

### Added
- **`[backend] type = "lima"` — real-VM isolation.** The whole agent
  runs inside a Lima VM (macOS vz/qemu, Linux qemu+KVM, Windows
  wsl2), host-proxied inference, project mounted at its real path.
  The VM is **persistent per project** (`enso-<base>-<hash>`):
  created once, resumed for later tasks (substrate reused — first run
  pays an image download + boot, later runs start fast). Per-task
  *workspace* isolation is still total via the overlay. Tunables
  under `[lima]` (`template`/`cpus`/`memory`/`disk`/`extra_mounts`).
  Fail-safe: refuses to start (no silent downgrade) if `limactl` is
  absent. Reclaim VMs explicitly with `enso sandbox prune`
  (`--older-than` honored); they are never auto-deleted. See the
  Sandbox doc for the carry-forward tradeoff.
- **`[backend] runtime`** selects the container CLI for
  `type = "podman"`: `"auto"` (default — podman, falling back to
  docker), `"podman"`, or `"docker"`.

### Changed
- **Workspace overlay is now a true three-way merge.** With
  `[bash.sandbox_options] workspace = "overlay"`, task end compares a
  pristine baseline against both the agent's copy and the live
  project. Non-conflicting agent changes apply **per file**
  (create/modify/delete) — there is no longer a blanket
  `rsync --delete` from a stale snapshot, so editing the project (or
  running `git`) while the agent works no longer risks clobbering
  your concurrent work. Files both sides changed are reported as
  conflicts and kept for manual merge; `[f]orce-all` (typed
  `overwrite`) is required to override them. The resolve prompt now
  shows a real unified diff with a `[v]iew` full-diff option.
  Superseded `merged.kept-*` review copies are capped (3 most
  recent) and swept by `enso sandbox prune`.

### Removed
- **BREAKING: `[bash] sandbox` is removed.** `[backend] type` is now
  the sole backend selector (`"local"` default, `"podman"`,
  `"lima"`); the legacy `[bash] sandbox` → backend derivation is
  gone and the key is silently ignored. **Migration:** delete
  `[bash] sandbox` and set `[backend] type`; if you relied on
  `sandbox = "podman"`/`"auto"`, set `[backend] type = "podman"`
  (optionally `[backend] runtime`) — until you do, you run with **no
  isolation**. An unrecognized `[backend] type` still fails safe to
  `local` and is flagged loudly on stderr. `[bash.sandbox_options]`
  (image/init/network/mounts/workspace/hardening/…) is unchanged.

## [v2.2.0] - 2026-05-09

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

## [v2.1.0] - 2026-05-09

### Changed
- TUI upgraded to Bubble Tea v2 with refreshed input handling and
  improved markdown rendering (semantic block colouring, diff-aware
  tool-result colouring).
- Session-inspector overlay now binds to `Ctrl-Space` (or `Ctrl-@`)
  instead of `Ctrl-A`; `Ctrl-A` is the readline-style "move to start of
  line" inside the input again.
- Status line gained a streaming-only `.TokensPerSec` template
  variable; the default template hides the segment when idle.

## [v2.0.0] - 2026-05-09

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

## [v1.2.0] - 2026-05-09

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

## [v1.1.1] - 2026-05-07

### Fixed
- `Ctrl-C` handling moved up to the application level so it no longer
  exits ensō by accident when the focused widget happened to ignore
  the keypress.

## [v1.1.0] - 2026-05-07

### Added
- `web_search` tool with two backends: DuckDuckGo's HTML endpoint by
  default (no signup, works anywhere with internet) and SearXNG when
  `[search.searxng] endpoint` is set. Backend selectable via
  `[search] provider` (`""` auto / `"searxng"` / `"duckduckgo"` /
  `"none"`).
- GitHub Actions release workflow that builds reproducible
  cross-platform binaries (Linux, macOS, Windows × amd64/arm64) on tag
  push and attaches them to the GitHub release.

## [v1.0.0] - 2026-05-07

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

[v2.5.0]: https://github.com/TaraTheStar/enso/compare/v2.4.0...v2.5.0
[v2.4.0]: https://github.com/TaraTheStar/enso/compare/v2.3.0...v2.4.0
[v2.3.0]: https://github.com/TaraTheStar/enso/compare/v2.2.0...v2.3.0
[v2.2.0]: https://github.com/TaraTheStar/enso/compare/v2.1.0...v2.2.0
[v2.1.0]: https://github.com/TaraTheStar/enso/compare/v2.0.0...v2.1.0
[v2.0.0]: https://github.com/TaraTheStar/enso/compare/v1.2.0...v2.0.0
[v1.2.0]: https://github.com/TaraTheStar/enso/compare/v1.1.1...v1.2.0
[v1.1.1]: https://github.com/TaraTheStar/enso/compare/v1.1.0...v1.1.1
[v1.1.0]: https://github.com/TaraTheStar/enso/compare/v1.0.0...v1.1.0
[v1.0.0]: https://github.com/TaraTheStar/enso/releases/tag/v1.0.0
