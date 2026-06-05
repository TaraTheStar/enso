# Changelog

All notable changes to ensō are documented here. The format is based
on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Structured workflow role outputs.** A workflow role can now expose
  named fields, not just raw text. Fields are parsed from the role's
  final message — the last fenced ` ```json ` block (decoded as a flat
  object) or, failing that, contiguous trailing `KEY: value` lines — and
  read downstream as `{{ .<role>.<field> }}`. The raw text remains
  available as `{{ .<role>.output }}`, so existing workflows are
  unaffected. Missing fields render empty rather than erroring, and
  `output`/`skipped` are reserved names. `Result.Fields` carries the
  parsed fields to programmatic callers.
- **Conditional workflow edges.** An edge may carry an `if` guard —
  `from -> to if '<predicate>'` — that decides at runtime whether the
  edge fires. A node runs only if **all** of its incoming edges fire
  (strict AND); otherwise it is skipped, and skips propagate down the
  graph. A skipped role makes no LLM call, prints `role: (skipped)`, and
  exposes `{{ .<role>.skipped }}`. Predicates are Go `text/template`
  expressions over the same context as role bodies, with `eq`/`ne`/`and`/
  `or`/`not` plus `contains` (case-insensitive substring) and `matches`
  (regexp); missing fields evaluate as not-satisfied rather than
  aborting. This unlocks ship-vs-escalate routing on a reviewer's
  structured verdict — see the new `examples/workflows/gated-ship.md`.
  `/workflow validate` now reports the conditional-edge count, and both
  the TUI and `enso run` surface which roles were skipped.

### Changed

- Dependency bumps: `google.golang.org/genai`, `modernc.org/sqlite`,
  `github.com/anthropics/anthropic-sdk-go`, and the AWS SDK
  (`config`, `service/bedrockruntime`).

### Fixed

- **Tool-registry data race under parallel workflows.** The registry's
  cached tool-definition list is now guarded by a `sync.RWMutex`, so
  concurrent sibling roles building their child agents no longer race on
  it (caught by `go test -race`).

## [v2.11.0] - 2026-06-02

### Added

- **`/rewind` — undo to an earlier turn.** A new slash command opens an
  overlay to roll the session back to any earlier turn. Stage one picks
  the turn (each per-turn checkpoint shows its turn number, a preview of
  that turn's message, and a relative timestamp; defaults to the most
  recent). Stage two picks what to restore: **[1]** code + conversation,
  **[2]** conversation only (keep files), or **[3]** code only (keep
  conversation). Restoring code mirrors the workspace snapshot taken just
  before that turn back over the working tree — reverting modified files
  and deleting files added since — while never touching `.git` (history
  is left to git). Before truncating the conversation the prior thread is
  preserved as a forked session (the overlay prints how to resume it
  with `enso --session <id>`), and the rewound-away message is
  pre-filled into the input so it can be re-sent or edited. Unavailable
  under `--ephemeral`.
- **Per-turn workspace checkpoints.** On every genuine user turn (not
  auto-recovery nudges or sub-agent runs) enso snapshots the project tree
  before any inference runs, so `/rewind` can restore exact prior state.
  Snapshots live under `$XDG_STATE_HOME/enso/checkpoints/<session>/<seq>/`
  and use `cp -a --reflink=auto` — near-free on copy-on-write
  filesystems, a real copy otherwise. Tunable via a new `[checkpoints]`
  block: `disabled` (bool, default `false` — checkpointing is on by
  default) and `retain` (int, default `20` per session; older snapshots'
  DB rows and on-disk trees are pruned). On isolated backends snapshots
  capture the overlay's host-side tree, so only conversation rewind is
  available when no overlay is in use. `enso prune` now also sweeps
  orphaned snapshot trees left by discarded sessions and honours
  `--older-than`.
- **Multimodal image input.** Attach an image to a message by typing an
  `@path` mention pointing at an image file (e.g. `look at @diagram.png`);
  the `@` file picker now inserts `@<path>` mentions. Supported formats
  are PNG, JPEG, GIF, and WebP up to 10 MiB each; other paths stay plain
  text. Images are resolved host-side and crossed to isolated workers as
  bytes, so sealed backends that can't see your filesystem still receive
  them, and a `📎 attached` notice is shown in scrollback. The `read`
  tool likewise now reads images: handing it an image path shows the
  image directly to a vision-capable model.
- **Slash-command palette.** Typing `/` on an empty input line opens a
  filtered, navigable list of every registered command (built-ins, user
  and project commands, and loaded skills) with one-line descriptions —
  prefix matches first, then substring. Type to filter, arrows or
  `Ctrl+P`/`Ctrl+N` to move, `Enter`/`Tab` to insert, `Esc` to dismiss.
  A discoverability surface over the existing commands; no commands were
  added or removed.
- **Live status-line badges.** The status line now always shows context
  usage (`ctx N%`, brightening past 80% as a pre-compaction heads-up) and
  session usage — cumulative spend (`$0.0123`) on a priced provider or
  cumulative tokens (`Σ 15k`) on a free/local one — so `/context` and
  `/cost` are no longer needed for an at-a-glance read.
- **"esc to interrupt" hint.** While a turn is in flight with an empty
  input line, the status line shows `· esc to interrupt`, then `· press
  esc again to stop` once armed (a two-stage chord). Suppressed during a
  pending permission/egress prompt, where `Esc` means deny.
- **Scrollable overlays.** The `@` file picker, the `/` slash palette,
  and the recent-sessions overlay now scroll with the selection on short
  terminals — with an `↑N ↓M more` footer — instead of overflowing.
- **Large-file streaming in `read`.** Files over 10 MiB now stream a
  bounded window (10 MiB / 50,000-line caps) instead of being slurped
  whole, preventing OOM on multi-gigabyte files and logs; the result
  reports a range like `lines 1-N (large file, <size>, capped)`.
  Pathologically long lines are clipped rather than erroring.

### Changed

- **Resume and `/sessions` are now scoped to the current directory.**
  `/sessions` lists only sessions started in the current directory by
  default (pass `--all` for every directory, mirroring `/grep`), and
  `--continue` resumes the most recent session in the current directory.
- **Resume and `/transcript` replay tools and reasoning.** Resumed
  sessions and transcript replay now reconstruct tool calls and assistant
  chain-of-thought into scrollback instead of showing only user/assistant
  prose. The reasoning is persisted for replay only and is never sent
  back to the model.
- **Quitting now requires confirmation.** An idle `Ctrl+C`, and `Ctrl+D`
  on an empty line, now need a confirming second press within 3 seconds
  so a reflexive tap no longer discards the session.
- **"Remember" rules for `read`/`grep`/`glob` are project-scoped.**
  Previously "Allow + Remember" derived `<tool>(**)`, which matched
  `/etc/passwd`, `~/.ssh`, and enso's own config. Now a path inside the
  session cwd yields `<tool>(<cwd>/**)`, a path outside cwd yields an
  exact-path rule, and `glob` remembers the exact pattern you ran —
  remembering a read no longer silently grants whole-filesystem access.
- **"Always"/"Turn" grants reach the worker's enforcing checker.** On the
  local backend, "Allow + Remember" / "Allow for turn" now RPC the grant
  to the worker-side checker before the decision is sent, so the next
  call is actually gated by the new rule; only true attach mode (no wire
  path) degrades to allow-once with a notice.
- **Security config arrays are unioned across config tiers, not
  replaced.** `permissions.allow`/`ask`/`deny` and `web_fetch.allow_hosts`
  now merge (deduped, grow-only) across layers, so a higher-priority
  layer can no longer wipe a more-trusted tier's deny list with
  `deny = []`. Deny still wins in matching.
- **`config.local.toml` is now trust-gated.** `.enso/config.local.toml`
  is trust-prompted like `config.toml` (a hostile repo can commit one and
  it loads at higher priority); enso's own config writes record the new
  hash as trusted so they don't re-prompt. The user/system-layer strip of
  project-only `[backend.podman|lima|egress]` env is now case-insensitive.
- **Bash deny rules are much harder to evade.** Deny matching now tests
  each command segment both raw and normalized — collapsed whitespace,
  path-to-basename (`/bin/rm` → `rm`), removed shell-escapes
  (`\rm`, `r\m`), unquoted command words (`"rm"`), and re-split command
  substitution bodies (`$(...)`, backticks). So `bash(rm -rf *)` now also
  catches `do_evil; /bin/\rm -rf /` and `$(rm -rf /)`. Interpreter
  indirection (`eval`, `sh -c`, `xargs`) and here-docs remain out of
  scope.
- **Allowlist path matching cleans `..` traversal.** Patterns and paths
  are `filepath.Clean`ed before matching, so `read(/repo/**)` no longer
  matches `/repo/../etc/passwd`.
- **`read` rejects directories** up front with `read <path>: is a
  directory` instead of failing later, and refuses oversized images with
  `[image too large to inline]` rather than emitting binary.

### Fixed

- **`bash` no longer exposes credentials to the model.** The local-backend
  `bash` child environment is now credential-scrubbed: every `ENSO_*` var
  and any name containing `API_KEY`/`SECRET`/`TOKEN`/`PASSWORD`/
  `CREDENTIAL`/`PRIVATE_KEY`/`ACCESS_KEY` is dropped, so the model can't
  `echo $OPENAI_API_KEY`. `PATH`/`HOME`/`LANG`/toolchain vars survive.
  Genuine tokens like `GITHUB_TOKEN` are hidden too — use a credential
  helper.
- **SSRF protection on the host egress proxy.** The egress proxy now
  resolves each target once, refuses to connect if any resolved IP is a
  denied class (loopback, RFC1918/ULA, link-local incl. the
  `169.254.169.254` cloud-metadata address, CGNAT, broadcast, multicast),
  then dials the pinned IP literal (DNS-rebind defense). This stops a
  sealed worker under `--yolo` from relaying through the proxy into
  host-loopback or cloud metadata; explicitly allowlisted targets (e.g.
  your host-loopback model server) are exempt, `AllowAll` is not. The
  classification is shared with `web_fetch` via a new `internal/netsec`
  package.
- **Tool-call order is now deterministic for prefix-cache stability.**
  Completed tool calls are returned in streamed-delta-index order rather
  than Go's randomized map order. Reordering multi-call turns diverged the
  re-serialised assistant message from the server's cached KV and forced a
  full prompt reprocess; sorting keeps the llama.cpp prefix cache intact
  across turns.
- **Vertex/Gemini parallel tool calls no longer collide.** Gemini omits
  tool-call IDs, so two parallel calls to the same tool both got
  `call_<name>` and results could be mis-matched (also breaking
  compaction boundaries and cross-provider `/model` swaps). IDs are now
  synthesised as `call_<name>_<idx>` from a per-stream counter.
- **Session message-sequence races.** The shared session `Writer` could
  hand out the same `seq` from an unlocked `seq++` across the agent,
  sub-agents, and the persistence goroutine, colliding on the
  `(session_id, seq)` key and silently dropping a message. Counters are
  now mutex-guarded and token usage is attributed to an explicit `seq`.
- **Migration framework hardened.** The migration version is now the
  filename's numeric prefix rather than its sorted position (so adding,
  removing, or gapping files can't shift versions and re-run or skip a
  migration), duplicate versions are rejected, and each migration body and
  its `user_version` bump run in one transaction so a mid-file failure
  rolls back cleanly.
- **`edit` rejects an empty `old_string`** (`edit: old_string must not be
  empty`) instead of splicing `new_string` between every byte under
  `replace_all`.
- **Malformed permission patterns no longer panic.** A pattern with `(`
  but no closing `)` (e.g. `bash(rm`) now errors with "missing closing
  ')'" instead of panicking.
- **The `@` file picker no longer drops typed text.** Cancelling the
  picker (Esc, or Enter with no match) restores the typed `@<filter>`
  text as literal input instead of discarding it.

## [v2.10.1] - 2026-05-30

### Added

- **Up / Down arrow navigation in the input.** `↑` / `↓` now move the
  cursor a visual row at a time through soft-wrapped and multi-line
  input, keeping the display column (clamping into shorter rows). On the
  top row `↑` jumps to the buffer start; on the bottom row `↓` jumps to
  the end. Complements the existing `←`/`→`/`Home`/`End`/`Ctrl+←`/`Ctrl+→`
  horizontal motions.
- **Foreground-polling nudge for `bash`.** Extending the v2.10.0
  hang-prevention check, a foreground command that only *waits* — a
  leading `sleep` of 10s or more, or a `while`/`until` loop that sleeps
  (the "chain shorter sleeps to poll" pattern) — is no longer run as-is;
  `bash` returns immediately with a nudge to run the real work in the
  background and poll it with `bash_output` instead of blocking the turn.
  Short startup waits (`sleep 2 && curl …`), trailing/keep-alive sleeps,
  backgrounded `sleep &`, finite `for` loops, and any command already
  bounded by a `timeout` wrapper are left alone; an explicit `timeout`
  arg bypasses the check.

### Fixed

- **Web tools now honour the egress proxy in sealed backends.** In a
  sealed backend (podman/lima) the guest's firewall permits only the
  host egress proxy, and name resolution is meant to happen host-side
  through it. Two paths bypassed that and dialed directly, which the
  firewall then rejected with `operation not permitted` on udp :53 (an
  iptables EPERM that *looked* like a DNS failure): (1) `web_search`
  built a custom HTTP transport whenever a SearXNG `ca_cert` /
  `insecure_skip_verify` was configured and dropped `HTTPS_PROXY` in the
  process; (2) `web_fetch` always resolved and pinned the target IP
  in-guest for its SSRF guard — a model that can't work when the guest
  can't resolve. Both now route through the injected proxy when one is
  set (the host allowlist / interactive broker is the single egress
  gate, exactly as for `bash`'s `curl`/`git`); `web_fetch`'s in-guest
  IP-pinning SSRF guard is retained for the `local` backend only, where
  tools share your network and there is no proxy.
- **TUI input no longer panics when the cursor sits on a newline.**
  Moving the cursor onto a `\n` in multi-line input could trip a
  `slice bounds out of range` panic and tear down the program: a newline
  byte belongs to no soft-wrap row, so the cursor's row fell through to
  the last row and the renderer sliced a backwards range. The cursor is
  now attributed to the line the newline terminates and drawn as an
  end-of-line cell.

## [v2.10.0] - 2026-05-29

### Added

- **Agentic generation guards + auto-recovery.** Three hardware-
  independent guards keep a local model from wedging a turn, all tunable
  under a new `[providers.<name>.generation]` block. (1) A **max-tokens
  cap** (`max_tokens`, default `0` → `min(16384, context_window/2)`) is
  the hard backstop against a model that stops emitting EOS and runs to
  the context ceiling. (2) A **mid-stream loop guard** (`loop_guard`,
  default on) detects degeneration loops — a short unit repeated back to
  back ("the the the", a duplicated line, a JSON fragment) — and aborts
  before the stream even reaches the cap; the scan is cheap and
  rate-independent (inspects a bounded rune tail, only every N runes) and
  tuned to ignore legitimately repetitive code. (3) A **stall watchdog**
  (`stall_timeout`, default `60s`, `"0s"` disables) aborts a stream that
  emits no token for the window — it fires on *silence*, not slowness, so
  prompt-processing pauses and speculative/MTP bursts are tolerated. On a
  length-truncation, tripped loop guard, or stall, **auto-recovery**
  (`auto_recover`, default on; `max_recover_attempts`, default `2`)
  retries the turn with a nudge instead of dropping it. By design these
  are token-count / stall-on-silence based rather than wall-clock, so the
  same defaults hold whether the box is GPU-fast or big-and-slow.
- **Explicit tool-call timeouts.** A foreground `bash` command now runs
  under a wall-clock budget (default `120s`, configurable via
  `[bash] command_timeout`); on expiry its whole process group is killed
  and the tool returns the partial output plus a hint, so a runaway
  test/server no longer hangs the agent until an operator intervenes. A
  command that finishes on its own but is slow (a big test suite) is run
  in the foreground with the tool's `timeout` arg raised — the value is
  honoured as given, up to the `[bash] command_timeout_max` runaway
  backstop (default `1h`). MCP tool calls get the same treatment via
  `[mcp.<name>] call_timeout` (default `120s`). Set `command_timeout` to
  `"0s"` to opt out of the bash timeout entirely.
- **Background bash mode.** `bash` accepts `run_in_background: true` to
  start a command detached and return immediately with a job id — for dev
  servers, file watchers, and long builds that shouldn't block the turn.
  Two new tools, **`bash_output`** (read new output + status since the
  last read) and **`bash_kill`** (SIGKILL the job's process group),
  manage the job. Still-running background jobs are reaped when the
  session or sub-agent ends.
- **Hang-prevention for foreground commands.** A new system-prompt rule
  steers the model away from commands that can't return on their own, and
  a mechanical pre-run check catches the common offenders (`tail -f`,
  `watch`, `journalctl -f`, `logs --follow`, dev servers) — instead of
  running them and waiting out the timeout, `bash` returns immediately
  with a nudge to use `run_in_background`. Commands that already bound or
  detach themselves (a `timeout` wrapper, `&`, `nohup`, a pipe into
  `head`) are left alone, and an explicit `timeout` arg bypasses the
  check.

### Fixed

- **Context-window overflow: compaction now reserves the output budget.**
  Auto-compaction triggered at a flat `0.75 × context_window`, which
  ignored that the model's reply (`max_tokens`) has to fit in the *same*
  window. With a large `max_tokens` (e.g. 64K on a 256K window) the reply
  consumed the entire 25% headroom, so a prompt near the threshold plus
  the reply overflowed the real ceiling and the server rejected the turn.
  The trigger is now `context_window − max_tokens − margin`, so input and
  output are guaranteed to share the window.
- **Context-window overflow now self-corrects instead of dead-ending.** As
  a backstop, when a request still exceeds the window the server's `400`
  ("exceeds the available context size (N tokens)") used to surface as a
  hard error. enso now detects that rejection, parses the real limit `N`
  the server reports, adopts it as the effective context window for that
  provider, force-compacts against it, and retries the turn (bounded).
  This recovers automatically when `context_window` is unset or wrong —
  common behind a litellm/proxy that hides the model group's true limit.
  Detection covers the litellm, OpenAI (`context_length_exceeded`), vLLM,
  and llama.cpp phrasings.
- **Keep-alive race no longer surfaces as a dead turn.** A bare `EOF` /
  `unexpected EOF` from the provider HTTP call is now classified as a
  transport-level "connection closed" so `doChatRequest` re-issues on a
  fresh connection. The common trigger in long-running sessions is a
  keep-alive race — a proxy (e.g. uvicorn's 5s `--timeout-keep-alive`)
  closes an idle pooled connection that enso then writes a request onto.
  Go's transport only auto-retries non-idempotent POSTs when *nothing*
  was written, so an EOF noticed after the request bytes go out wasn't
  retried for us. Safe because an EOF from `Do()` means no response
  streamed, so the retry can't duplicate output.

## [v2.9.0] - 2026-05-28

This release lands **end-to-end inference cancellation** (Ctrl-C now
actually aborts the upstream HTTP call instead of waiting it out),
adds **`enso config init --project`** for one-shot scaffolding of a
language-tuned backend environment, and tightens the TUI's prompt /
cancel UX with pinned status-line hints, an Enter-commits default,
and a Ctrl-C chord that distinguishes "cancel turn" from "force quit".

### Added

- **`enso config init --project`** — scaffolds `<cwd>/.enso/config.toml`
  with `[backend.podman]` / `[backend.lima]` / `[backend.egress]`
  blocks tuned to the detected language. Flags: `--lang
  go|node|python|rust|generic` (empty auto-detects from `go.mod`,
  `package.json`, `pyproject.toml` / `requirements.txt`, `Cargo.toml`);
  `--backend podman|lima` selects which backend block is emitted
  uncommented (the other is included commented as a hint). Interactive
  under a TTY, deterministic under pipes / CI. File modes match the
  user-config path: `0700` parent dir, `0600` file.
- **Pinned status-line hints for unresolved prompts.** While a
  permission or egress prompt is awaiting an answer, the status line
  above the input shows `▸ awaiting: <tool>(…)   [y/n/a/t]` (with the
  auto-deny countdown when applicable), appended to the existing
  spinner / model name. The full prompt still prints to scrollback;
  the hint is the always-visible reminder when heavy streaming pushes
  it off-screen.
- **Enter = "yes" default** on permission and egress prompts. The
  default choice is rendered with a `▸ ` cursor glyph; the other
  choices are dimmed.
- **Ctrl-C cancels in-flight turns**, with a force-quit escape hatch.
  First Ctrl-C while a turn is busy sends the cancel and prints
  `(cancelling turn — press Ctrl-C again to force quit)`. A second
  Ctrl-C within ~500 ms force-quits even if the cancel itself wedged
  (e.g. a provider not honouring `ctx`). Idle Ctrl-C still quits
  immediately.

### Changed

- **`Registry.ToolDefs()` returns tools sorted by name**, memoized on
  the registry. Go's randomized map iteration was reshuffling the
  serialized tools array each turn and busting the prompt-prefix
  cache (llama.cpp, Anthropic ephemeral cache) — sorting makes the
  prefix byte-stable across turns.
- **Permission / egress `[n]o` no longer rendered in red.** Deny is
  the safe outcome, not the destructive one; red was misleading.
  Dim (`noticeStyle`) for non-default choices, bold + cursor for the
  default.

### Fixed

- **Inference cancellation race.** `agent.Cancel()` is now guaranteed
  to abort the host-proxied provider HTTP call. Previously the worker
  would emit `MsgInferenceCancel` and the host would look up the corr
  in `s.infCancel` — but the per-corr canceller was registered inside
  the `serveInference` goroutine, so a cancel arriving before that
  goroutine scheduled would find an empty map and be silently dropped.
  The canceller is now installed synchronously in the demux loop
  before the goroutine is launched.
- **Stale TUI prompt pin after a cancelled turn.** When a turn was
  cancelled or errored mid-prompt, `m.perm` / `m.egress` were not
  cleared, so the status line stayed pinned to `▸ awaiting: …` and
  `handleKey` kept intercepting `y/n/a/t` for a dead resolver. Both
  are now cleared on `EventCancelled` / `EventError`, with a
  best-effort Deny sent in case the agent goroutine was still
  blocked on `req.Respond`.
- **Project config file modes.** `enso config init --project` used
  to write `0o755` parent / `0o644` file; tightened to `0o700` /
  `0o600` (with a `chmod` clamp on pre-existing dirs) to match the
  user-config path. Project configs can carry project-scoped
  provider overrides (api_key, endpoint creds).
- **`os.Stat` error handling on config init pre-existence check.**
  A non-`ErrNotExist` Stat error (`EACCES` on a parent dir, weird
  ACLs) was treated as "file doesn't exist" and let `WriteFile`
  proceed without `--force`. Now propagates as a clear `stat <path>:
  <err>` failure. Fix applied to both the user-config and
  `--project` paths.
- **Silent flag coercions in `enso config init --project`.** An
  invalid `--backend` value (e.g. `--backend docker`) used to be
  silently coerced to `podman`; now returns an error listing valid
  values. Passing both `--project` and `--wizard` used to silently
  drop `--wizard`; now returns an explicit error explaining that
  `--wizard` writes provider config (user-scoped) and `--project`
  writes backend env (project-scoped).
- **Spurious `MsgInferenceCancel` on the wire.** When a stream
  completed cleanly at the same instant the caller's `ctx` was
  cancelled, Go's randomized `select` could fire a cancel envelope
  for an already-completed corr. The cancel-watch goroutine now
  double-checks `entry.done` before sending.

## [v2.8.0] - 2026-05-23

This release focuses on **context-management parity with state-of-the-art
coding agents** (real token accounting, structured compaction, contextual
instructions), **live LSP integration** for instant feedback on edits, and
a **first-class observer protocol** so third-party tools can visualise and
react to agent activity without embedding into enso itself.

### Added

#### Context management

- **Real per-message token accounting.** Provider-reported token counts
  (input / output / cache-read / cache-write / reasoning / total) are
  parsed from every adapter (`anthropic`, `anthropic-bedrock`,
  `anthropic-vertex`, `bedrock`, `openai`, `vertex`) and stored alongside
  each assistant message in a new `message_usage` table (migration auto-
  applied on session-DB open). The agent's compaction threshold,
  status-line token display, and resume hydration all switch from the
  4-char heuristic to real numbers; the heuristic stays as a last-resort
  fallback when usage is missing (first turn, or a provider that doesn't
  report it).
- **Structured Markdown compaction summaries.** The summariser is now
  prompted for a fixed seven-section structure (`## Goal`,
  `## Constraints & Preferences`, `## Progress` with Done/In Progress/
  Blocked subsections, `## Key Decisions`, `## Next Steps`,
  `## Critical Context`, `## Relevant Files`). Long sessions stay
  legible across multiple compactions.
- **Update-mode compaction.** On the second and later compactions of a
  session, the summariser is fed the prior summary alongside the new
  events and asked to *update* the seven sections rather than re-summarise
  from scratch. Detection is anchored on the new `Synthetic` flag (with a
  legacy bracket-prefix fallback for pre-flag session DBs). Stops the
  "summary of a summary" lossy-degradation problem.
- **Token-budgeted recent-turn tail.** Compaction's pinned-tail size is
  now driven by `min(8000, max(2000, ctxWindow * 0.25))` tokens rather
  than a fixed 6 turns. A session that just emitted a 20 KB grep result
  pins fewer turns; a session of tight conversation pins more.
- **`[compaction] provider` config.** Optional override that routes the
  summarisation pass through a different `[providers.<name>]` entry
  (e.g. cheap-and-fast for compaction, slow-and-careful for the main
  loop). Falls back to the session provider with a warn log when the
  named provider is missing.
- **Contextual ENSO.md / AGENTS.md injection on `read`.** When the
  model reads a file that lives below a directory with its own
  `ENSO.md` or `AGENTS.md` not covered by the static system prompt,
  that instruction file is appended to the read tool's result wrapped
  in a `<system-reminder>` block. Per-session dedup so the same
  instruction never lands twice; survives compaction.
- **`Synthetic` / `Ignored` flags on `llm.Message`.** `Synthetic` marks
  programmatically-injected messages (compaction summaries, env
  reminders, contextual instructions) — sent to the model but
  distinguishable. `Ignored` marks audit-only rows kept in history but
  filtered from the outgoing `ChatRequest`. Persisted via a new
  migration; threaded through all provider adapters'
  `FilterForRequest` pass.

#### Output truncation

- **Trailing-message prompt-cache markers (Anthropic + Bedrock).** The
  `prompt_caching = true` providers now mark the last 1–2 conversation
  messages with `cache_control: ephemeral` (Anthropic) or `CachePoint`
  (Bedrock) in addition to the previously-marked system block and
  final tool. Total markers respect the 4-marker hard cap. Boosts cache
  hit ratio on multi-turn workloads where the user sends short
  follow-ups.
- **`MaxBytes` and `MaxLineLength` output caps.** `DefaultOutputCaps`
  gains byte-ceiling (50 KB default) and per-line-character (2000 char
  default) thresholds applied alongside the existing line cap inside
  `capTruncate` (byte → line → per-line). Catches pathological
  single-line outputs the line cap can't see (a minified-JS dump, a
  binary blob accidentally cat'd). Per-tool overrides via
  `[context_prune.output_caps]`.
- **Spill recovery for truncated tool output.** When a tool result is
  capped, the full output is persisted to
  `$XDG_STATE_HOME/enso/truncated/<session>/<hash>.txt` and the
  model-visible string ends with a `[full output: <path> — use 'read'
  with offset/limit to recover sections, or 'grep' to filter]` footer.
  Lets the model self-recover when something it needs was in the
  elided middle. Spill dirs are TTL-swept (7 days) on `Agent.New`.

#### LSP

- **Live diagnostics on write/edit.** When an LSP server is configured
  for the file's language, the `write` and `edit` tools refresh the
  server's view of the buffer (`DidChange` + `DidSave`) after a
  successful save and append filtered diagnostics to the tool result.
  The model learns about compile errors instantly, without an extra
  `lsp_diagnostics` call or a project build. Bounded 500ms wait with a
  100ms dedup window (gopls and pyright commonly emit an interim empty
  publication before the real one). Errors-only by default; configurable
  via `NotifierOptions`.
- **`Client.DidChange` and `Client.DidSave`.** Per LSP spec, with
  monotonic per-URI versions. Servers that only react to `didChange`
  (rather than fsnotify) now see edits the agent makes.
- **Builtin LSP auto-activation.** `gopls`, `typescript-language-server`,
  `pyright-langserver`, and `rust-analyzer` are auto-activated when the
  binary is on `PATH` — no `[lsp.<name>]` declaration required.
  Override per-server by declaring `[lsp.<name>]` with the same name;
  disable a single builtin via `command = ""`; disable all with
  `lsp_builtins_disabled = true`.

#### Daemon / observer protocol

- **`session_id` on daemon event envelopes.** Every event the daemon
  socket emits now carries its originating session id, so multi-session
  observers can route on the envelope alone without keeping their own
  outer subscribe context.
- **Wire-form `PermissionRequest`.** `EventPermissionRequest` now
  serialises a sanitised payload (`tool_name`, `args`, `agent_id`,
  `agent_role`, `diff`) for daemon-socket observers. The
  unserialisable `Respond` channel and `Deadline` are excluded via
  `json:"-"`. In-process consumers reach permissions through the
  unchanged `pendingPerms` / `PermissionResponseReq` path. Daemon-socket
  observers can finally show "awaiting permission" state.
- **`on_event` hook (`[hooks] on_event = "..."`)** — generic per-event
  shell hook that receives the event as JSON on stdin
  (`{session_id, cwd, type, payload}`). Fires off the agent loop on a
  dedicated fanout goroutine, bounded by the 10s hook timeout. Filtered
  by `on_events = [...]` or by the curated `DefaultEventFilter` (which
  excludes per-token deltas so the hook isn't spawned tens of thousands
  of times per turn). Complementary to the daemon-socket subscription
  path: this is the low-friction option for status boards, audit
  pipelines, and watchourai-style visualisers that don't want to manage
  a long-lived process.
- **`[hooks.env]` table.** Extra environment variables merged onto
  every hook subprocess's environment, overriding matching keys from
  `os.Environ()`. Lets observers keep tokens out of shell rc files.
- **External observers section** in README + `docs/content/config/
  reference.md` documenting both integration shapes (hook + daemon
  socket) with a pointer to the watchourai adapter as a worked
  example.

#### TUI

- **Double-Esc chord to cancel the in-flight turn.** When the input
  line is empty and a turn is in progress, two Esc keystrokes within
  the standard chord window send `MsgCancel` to the worker — the
  agent stops cleanly and emits `EventCancelled`. A "press esc again
  to stop" hint renders between the two presses. Single Esc remains a
  no-op so a stray keystroke can't kill a long-running task.

### Changed

- **`hooks.New` signature.** Replaced the multi-positional
  `New(onFileEdit, onSessionEnd string)` with a single
  `New(hooks.Config)` so the hook surface can grow without rippling
  through every embedder. The two internal callers
  (`internal/daemon/server.go`, `internal/backend/worker/adapter.go`)
  have been updated. External embedders that constructed
  `*hooks.Hooks` directly will need a one-line migration.

### Fixed

- **GitHub code-scanning autofixes.** Two Copilot Autofix patches:
  workflow permissions explicitly declared, and a slice-allocation
  size computation that could theoretically overflow.

## [v2.7.0] - 2026-05-22

### Added
- **AWS Bedrock provider (`[providers.<name>] type = "bedrock"`).** A
  multi-vendor adapter: Claude, Amazon Nova, Llama, Mistral, Cohere,
  and AI21 are all reachable through one provider type — the `model`
  id picks which. Auth follows the standard AWS credential chain
  (env vars, `~/.aws/credentials`, EC2/ECS/EKS instance role);
  `aws_region` and `aws_profile` override it. Claude models can opt
  into extended thinking via `extended_thinking` /
  `extended_thinking_budget`; reasoning surfaces through the same
  channel the TUI already renders for OpenAI reasoning models.
  Inference-profile ARNs are accepted as the `model` value.
- **Google Cloud Vertex AI provider (`type = "vertex"`).** Gemini and
  other Vertex-hosted models via the multi-vendor `generateContent`
  API. Auth uses Application Default Credentials (`gcloud auth
  application-default login`, service-account JSON via
  `GOOGLE_APPLICATION_CREDENTIALS`, or workload identity); set
  `project_id` and `location` (region) per provider.
- **Anthropic-native opt-in paths for Claude
  (`type = "anthropic"` / `"anthropic-bedrock"` /
  `"anthropic-vertex"`).** Opt-in adapters that go through Anthropic's
  own SDK for users who want Claude-specific niceties (extended
  thinking, prompt caching, native tool-result blocks) without the
  vendor-translation layer. The multi-vendor `bedrock` and `vertex`
  types remain the default route; the Anthropic-native paths are
  strictly opt-in and never silently shadow the vendor-neutral ones.
- **Vendor-side prompt caching (`prompt_caching = true`).** When set
  on a Claude-capable provider (`anthropic`, `anthropic-bedrock`,
  `anthropic-vertex`, and `bedrock` for Claude models) the adapter
  emits Anthropic `cache_control` markers on the last system block
  and the last tool, so the system + tool-definition prefix becomes a
  stable cacheable block reused across turns. No-op on `openai` /
  local providers. Off by default.
- **Bedrock guardrails and Vertex safety controls.** Per-provider
  policy knobs surface the vendors' built-in content-safety layers:
  Bedrock's `guardrail_id` / `guardrail_version` and Vertex's
  `safety_settings` (category → threshold map). Both attach to every
  request from that provider; off when unset.
- **Multimodal input (images and documents).** The `read` tool now
  recognises image (`png`/`jpeg`/`gif`/`webp`) and PDF inputs and
  attaches them as native multimodal parts on providers that support
  it (Anthropic, Bedrock Claude/Nova, Vertex Gemini). Text-only
  providers continue to receive a plain-text fallback. Includes
  message-shape conversion in the LLM adapters and per-vendor
  encoding tests.

### Changed
- **Dependency bumps.** `go.opentelemetry.io/otel` 1.39.0 → 1.41.0;
  `google.golang.org/grpc` 1.66.2 → 1.79.3; `github.com/mark3labs/
  mcp-go` 0.53.0 → 0.54.0.

## [v2.6.0] - 2026-05-21

### Added
- **Multi-line input in the TUI.** The input is no longer single-line.
  `Shift-Enter` (in terminals that speak the Kitty keyboard protocol),
  `Alt-Enter`, or `Ctrl-J` insert a literal newline; plain `Enter` still
  submits the whole buffer. The render soft-wraps on `\n` and on width,
  and scrolls vertically within up to three rows so the cursor stays
  visible regardless of buffer length (#52).
- **Model-driven checkpoints (`checkpoint` tool).** A new built-in tool
  the model can call to declare a logical step boundary (typically right
  after committing a finished step). The agent loop honors the request
  before the next chat completion: it runs a forced compaction pass
  with the supplied `reason` flowing into both the `compacted` event
  payload and the summariser's anchor, so the next response is
  generated against a freshly compacted history. The threshold-based
  auto-compaction at 60% of the context window is unchanged; this is a
  signal the model can emit at any logical boundary without waiting for
  the context to fill.

### Changed
- **Paste preserves newlines.** Terminal bracketed paste (`Ctrl-Shift-V`
  / `Cmd-V` / middle-click PRIMARY) previously flattened every newline
  to a space — a holdover from the single-line input. Now `\n` is kept
  verbatim; only `\r\n` and bare `\r` are normalised to `\n` so the
  buffer's line endings are consistent regardless of the source
  platform. Multi-line snippets paste as multi-line; `Enter` submits
  the whole buffer.

### Fixed
- **Lima VMs no longer drift-recreate on every `make build`.** The
  persistent per-project Lima VM's mount config used to drift each time
  the host's enso binary was rebuilt — the content-addressed snapshot
  path (`$XDG_STATE_HOME/enso/exe/<hash>/`) was mounted directly, so a
  new hash changed the YAML and triggered a full drift-recreate (~10s
  cold reboot per rebuild). The VM now mounts the **stable snapshot
  root** read-only and execs the content-addressed path *within* it via
  the shell argv: the YAML is invariant across rebuilds, the new binary
  still reaches the guest as a fresh `<hash>` subdir, and the persistent
  VM is preserved through host rebuilds (#54).
- **Lima cold boots ~10s faster on Alpine.** Alpine's stock cloud image
  ships a ~10s GRUB serial-console countdown that the persistent VM
  paid on every cold boot. A best-effort provisioning step zeroes the
  bootloader timeout (GRUB, `/etc/default/grub`, and the extlinux
  variant some Alpine builds use), taking effect on the next boot. The
  script never fails an otherwise-good boot (every step guarded; exits
  0) and is idempotent across reboots. The GRUB regex also handles
  the indented `set timeout=` lines real `grub.cfg` files use (#54).

## [v2.5.1] - 2026-05-19

### Fixed
- **Terminal/shell hung after a Lima-backed session.** Closing the
  terminal or running `exit` after using enso on a Lima VM would block
  indefinitely. Root cause (proven via `/proc/<pid>/fd`): the
  persistent-VM `limactl hostagent` — a daemon that by design outlives
  enso — was launched as a plain child of enso and kept the user's
  controlling terminal (`/dev/tty`, fd 4) open, so the shell's pty was
  never released even though enso itself had exited. `runLimactl` (the
  `limactl start`/resume path) now starts limactl in its own session
  with no controlling terminal (`Setsid`) and `/dev/null` stdin, so
  neither limactl nor the hostagent it daemonizes can inherit the
  shell's terminal. Live bring-up progress is unaffected (stdout/stderr
  remain enso-owned pipes). The persistent VM and its `ssh.sock` mux
  still outlive the session by design and are reclaimed by `enso prune`.
- **enso did not exit on a normal TUI quit with an isolated backend.**
  TUI shutdown sent the worker a shutdown message and then waited,
  unbounded, for the in-guest worker to EOF the Channel. With the Lima
  backend that Channel is an SSH session multiplexed over Lima's
  persistent ControlMaster mux, so if the worker did not wind down
  promptly the wait never returned and enso never exited (the launching
  shell never got its prompt back). The graceful wind-down is now
  bounded: on timeout the idempotent worker teardown — which
  force-closes the Channel and reaps the `limactl`+ssh process group —
  runs immediately, guaranteeing the seam loop returns and enso exits.
  The non-interactive `enso run` path is intentionally unchanged (there
  a worker finishing *is* normal completion).
- **`limactl shell` worker could wedge `cmd.Wait()` during teardown.**
  The Lima worker passed an `io.Writer` as the command's stderr, so
  `os/exec` spawned a copy goroutine that `cmd.Wait()` joins on. Lima's
  `ControlPersist` ssh master (in its own session, intentionally not in
  enso's reaped process group) inherits and holds that stderr pipe's
  write end open for the life of the VM, so the goroutine never EOF'd
  and `cmd.Wait()` could block forever. The worker now owns the stderr
  pipe as an `*os.File` (no `os/exec` join goroutine) and copies it
  itself, closing the read end in teardown to end its own copier.
- **Qwen3 tool calls were silently dropped on llama.cpp.** Qwen3
  thinking-style templates wrap the tool call *inside* the reasoning
  channel, so it reached neither the content stream nor the structured
  `tool_calls` channel and the model appeared to do nothing. The agent
  now falls back to parsing inline tool calls out of the reasoning text
  as a last resort (the cleaned reasoning is still never persisted to
  history).

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

[v2.11.0]: https://github.com/TaraTheStar/enso/compare/v2.10.1...v2.11.0
[v2.10.1]: https://github.com/TaraTheStar/enso/compare/v2.10.0...v2.10.1
[v2.10.0]: https://github.com/TaraTheStar/enso/compare/v2.9.0...v2.10.0
[v2.9.0]: https://github.com/TaraTheStar/enso/compare/v2.8.0...v2.9.0
[v2.8.0]: https://github.com/TaraTheStar/enso/compare/v2.7.0...v2.8.0
[v2.7.0]: https://github.com/TaraTheStar/enso/compare/v2.6.0...v2.7.0
[v2.6.0]: https://github.com/TaraTheStar/enso/compare/v2.5.1...v2.6.0
[v2.5.1]: https://github.com/TaraTheStar/enso/compare/v2.5.0...v2.5.1
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
