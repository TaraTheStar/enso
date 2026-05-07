---
title: JSON event schema
weight: 2
---

# JSON event schema

`enso run --format json` emits one JSON object per line on stdout —
newline-delimited JSON, parseable with any streaming JSON tool
(`jq -c .`, `python -m json.tool --json-lines`, etc.).

The schema mirrors the internal bus events 1:1, with a leading
`session_start` and trailing `session_end` for boundaries.

## Example

```bash
$ enso run --yolo --format json "list .go files in cmd/" | head
{"type":"session_start","cwd":"/home/me/proj","id":"4d8b2e9a-…","model":"qwen3.6-35b-a3b","resumed":false}
{"type":"user_message","content":"list .go files in cmd/"}
{"type":"reasoning_delta","text":"The user wants…"}
{"type":"reasoning_delta","text":" to enumerate…"}
{"type":"tool_call_start","args":{"pattern":"**/*.go"},"id":"call_1","name":"glob"}
{"type":"tool_call_end","error":null,"id":"call_1","name":"glob","result":"cmd/enso/main.go\ncmd/enso/run.go\n…"}
{"type":"assistant_delta","text":"There are five Go files in cmd/:\n"}
…
{"type":"assistant_done"}
{"type":"session_end","tool_errors":false}
```

## Event types

Every line has a `"type"` field. Other fields depend on the type.

### `session_start`

Emitted exactly once, before anything else.

```json
{"type":"session_start","id":"<uuid>","model":"<model>","cwd":"<abs path>","resumed":false}
```

| Field      | Type    | Description                                        |
| ---------- | ------- | -------------------------------------------------- |
| `id`       | string  | UUID of the session. Empty in `--ephemeral` mode.  |
| `model`    | string  | Provider model name.                               |
| `cwd`      | string  | Absolute project cwd.                              |
| `resumed`  | bool    | `true` when the session was loaded via `--resume`. |

### `user_message`

The prompt ensō received (CLI arg, stdin, or skill expansion).

```json
{"type":"user_message","content":"<text>"}
```

### `reasoning_delta`

Streaming reasoning content (`<think>` blocks for Qwen, ditto for
DeepSeek-R1, etc.). Not appended to history — the model re-derives
reasoning each turn. May not appear at all for non-reasoning models.

```json
{"type":"reasoning_delta","text":"<chunk>"}
```

### `assistant_delta`

Streaming assistant text (the visible reply).

```json
{"type":"assistant_delta","text":"<chunk>"}
```

### `assistant_done`

Marks the end of an assistant message. Tool calls follow if the
message included any.

```json
{"type":"assistant_done"}
```

### `tool_call_start`

A tool call is about to run.

```json
{"type":"tool_call_start","id":"<call-id>","name":"<tool>","args":{...}}
```

### `tool_call_end`

A tool call finished.

```json
{"type":"tool_call_end","id":"<call-id>","name":"<tool>","result":"<text>","error":null}
```

`error` is the error string (or `null`). When the tool was denied by
the permission system, an extra `"denied":true` field is set.

### `compacted`

Auto-compaction fired.

```json
{"type":"compacted","reason":"<why>","summary":"<one-line summary>"}
```

### `agent_start`

A subagent (via `spawn_agent` or workflow role) started.

```json
{"type":"agent_start","id":"<agent-id>","parent_id":"<parent>","depth":1,"prompt":"<truncated>"}
```

`role` is also set when the agent comes from a workflow.

### `agent_end`

```json
{"type":"agent_end","id":"<agent-id>","parent_id":"<parent>","error":"<msg or empty>"}
```

### `cancelled`

The turn was cancelled (Ctrl-C or signal).

```json
{"type":"cancelled"}
```

### `error`

The agent loop hit a non-tool error.

```json
{"type":"error","message":"<text>"}
```

### `permission_auto_deny`

A permission prompt fired but no client was available (e.g.
non-interactive mode). The tool call is denied automatically.

```json
{"type":"permission_auto_deny","tool":"<tool>"}
```

### `session_end`

Emitted exactly once at the end. `tool_errors` is `true` if any tool
call errored or was denied during the run.

```json
{"type":"session_end","tool_errors":false}
```

If the run ended in error, the field `error` carries the message:

```json
{"type":"session_end","tool_errors":false,"error":"context deadline exceeded"}
```

## What's not emitted

- `EventPermissionRequest` — these payloads contain a Go channel for
  the response and don't serialize. In non-interactive mode requests
  are auto-denied (and surface as `permission_auto_deny`). For
  interactive permission flow, use `enso tui` or `enso attach`.
- Per-character cursor / focus events from the TUI; the JSON stream
  is agent-level only.
