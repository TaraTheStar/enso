---
title: LSP
weight: 5
---

# LSP integration

Configure language servers under `[lsp.<name>]` and four tools become
available to the agent:

- `lsp_hover(file, line, column)` — type / signature / doc at a position.
- `lsp_definition(file, line, column)` — where the symbol is defined.
- `lsp_references(file, line, column, include_declaration?)` — every reference.
- `lsp_diagnostics(file)` — errors / warnings the server has published.

Servers are spawned lazily on first use, scoped by file extension,
and reused for the rest of the session. ensō's LSP client is
hand-rolled JSON-RPC over stdio (no new dependencies; ~600 lines of
pure Go), so anything that speaks standard LSP works.

## Configuration

```toml
[lsp.go]
command      = "gopls"
extensions   = [".go"]
root_markers = ["go.mod", ".git"]

[lsp.typescript]
command      = "typescript-language-server"
args         = ["--stdio"]
extensions   = [".ts", ".tsx", ".js", ".jsx"]
root_markers = ["package.json", "tsconfig.json", ".git"]
init_options = { preferences = { quotePreference = "single" } }

[lsp.python]
command      = "pyright-langserver"
args         = ["--stdio"]
extensions   = [".py"]
root_markers = ["pyproject.toml", "setup.py", ".git"]

[lsp.rust]
command      = "rust-analyzer"
extensions   = [".rs"]
root_markers = ["Cargo.toml", ".git"]

[lsp.cpp]
command      = "clangd"
args         = ["--background-index"]
extensions   = [".c", ".cpp", ".cc", ".h", ".hpp"]
root_markers = ["compile_commands.json", "CMakeLists.txt", ".git"]

[lsp.ruby]
command      = "ruby-lsp"
extensions   = [".rb"]
root_markers = ["Gemfile", ".git"]
```

## Field reference

| Field          | Required | Description                                                                                                                            |
| -------------- | -------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `command`      | yes      | The executable to spawn.                                                                                                               |
| `args`         | no       | Extra CLI args. Many servers need `--stdio`.                                                                                           |
| `extensions`   | yes      | File suffixes (with the leading dot) that route to this server. Case-insensitive.                                                      |
| `root_markers` | no       | Filenames the manager walks up from the file looking for the project root. First match wins. Falls back to the project cwd.            |
| `init_options` | no       | Opaque blob passed verbatim as `initializationOptions` in the LSP `initialize` request. Server-specific.                               |
| `env`          | no       | Extra `KEY=value` pairs for the server process. Inherits the parent enso env.                                                          |
| `language_id`  | no       | The LSP `languageId` to send on `didOpen`. Defaults to the config-block name (`<name>` in `[lsp.<name>]`).                             |

## How the model uses these tools

The agent typically reaches for LSP when:

- Investigating an unfamiliar codebase: `lsp_hover` on a function name
  to read its signature without grepping for the declaration.
- After editing: `lsp_diagnostics(file)` to confirm the file compiles
  cleanly without invoking the full build.
- Tracing impact: `lsp_references(file, line, col)` to find every
  caller before changing a function's signature.

Position arguments are **1-based** for parity with editor and grep
output. The client converts to LSP's 0-based positions internally.
Multi-byte characters use rune position (no UTF-16 conversion); for
ASCII-only files this is identical to LSP's spec.

## Limitations

- Diagnostics are pulled from the server's `publishDiagnostics`
  notifications — there can be a brief lag after `didOpen` before
  results arrive. If `lsp_diagnostics` returns empty for a file you
  just opened, retry after a moment.
- Symlinks are followed normally; the server sees the canonicalized
  path.
- The agent's bash tool runs in a different namespace from the LSP
  servers (especially when the sandbox is on). Servers see the host
  filesystem; bash sees the container's view. This is by design —
  language servers want host paths, not container paths.

## Daemon mode caveat

The daemon path does not register `lsp_*` tools (per-session cwd
issue). Use `enso run` or `enso tui` if you need them.
