# Security policy

## Reporting a vulnerability

Please report security issues **privately**. Do not open a public
issue, PR, or discussion thread for anything that looks like a
vulnerability.

Two ways to reach the maintainer:

1. **GitHub private vulnerability reporting** (preferred) — on the
   repo, click **Security → Report a vulnerability**. This opens a
   private advisory only the maintainer can see.
2. **Email** — <TaraTheStar@proton.me>. PGP not currently offered;
   if you need encrypted transport, say so in a first-contact email
   and we'll arrange a key exchange.

Please include:

- A description of the issue and its impact.
- Steps to reproduce, or a proof-of-concept.
- The ensō version (`enso version`) and OS.
- Whether you'd like credit in the eventual advisory.

## Scope

In scope:

- The `enso` binary and everything under `cmd/` and `internal/`.
- Default configs shipped in this repo (`.enso/config.toml`,
  embedded defaults).
- The daemon's unix-socket protocol and permission-prompt path.
- Sandbox escape from the bash sandbox into the host.
- Trust-prompt bypasses (anything that lets a project config
  silently override security-relevant user settings — see
  `cmd/enso/trust.go`).

Out of scope:

- Third-party MCP servers, LSP servers, or model providers users
  configure themselves.
- Issues that require an attacker who already has code execution
  as the user running ensō.
- Bugs in the docs site that don't affect the binary.

## Disclosure

We aim to acknowledge reports within 7 days and ship a fix or a
mitigation plan within 30 days for confirmed issues. Coordinated
disclosure timelines are negotiable for complex bugs; please say so
in your report.
