# Changelog

All notable changes to ensō are documented here. The format is based
on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.0.0] - TBD

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

[Unreleased]: https://github.com/TaraTheStar/enso/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/TaraTheStar/enso/releases/tag/v1.0.0
