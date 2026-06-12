# Changelog

All notable changes to ensō are documented here. The format is based
on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v2.13.0] - 2026-06-11

### Added

- **Decoupled compaction budget (`compaction_budget`).** A per-provider
  token target that fires proactive compaction independently of
  `context_window`, so you can set a smaller "fast zone" for
  quick-decoding hardware while the gauge keeps the real window as its
  honest denominator (safety-clamped so it can never disable compaction).
- **Live compaction progress bar.** While history is summarising the TUI
  now shows an animated braille progress bar instead of a generic
  spinner, and the status-line context gauge marks the compaction budget,
  turning amber once it's exceeded; progress crosses the worker/host
  boundary via a new `EventCompacting`.
- **Opt-in reasoning budget (`reasoning_budget`).** Caps the
  chain-of-thought runes a model may stream before it must act (answer or
  tool call); exceeding it aborts and auto-recovers with a nudge to
  commit — a hard backstop for reasoning models that deliberate for
  minutes without deciding (OpenAI-compatible path only; `0` disables).
- **Sampler anti-loop knobs (`sampler.frequency_penalty`,
  `sampler.repetition_penalty`).** OpenAI-style and llama.cpp
  `repeat_penalty` levers that discourage re-emitting recent tokens,
  suppressing a deliberation spiral at sampling time before any guard has
  to abort (a zero value is omitted on the wire so the server default
  applies).
- **`/new` slash command** mints a fresh session in place — it re-execs
  the same binary with the session-selecting flags stripped, preserving
  the rest (`--ephemeral`, `--agent`, …).
- **Switch sessions live from the recent-sessions overlay** — selecting a
  session now re-execs into it rather than just printing the resume
  command; the overlay also tags each row with its execution backend
  (`[podman]`/`[lima]`) and an `[interrupted]` marker.
- **Unlimited-turns flag (`-1`)** for non-interactive runs.

### Changed

- **Workflows now run inside the backend seam.** Workflow execution is
  inverted to run worker-side (shipped over the Channel via the control
  protocol), so workflows work under isolated backends (podman/lima) —
  with real end-to-end tests on both — and sessions now record their
  execution backend as provenance (migration `0009_sessions_backend`).
- **`loop_guard` now also scores semantic novelty.** Beyond catching a
  model that repeats the same short unit verbatim, it measures shingle
  diversity over a rolling reasoning window and trips when a model
  re-treads the same plan in fresh wording ("let me reconsider… actually,
  reviewing again…") — the semantically-looping shape that grinds a
  reasoning model toward `max_tokens`.
- **Egress allowlist distinguishes operator config from interactive
  grants** — config-file allow entries are seeded as `AllowConfigured`
  and opt out of the interactive broker, while runtime grants stay
  subject to it.
- **Faster live rendering of long streams** — the in-flight block's
  wrapped and styled text is now memoized, so a long model stream
  re-renders in O(n) per frame instead of O(n²) (a 24K-token local
  stream is ~96 KB re-wrapped every tick otherwise).
- **`interface{}` modernized to `any`** across the codebase.
- Dependency bumps: `github.com/anthropics/anthropic-sdk-go`,
  `modernc.org/sqlite` 1.52.0, `charm.land/bubbletea/v2` 2.0.7, and the
  AWS SDK (`config`, `service/bedrockruntime`).

## [v2.12.0] - 2026-06-07

### Added

- **Tool-output context compression.** Bash and read output is compacted
  before it reaches the model — declarative per-command filters (Go, Rust,
  Node, Python, grep, VCS, Docker, and Kubernetes ship built-in; drop a
  `*.toml` under `$XDG_CONFIG_HOME/enso/filters/` to add more) plus
  structural compression of diffs, logs, and JSON — with the raw output
  always spilled to disk so nothing is lost. On by default via
  `[context_prune] compress`; `/context` reports the tokens saved.
- **`/discover`** mines the session store for the bash commands that
  produced the most output and flags which are already covered by a filter
  and which aren't, so it's obvious where a new one would pay off.
- **`read` outline mode** (`mode: "outline"`) returns a signature-only view
  of a file — Go via AST, other languages via a definition-line heuristic —
  so the model can survey a large file without pulling in every body.
- **Structured workflow role outputs.** A role can expose named fields
  (parsed from a trailing ` ```json ` block or `KEY: value` lines) read
  downstream as `{{ .<role>.<field> }}`; the raw text stays available as
  `{{ .<role>.output }}`, so existing workflows are unaffected.
- **Conditional workflow edges.** An edge may carry an `if` guard
  (`from -> to if '<predicate>'`); a node runs only if all its incoming
  edges fire, and skips propagate down the graph — unlocking
  ship-vs-escalate routing (see `examples/workflows/gated-ship.md`).

### Changed

- **Compaction cache-boundary invariant is now regression-tested** — a
  guard asserts the cache-hot system-prompt prefix survives compaction
  byte-for-byte and the summary is appended after it, never spliced inside.
- Dependency bumps: `google.golang.org/genai`, `modernc.org/sqlite`,
  `github.com/anthropics/anthropic-sdk-go`, and the AWS SDK.

### Fixed

- **Tool-registry data race under parallel workflows** — the cached
  tool-definition list is now guarded by a `sync.RWMutex` so concurrent
  sibling roles no longer race on it (caught by `go test -race`).

## [v2.11.0] - 2026-06-02

### Added

- **`/rewind` — undo to an earlier turn.** An overlay rolls the session
  back to any earlier turn and lets you restore code + conversation,
  conversation only, or code only; the prior thread is preserved as a
  forked session and the rewound-away message is pre-filled for re-sending.
  Unavailable under `--ephemeral`.
- **Per-turn workspace checkpoints.** enso snapshots the project tree before
  each genuine user turn (via `cp -a --reflink=auto`) so `/rewind` can
  restore exact prior state; tunable under `[checkpoints]` (`disabled`,
  `retain` default 20/session) and swept by `enso prune`.
- **Multimodal image input.** Attach an image with an `@path` mention
  (PNG/JPEG/GIF/WebP up to 10 MiB); images are resolved host-side and
  crossed to isolated workers as bytes, and `read` now shows images directly
  to a vision-capable model.
- **Slash-command palette.** Typing `/` on an empty line opens a filtered,
  navigable list of every registered command and loaded skill with one-line
  descriptions.
- **Live status-line badges** always show context usage (`ctx N%`,
  brightening past 80%) and session spend (`$0.0123`) or tokens (`Σ 15k`),
  so `/context` and `/cost` aren't needed for an at-a-glance read.
- **"esc to interrupt" hint** on the status line while a turn is in flight
  (a two-stage chord), suppressed during a pending permission/egress prompt
  where `Esc` means deny.
- **Scrollable overlays** — the file picker, slash palette, and
  recent-sessions list now scroll with the selection on short terminals
  instead of overflowing.
- **Large-file streaming in `read`** — files over 10 MiB stream a bounded
  window (10 MiB / 50,000-line caps) instead of being slurped whole,
  preventing OOM on multi-gigabyte logs.

### Changed

- **Resume and `/sessions` are now scoped to the current directory** by
  default (`--all` for every directory); `--continue` resumes the most
  recent session in the cwd.
- **Resume and `/transcript` replay tool calls and reasoning** into
  scrollback, not just user/assistant prose; the reasoning is for replay
  only and never sent back to the model.
- **Quitting now requires confirmation** — an idle `Ctrl+C`, or `Ctrl+D` on
  an empty line, needs a confirming second press within 3 seconds.
- **"Remember" rules for `read`/`grep`/`glob` are now project-scoped** — a
  path inside the cwd yields `<tool>(<cwd>/**)` and a path outside yields an
  exact-path rule, so remembering a read no longer grants whole-filesystem
  access.
- **"Always"/"Turn" grants now reach the worker's enforcing checker** over
  RPC before the decision is sent, so the next call is actually gated by the
  new rule.
- **Security config arrays are unioned across config tiers, not replaced** —
  `permissions.allow`/`ask`/`deny` and `web_fetch.allow_hosts` merge
  (deduped, grow-only), so a higher layer can't wipe a more-trusted tier's
  deny list. Deny still wins.
- **`config.local.toml` is now trust-gated** like `config.toml`, since a
  hostile repo can commit one at higher priority.
- **Bash deny rules are much harder to evade** — matching now tests each
  command segment raw and normalized (collapsed whitespace, path-to-basename,
  removed shell-escapes, unquoted words, re-split command substitution), so
  `bash(rm -rf *)` also catches `/bin/\rm -rf /` and `$(rm -rf /)`.
- **Allowlist path matching cleans `..` traversal**, so `read(/repo/**)` no
  longer matches `/repo/../etc/passwd`.
- **`read` rejects directories up front** and refuses oversized images with
  `[image too large to inline]` rather than emitting binary.

### Fixed

- **`bash` no longer exposes credentials to the model** — the child
  environment is scrubbed of every `ENSO_*` var and any name containing
  `API_KEY`/`SECRET`/`TOKEN`/`PASSWORD`/`CREDENTIAL`/`PRIVATE_KEY`/
  `ACCESS_KEY`, so the model can't `echo $OPENAI_API_KEY`.
- **SSRF protection on the host egress proxy** — it resolves each target
  once, refuses denied IP classes (loopback, RFC1918/ULA, link-local incl.
  the `169.254.169.254` metadata address, CGNAT, multicast), then dials the
  pinned IP (DNS-rebind defense); shared with `web_fetch` via the new
  `internal/netsec` package.
- **Tool-call order is now deterministic for prefix-cache stability** —
  completed calls return in streamed-delta-index order rather than Go's
  randomized map order, keeping the llama.cpp prefix cache intact across
  turns.
- **Vertex/Gemini parallel tool calls no longer collide** — IDs are now
  synthesised as `call_<name>_<idx>` since Gemini omits them.
- **Session message-sequence races fixed** — the shared `seq` counter is now
  mutex-guarded so concurrent writers no longer collide on the
  `(session_id, seq)` key and silently drop a message.
- **Migration framework hardened** — version is the filename's numeric
  prefix (not sorted position), duplicates are rejected, and each migration
  runs with its `user_version` bump in one transaction.
- **`edit` rejects an empty `old_string`** instead of splicing `new_string`
  between every byte under `replace_all`.
- **Malformed permission patterns no longer panic** — `bash(rm` now errors
  with "missing closing ')'".
- **The `@` file picker no longer drops typed text** when cancelled.

## [v2.10.1] - 2026-05-30

### Added

- **Up / Down arrow navigation in the input** moves the cursor a visual row
  at a time through soft-wrapped and multi-line input, keeping the display
  column.
- **Foreground-polling nudge for `bash`** — a command that only waits (a
  leading `sleep` ≥10s, or a sleep-in-loop poll) returns immediately with a
  nudge to background it and poll with `bash_output` instead of blocking the
  turn.

### Fixed

- **Web tools now honour the egress proxy in sealed backends** —
  `web_search` and `web_fetch` previously dialed directly and were rejected
  by the guest firewall (an iptables EPERM on udp :53 that looked like a DNS
  failure); both now route through the injected proxy when set (in-guest
  SSRF IP-pinning is retained for the `local` backend only).
- **TUI input no longer panics when the cursor sits on a newline** — the
  cursor is now attributed to the line the newline terminates instead of
  slicing a backwards range and tearing down the program.

## [v2.10.0] - 2026-05-29

### Added

- **Agentic generation guards + auto-recovery.** Three hardware-independent
  guards keep a local model from wedging a turn — a max-tokens cap, a
  mid-stream loop guard, and a stall-on-silence watchdog — and on a trip
  auto-recovery retries the turn with a nudge; all tunable under
  `[providers.<name>.generation]`.
- **Explicit tool-call timeouts.** A foreground `bash` command runs under a
  wall-clock budget (`[bash] command_timeout`, default 120s) after which its
  process group is killed and partial output returned; MCP calls get the
  same via `[mcp.<name>] call_timeout`.
- **Background bash mode.** `bash` accepts `run_in_background: true` to start
  a command detached and return a job id; new tools `bash_output` and
  `bash_kill` read and terminate it.
- **Hang-prevention for foreground commands** — a pre-run check catches the
  common offenders (`tail -f`, `watch`, `journalctl -f`, dev servers) and
  nudges toward `run_in_background` instead of waiting out the timeout.

### Fixed

- **Context-window overflow: compaction now reserves the output budget** —
  the trigger is now `context_window − max_tokens − margin` rather than a
  flat `0.75×`, so a near-threshold prompt plus its reply can't overflow the
  real ceiling.
- **Context-window overflow now self-corrects** — when a request still
  exceeds the window, enso parses the real limit from the server's 400,
  adopts it, force-compacts, and retries (covers litellm, OpenAI, vLLM, and
  llama.cpp phrasings).
- **Keep-alive race no longer surfaces as a dead turn** — a bare `EOF` from
  the provider call is now classified as a closed connection and re-issued
  on a fresh one (safe, since no response had streamed).

## [v2.9.0] - 2026-05-28

### Added

- **`enso config init --project`** scaffolds `<cwd>/.enso/config.toml` with
  `[backend.podman]`/`[backend.lima]`/`[backend.egress]` blocks tuned to the
  detected language (`--lang`, `--backend` flags; interactive under a TTY,
  deterministic under pipes).
- **Pinned status-line hints for unresolved prompts** — while a permission
  or egress prompt awaits an answer, the status line shows
  `▸ awaiting: <tool>(…)   [y/n/a/t]` with the auto-deny countdown.
- **Enter = "yes" default** on permission and egress prompts, rendered with
  a `▸ ` cursor; the other choices dimmed.
- **Ctrl-C cancels in-flight turns**, with a second press within ~500 ms to
  force-quit if the cancel itself wedged; idle Ctrl-C still quits
  immediately.

### Changed

- **`Registry.ToolDefs()` returns tools sorted by name** (memoized), keeping
  the serialized tools array byte-stable across turns so it doesn't bust the
  prompt-prefix cache.
- **Permission/egress `[n]o` is no longer rendered in red** — deny is the
  safe outcome, not the destructive one.

### Fixed

- **Inference cancellation race** — the per-corr canceller is now installed
  synchronously in the demux loop before the inference goroutine launches,
  so a cancel arriving early is no longer dropped.
- **Stale TUI prompt pin after a cancelled turn** — `m.perm`/`m.egress` are
  now cleared on cancel/error, with a best-effort Deny sent.
- **Project config file modes** tightened to `0700` dir / `0600` file to
  match the user-config path.
- **`os.Stat` error handling on the config-init pre-existence check** — a
  non-`ErrNotExist` error now propagates instead of being treated as "file
  doesn't exist".
- **Silent flag coercions in `enso config init --project`** — an invalid
  `--backend`, or `--project` combined with `--wizard`, now errors instead
  of being silently dropped.
- **Spurious `MsgInferenceCancel` on the wire** — the cancel-watch goroutine
  now double-checks `entry.done` before sending.

## [v2.8.0] - 2026-05-23

### Added

#### Context management

- **Real per-message token accounting** — provider-reported token counts are
  parsed from every adapter and stored in a new `message_usage` table, so
  compaction, the status line, and resume hydration use real numbers instead
  of a char heuristic.
- **Structured Markdown compaction summaries** — the summariser now fills a
  fixed seven-section structure so long sessions stay legible across
  compactions.
- **Update-mode compaction** — later compactions update the prior summary
  rather than re-summarising from scratch, stopping "summary of a summary"
  degradation.
- **Token-budgeted recent-turn tail** — the pinned tail is sized by
  `min(8000, max(2000, ctxWindow*0.25))` tokens rather than a fixed 6 turns.
- **`[compaction] provider`** routes the summarisation pass through a
  different provider (e.g. cheap-and-fast), falling back to the session
  provider with a warn log.
- **Contextual ENSO.md / AGENTS.md injection on `read`** — reading a file
  below a directory with its own instruction file appends that file (once
  per session) to the read result.
- **`Synthetic` / `Ignored` flags on `llm.Message`** distinguish injected
  messages and audit-only rows filtered from the outgoing request.

#### Output truncation

- **Trailing-message prompt-cache markers (Anthropic + Bedrock)** mark the
  last 1–2 conversation messages as cacheable (respecting the 4-marker cap)
  to boost hit ratio on multi-turn workloads.
- **`MaxBytes` and `MaxLineLength` output caps** (50 KB / 2000 chars
  default) catch pathological single-line outputs the line cap can't see;
  per-tool overrides via `[context_prune.output_caps]`.
- **Spill recovery for truncated tool output** — the full output is
  persisted under `$XDG_STATE_HOME/enso/truncated/` and the model is told
  the path so it can `read`/`grep` to recover the elided middle.

#### LSP

- **Live diagnostics on write/edit** — `write` and `edit` refresh the LSP
  server's buffer view and append filtered diagnostics, so the model learns
  about compile errors instantly without a separate call or build.
- **`Client.DidChange` and `Client.DidSave`** with monotonic per-URI
  versions, so servers that only react to `didChange` see the agent's edits.
- **Builtin LSP auto-activation** for `gopls`, `typescript-language-server`,
  `pyright-langserver`, and `rust-analyzer` when on `PATH` — no
  `[lsp.<name>]` declaration required.

#### Daemon / observer protocol

- **`session_id` on daemon event envelopes** so multi-session observers can
  route on the envelope alone.
- **Wire-form `PermissionRequest`** — `EventPermissionRequest` now serialises
  a sanitised payload so daemon-socket observers can show "awaiting
  permission" state.
- **`on_event` hook (`[hooks] on_event`)** — a generic per-event shell hook
  receiving the event as JSON on stdin, filtered to exclude per-token deltas.
- **`[hooks.env]` table** merges extra environment variables onto every hook
  subprocess, keeping tokens out of shell rc files.
- **External observers documentation** in the README and config reference
  covering both the hook and daemon-socket shapes.

#### TUI

- **Double-Esc chord to cancel the in-flight turn** when the input line is
  empty; single Esc stays a no-op so a stray keystroke can't kill a task.

### Changed

- **`hooks.New` signature** is now `New(hooks.Config)` instead of multiple
  positional strings, so the hook surface can grow without rippling through
  embedders (external embedders need a one-line migration).

### Fixed

- **GitHub code-scanning autofixes** — explicit workflow permissions and a
  slice-allocation overflow guard.

## [v2.7.0] - 2026-05-22

### Added

- **AWS Bedrock provider (`type = "bedrock"`)** — one multi-vendor adapter
  reaching Claude, Amazon Nova, Llama, Mistral, Cohere, and AI21 (the
  `model` id picks which); auth follows the standard AWS credential chain.
- **Google Cloud Vertex AI provider (`type = "vertex"`)** — Gemini and other
  Vertex models via the multi-vendor `generateContent` API, authed with
  Application Default Credentials; set `project_id` and `location`.
- **Anthropic-native opt-in paths (`anthropic` / `anthropic-bedrock` /
  `anthropic-vertex`)** for Claude-specific niceties (extended thinking,
  prompt caching, native tool-result blocks); strictly opt-in, never
  shadowing the vendor-neutral routes.
- **Vendor-side prompt caching (`prompt_caching = true`)** emits Anthropic
  `cache_control` markers on the system + tool-definition prefix for
  Claude-capable providers; no-op elsewhere, off by default.
- **Bedrock guardrails and Vertex safety controls** — per-provider policy
  knobs (`guardrail_id`/`guardrail_version`, `safety_settings`) attach to
  every request; off when unset.
- **Multimodal input (images and documents)** — `read` recognises image and
  PDF inputs and attaches them as native multimodal parts on providers that
  support it, with a plain-text fallback elsewhere.

### Changed

- Dependency bumps: `go.opentelemetry.io/otel`, `google.golang.org/grpc`,
  `github.com/mark3labs/mcp-go`.

## [v2.6.0] - 2026-05-21

### Added

- **Multi-line input in the TUI** — `Shift-Enter`, `Alt-Enter`, or `Ctrl-J`
  insert a newline while plain `Enter` submits; the render soft-wraps and
  scrolls within up to three rows (#52).
- **Model-driven checkpoints (`checkpoint` tool)** — a built-in tool the
  model can call at a logical step boundary to force a compaction pass with
  its `reason`, independent of the threshold-based auto-compaction.

### Changed

- **Paste preserves newlines** — bracketed paste now keeps `\n` verbatim
  (normalising only `\r\n`/`\r`) instead of flattening to spaces.

### Fixed

- **Lima VMs no longer drift-recreate on every `make build`** — the VM now
  mounts the stable snapshot root read-only and execs the content-addressed
  binary within it, so the instance YAML is invariant across rebuilds (#54).
- **Lima cold boots ~10s faster on Alpine** — a best-effort provisioning
  step zeroes the Alpine cloud image's GRUB serial-console countdown
  (idempotent, never fails an otherwise-good boot) (#54).

## [v2.5.1] - 2026-05-19

### Fixed

- **Terminal/shell hung after a Lima-backed session** — the persistent-VM
  `limactl hostagent` (which outlives enso by design) held the user's
  controlling terminal open; `runLimactl` now starts limactl with `Setsid`
  and `/dev/null` stdin so neither it nor the hostagent inherits the shell's
  tty.
- **enso did not exit on a normal TUI quit with an isolated backend** — the
  graceful worker wind-down is now bounded, falling back to the idempotent
  teardown on timeout so the seam loop always returns and enso exits.
- **`limactl shell` worker could wedge `cmd.Wait()` during teardown** — the
  worker now owns the stderr pipe as an `*os.File` and copies it itself,
  instead of relying on an `os/exec` join goroutine that the persistent ssh
  master kept open forever.
- **Qwen3 tool calls were silently dropped on llama.cpp** — Qwen3 thinking
  templates wrap the call inside the reasoning channel, so the agent now
  falls back to parsing inline tool calls out of reasoning text (the cleaned
  reasoning is still never persisted).

## [v2.5.0] - 2026-05-18

### Changed (BREAKING — config migration required)

- **Unified backend configuration.** Every backend's environment now lives
  under its own `[backend.<name>]` sub-table selected by the single
  `[backend] type` switch, with a shared vocabulary (`init`, `extra_mounts`)
  across podman and lima. There is **no backward compatibility** — old keys
  are silently ignored, so a stale config runs with defaults until migrated.
- **Config scoping is now enforced.** `[backend] type`/`workspace` may be set
  in any layer, but the per-backend environment sub-tables
  (`[backend.podman|lima|egress]`) are honored **only** from project-scoped
  config and are stripped with a one-time warning in user/system config.

  Migration: keep only `[backend] type`/`workspace` in user config; in each
  repo, split `[bash.sandbox_options]` into `[backend.podman]` /
  `[backend.egress]`, move `[lima]` to `[backend.lima]`, and set
  `[backend] workspace` (was `[bash.sandbox_options] workspace`).

  ```toml
  # user config (~/.config/enso/config.toml)
  [backend]
  type      = "lima"
  workspace = "overlay"

  # repo .enso/config.toml
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

- **Backend bring-up is no longer silent** — a cold lima run streams Lima's
  native progress plus concise enso phase lines to stderr (stdout stays
  clean for `--format json`).
- **Lima provisioning (`[backend.lima] init`)** — shell lines that run once
  during VM provisioning, the podman `init` analogue for installing
  toolchains the base template lacks.
- **Colored post-session overlay diff** — the end-of-task commit/discard/keep
  prompt renders the agent's diff with TUI-style git coloring.
- **Podman `[backend.podman] init` is now honored** — each per-task
  `podman run` wraps the entrypoint to run init in-container (output to
  stderr, `set -e`) then exec the worker; since containers are per-task
  `--rm`, keep init cheap.
- **Lima VM auto-recreate on config drift** — a persistent VM is rebuilt
  automatically when its generated config no longer matches the effective
  config.

### Removed

- **Legacy per-project sandbox** — the `internal/sandbox` package and
  `enso sandbox list|stop|rm|prune` are gone (podman is now strictly
  per-task `--rm`); **`enso prune` replaces `enso sandbox prune`** and also
  reclaims lima VMs and workspace review copies. The dead in-process
  `SandboxRunner` bash path was removed too.

### Security

- **Lima no longer mounts host `$HOME` into the guest.** enso now inherits an
  image-only base (`template:_images/<distro>`, default Alpine) and mounts
  only the writable project copy and the read-only enso binary, so the agent
  can no longer read `~/.ssh`, `~/.aws`, or enso's own config.
  `[backend.lima] template` now selects the guest image distro; the default
  Alpine image auto-installs `iptables` for the egress seal; pre-existing
  `$HOME`-mounting VMs are auto-recreated on next run.

### Fixed

- **TUI text overflowed the right edge of the terminal** — live streaming
  blocks now hard-wrap to terminal width and the input is a width-bounded
  scrolling window (ANSI/CJK-aware); regression tests assert no rendered
  line exceeds the width.
- **Lima overlay write-back / empty-project / missing commit prompt** — the
  overlay's `merged` directory now keeps a stable inode for the life of the
  persistent VM (refreshed in place, never rename/rmdir'd), since a
  persistent Lima VM 9p-exports it by its boot-time inode. Covered by a
  real-VM regression test.
- **`fatal error: invalid runtime symbol table` mid-session (lima/podman)** —
  enso now execs an immutable content-addressed snapshot of the binary under
  `$XDG_STATE_HOME/enso/exe/<hash>/` instead of the live binary, so
  rebuilding enso while a guest worker runs no longer corrupts its mmap'd
  pclntab.
- **Paste into the TUI did nothing** — added a `tea.PasteMsg` case
  (bubbletea v2 bracketed paste) inserting at the cursor, gated like typed
  text; pasted newlines flatten to spaces in the single-line input.
- **Sessions not saved under podman/lima** — the in-guest worker wrote
  messages to a throwaway guest DB; isolated workers now stream each append
  over the Channel (`MsgPersistMessage`/`MsgPersistToolCall`) to the host DB
  via `host.WithWriter`, with resume history shipped to the worker. Local
  backend unchanged.
- **lima worker not reaped on terminal close (SIGHUP)** — both the TUI and
  `enso run` now catch SIGHUP and run the idempotent teardown, since its
  default action killed enso before the `defer` could reap the `limactl`+ssh
  process group.

## [v2.4.0] - 2026-05-17

### Added

- **Unification of egress broker & Lima isolation** — the egress broker now
  behaves consistently across all sealed backends (podman and lima).
- **Interactive egress prompting** — in attended sessions a sandboxed command
  reaching a non-allowlisted target prompts `[y]es once`, `[t]his task`
  (memoized for the session), or `[n]o`.

### Changed

- **Lima backend network sealing** — lima isolation is upgraded from
  inference-only to genuine network sealing, firewalling guest egress by
  default through a host-side proxy.
- **Egress proxy architecture** — a unified `EgressProxy`/`EgressBroker`
  provides a central, observable, policy-enforced gateway for outbound
  traffic from sealed sandboxes.
- **Enhanced documentation** for the new egress, static-allowlist,
  interactive-prompting, and `--yolo` modes.

### Fixed

- **Lima backend shell injection** — the worker launch now injects
  `HTTPS_PROXY`/`HTTP_PROXY` via `env` so it can reach the host egress proxy.

## [v2.3.0] - 2026-05-17

### Added

- **`[backend] type = "lima"` — real-VM isolation.** The whole agent runs
  inside a persistent-per-project Lima VM (macOS vz/qemu, Linux qemu+KVM,
  Windows wsl2) with host-proxied inference and the project mounted at its
  real path; tunables under `[lima]`, and it refuses to start if `limactl`
  is absent (no silent downgrade).
- **`[backend] runtime`** selects the container CLI for `type = "podman"`:
  `"auto"` (default), `"podman"`, or `"docker"`.

### Changed

- **Workspace overlay is now a true three-way merge** — task end compares a
  pristine baseline against both the agent's copy and the live project,
  applying non-conflicting changes per file (no more blanket
  `rsync --delete`); conflicts are kept for manual merge and require typed
  `overwrite` to force.

### Removed

- **BREAKING: `[bash] sandbox` is removed.** `[backend] type` is now the sole
  backend selector (`"local"` default, `"podman"`, `"lima"`); migrate by
  deleting `[bash] sandbox` and setting `[backend] type` (until you do, you
  run with no isolation). `[bash.sandbox_options]` is unchanged.

## [v2.2.0] - 2026-05-16

### Added

- **Provider pools (`[pools.<name>]`)** — providers behind the same endpoint
  share one concurrency semaphore by default (one llama-swap = one pool),
  fixing shared-hardware thrash; override with per-provider `pool =` and tune
  `concurrency`/`queue_timeout`. Pools coordinate across every session hosted
  by one `enso daemon`. (`rpm`/`tpm`/`daily_budget` are parsed but not yet
  enforced.)
- **Auto-rendered "## Available models" prompt section** — when two or more
  providers are configured, enso names the running model and lists the others
  with an optional `description`, pool membership, and swap-cost so the model
  can route work via `spawn_agent`'s `model:` arg. Opt out with
  `[instructions] include_providers = false`.

### Changed

- **enso now follows the XDG Base Directory layout instead of `~/.enso`** —
  user-editable files under `$XDG_CONFIG_HOME/enso`, app data under
  `$XDG_DATA_HOME/enso`, logs/`trust.json` under `$XDG_STATE_HOME/enso`, the
  daemon socket under `$XDG_RUNTIME_DIR/enso`; existing `~/.enso` installs
  must be moved by hand.
- **Prompt layering is now append-by-default at every level** —
  `~/.config/enso/ENSO.md` now appends to the embedded default (matching
  project-level behavior); add `--- replace: true ---` frontmatter to restore
  replace semantics.

## [v2.1.0] - 2026-05-09

### Changed

- **TUI upgraded to Bubble Tea v2** with refreshed input handling and
  improved markdown rendering (semantic block colouring, diff-aware
  tool-result colouring).
- **Session-inspector overlay now binds to `Ctrl-Space`** (or `Ctrl-@`);
  `Ctrl-A` is the readline "move to start of line" again.
- **Status line gained a streaming-only `.TokensPerSec`** template variable,
  hidden when idle.

## [v2.0.0] - 2026-05-09

The TUI was migrated off `rivo/tview` onto `charmbracelet/bubbletea` +
`lipgloss`: completed messages now live in your real terminal scrollback (so
`tmux` highlight + middle-click copy work), with a small live region for
streaming output, the status line, and the input.

### Added

- **Scrollback-native chat rendering** via Bubble Tea / Lipgloss
  (`internal/ui/bubble`); the `internal/ui` surface stays framework-agnostic.
- **Live tool-call status indicators** with elapsed-time badges and a live
  spinner.
- **Semantic chat lanes** — yellow for user input, teal for tool calls, gray
  for reasoning, red `✘` for errors.

### Changed

- **Default `enso` (and `enso tui`) now run the Bubble Tea backend**; the old
  tview backend is removed.

## [v1.2.0] - 2026-05-09

### Added

- **`enso config init --wizard`** interactive first-run onboarding (provider
  preset, model, optional API key).
- **Slash commands** `/export`, `/stats`, `/fork`, `/find`, `/rename`,
  `/info`, `/lsp`, `/mcp`, `/git`, `/cost`, `/transcript` for inline access
  to CLI features.
- **`/find` overlay** (and `Ctrl-F`) for searching the current session's
  transcript (substring or `-e` regex).
- **Auto-derived and manual session labels**, settable via `/rename <label>`.
- **Live elapsed-time badges** on in-flight tool calls.
- **Cumulative token / cost tracking** in the status bar and `/cost`.
- **MCP server health tracking** with sidebar error reporting.
- **LLM connection state tracking** with a background recovery probe
  reflected in the status bar.
- **Daemon-side permission timeouts** with TUI countdown (auto-deny after the
  window if no client is attached).
- **Turn-scoped permission grants** — a `t` decision ("allow for the rest of
  this turn") alongside allow/remember/deny.
- **Untrusted-content marking** in the TUI for tool results from external
  systems (LSP, web_fetch) to flag prompt-injection vectors.
- **Classified error reporting** with retry countdowns (transport vs.
  protocol vs. cancellation).
- **Workflow validation at parse time** (cycles, dangling edges, duplicate
  role names).
- **Hook observability** — `on_file_edit`/`on_session_end` failures surface
  as inline TUI notices.

### Changed

- **Bash deny rules are now segment-aware** — `bash(rm -rf *)` catches
  chained variants like `do_evil; rm -rf /` and `cd / && rm -rf *`.
- **System prompt injects sandbox state and file-confinement details** so the
  model knows which paths are reachable.
- **Crash-recovery overhaul** — tool-call backfill is inline and shutdown
  ordering is deterministic, so `kill -9` mid-tool-call resumes cleanly.
- **Input-discard handling** — cancelling a turn flushes queued user messages
  as `(discarded N queued messages)` instead of replaying them out of order.
- **`Ctrl-C` is now gated on activity state** — it cancels during a tool call
  and is a no-op when idle.
- **The tview built-in undo/redo path is no longer intercepted** by enso's
  key handler.

### Fixed

- **Bash deny-rule bypasses** where prepending an allowed segment
  (`do_evil; git status`) evaded a rule.
- **Hook timeouts no longer leak goroutines** on slow user scripts.

### Security

- **Segment-aware bash deny rules** close the most-common bypass class; the
  rest (command substitution, eval, here-docs) are documented out-of-scope,
  with sandbox mode the recommended boundary for hostile-input sessions.

## [v1.1.1] - 2026-05-07

### Fixed

- **`Ctrl-C` handling moved to the application level** so it no longer exits
  ensō when the focused widget ignored the keypress.

## [v1.1.0] - 2026-05-07

### Added

- **`web_search` tool** with two backends — DuckDuckGo's HTML endpoint by
  default and SearXNG when `[search.searxng] endpoint` is set; selectable via
  `[search] provider`.
- **GitHub Actions release workflow** building reproducible cross-platform
  binaries (Linux/macOS/Windows × amd64/arm64) on tag push.

## [v1.0.0] - 2026-05-07

First public release.

### Added

- **TUI agent (`enso tui`) and one-shot mode (`enso run`)** for any
  OpenAI-compatible chat endpoint; default config targets a local
  `llama-server` running Qwen3.6-35B-A3B.
- **Built-in tools** — `read`, `write`, `edit` (with diff prompt), `bash`,
  `grep`, `glob`, `web_fetch`, `todo`, `memory_save`.
- **Sandboxed `bash` tool** with docker/podman backends, configurable
  per-project via `.enso/config.toml`.
- **Permissions system** with allow/ask/deny lists, per-user "Allow +
  Remember" persistence (`.enso/config.local.toml`), and always-prompt
  overrides for high-blast-radius commands.
- **Session persistence to SQLite** with crash-safe resume.
- **Pluggable LSP integration**; `gopls` wired in by default.
- **`enso config` and `enso version` subcommands** (version reports build
  info for `go install` and a `git describe` string for `make build`).
- **First-run welcome flow** plus friendlier transport-level error messages.
- **Hugo documentation site** (`docs/`) published to GitHub Pages.

### Security

- **Private vulnerability reporting** via GitHub Security Advisories (see
  [`SECURITY.md`](SECURITY.md)).

[v2.13.0]: https://github.com/TaraTheStar/enso/compare/v2.12.0...v2.13.0
[v2.12.0]: https://github.com/TaraTheStar/enso/compare/v2.11.0...v2.12.0
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
