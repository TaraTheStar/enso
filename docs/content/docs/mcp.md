---
title: MCP servers
weight: 10
---

# MCP servers

[Model Context Protocol](https://modelcontextprotocol.io/) servers
expose tools to the agent over a JSON-RPC transport. ensō's MCP
client (built on `mark3labs/mcp-go`) connects to as many servers as
you configure and merges their tools into the same registry as the
built-ins.

## Configuration

```toml
# Stdio transport — server is a subprocess.
# Only $ENSO_*-prefixed env vars expand here; everything else collapses
# to "" (logged once). See [Secrets]({{< relref "secrets.md" >}}) for why.
[mcp.gitea]
command = "gitea-mcp-server"
args    = ["--token", "$ENSO_GITEA_TOKEN"]

# HTTP transport (Streamable-HTTP, falls back to SSE).
[mcp.notion]
url     = "https://mcp.notion.com/v1"
headers = { Authorization = "Bearer $ENSO_NOTION_TOKEN" }
```

| Field     | Description                                                                                          |
| --------- | ---------------------------------------------------------------------------------------------------- |
| `command` | Stdio transport: the binary to spawn. Mutually exclusive with `url`.                                 |
| `args`    | CLI args. `$VAR` / `${VAR}` resolve against the process env at startup.                              |
| `url`     | HTTP transport: the server's base URL. Mutually exclusive with `command`.                            |
| `headers` | HTTP transport only. Static map of header name → value, applied to every request. `$VAR` expanded.   |

Each block becomes one connection. ensō connects with a 10-second
per-server timeout and proceeds with whichever connect; failures are
logged but don't abort startup.

## Tool naming

MCP tools are exposed as `mcp__<server>__<tool>`. So a Gitea server
named `gitea` with a `list_repos` tool surfaces as
`mcp__gitea__list_repos`. This naming applies in:

- The `/tools` slash command output.
- Permission patterns: `mcp__gitea__*` matches every tool from that
  server.
- The model's tool list — the server's description is passed
  through.

## Permission patterns for MCP

```toml
[permissions]
allow = [
  "mcp__gitea__list_*",         # read-only Gitea ops
  "mcp__notion__search",        # search-only Notion access
]
ask = [
  "mcp__gitea__create_*",       # confirm creation
  "mcp__gitea__delete_*",       # confirm deletion
]
deny = [
  "mcp__gitea__delete_repo",    # never delete repos via MCP
]
```

Patterns use the same doublestar globbing as everything else in
permissions; `*` matches anywhere within a single segment.

## Discovering what a server provides

```
/tools
```

…in the TUI lists every tool currently registered, including all MCP
tools. The model also sees per-tool descriptions (provided by the
MCP server) when deciding whether to call.

## Built-in vs MCP

Built-in tools (`read`, `write`, `edit`, `bash`, etc.) are baked into
the binary and always available. MCP tools are loaded at startup from
config; missing servers don't affect anything else.

If you only want MCP and don't trust the built-ins for something,
write `[permissions] deny = ["bash(*)", "edit(**)"]` and rely entirely
on MCP servers for those operations.

## Authentication

MCP doesn't standardize auth — each server handles it differently.
Common patterns:

- **Stdio + token via args**: put the token in `args` and reference
  it with `$ENSO_FOO`. Only `ENSO_`-prefixed env vars expand —
  see [Secrets]({{< relref "secrets.md" >}}) for why. Keep the actual
  value in your shell config or a systemd `EnvironmentFile`, not the
  TOML.
- **HTTP + bearer**: set `headers = { Authorization = "Bearer $ENSO_FOO" }`.
  Any header works, so non-bearer schemes (`X-Api-Key`, `X-Auth-Token`,
  etc.) are equally fine. Same `ENSO_`-prefix rule applies.
- **HTTP + OAuth**: not currently exposed via config. Drop into the
  manager directly if you need it (`mark3labs/mcp-go` ships an
  `OAuthHandler`); file a request if you want it surfaced.

Check the specific server's docs for what it actually expects.

## Known servers

A few that work well with ensō:

- **gitea-mcp-server** — Gitea / Forgejo issues, PRs, labels, etc.
- **github-mcp-server** — GitHub equivalent (official from
  `github/mcp-server`).
- **kubernetes-mcp-server** — kubectl-style cluster operations.
- **filesystem-mcp-server** — generic file operations (largely
  redundant with ensō's built-ins).
- **opentofu-mcp-server** — Terraform / OpenTofu registry queries.

The MCP ecosystem moves fast; this list goes stale immediately. Check
[awesome-mcp-servers](https://github.com/punkpeye/awesome-mcp-servers)
for an up-to-date catalogue.
