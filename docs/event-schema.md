# Event schema (v1)

Source of truth: `internal/event/event.go`. All events are sanitized before they are persisted, broadcast or spooled.

```json
{
  "schema_version": 1,
  "event_id": "evt_660f3ae9a30b7000130762bab",
  "timestamp": "2026-11-23T15:59:42.923973Z",
  "host_id": "local",
  "harness": "claude",
  "session_id": "90bcb93b-41ec-4547-8bf6-eb62ee302e6b",
  "turn_id": "e1d1a303-…",
  "tool_call_id": "toolu_01…",
  "type": "tool.started",
  "source_event": "PreToolUse",
  "workspace": "~/projects/demo",
  "tool": { "name": "Bash", "category": "shell", "operation": "execute" },
  "target": { "kind": "process", "label": "npm test" },
  "resources": [
    { "kind": "process", "label": "npm test", "relation": "invokes", "evidence": "observed" }
  ],
  "status": "running",
  "evidence": "observed",
  "privacy": "metadata_only",
  "attrs": { "permission_mode": "auto", "input_keys": "command,description" }
}
```

## Types

| Type | Meaning |
|---|---|
| `session.started` / `session.ended` | Observed session lifecycle |
| `turn.started` / `turn.ended` | Observed turn boundaries (`turn.ended` with `attrs.interrupted` for Codex `Interrupt`) |
| `tool.started` | Tool invocation began |
| `tool.completed` | Tool returned successfully |
| `tool.failed` | Failure confirmed by an explicit signal (`outcome_basis` says which) |
| `tool.denied` | Denial observed |
| `tool.batch_reported` | Harness reported the call's result to the model. Status `denied` (inferred from denial text) or `unknown` (derived) |
| `tool.stale` | Synthetic. No completion within `stale_after_secs`. **Not a failure** |
| `permission.requested` | Harness asked the user for permission |
| `subagent.started` / `subagent.stopped` | Subagent lifecycle (no lineage in MVP) |
| `telemetry.gap` | Lost events (`attrs.gap_count`, `attrs.gap_reason`) |

## Status and evidence

- Status: `running`, `completed`, `failed`, `denied`, `unknown`, `stale`. `stale` is not terminal.
- Evidence: `observed` > `derived` > `inferred` > `unknown`. A span's terminal status is replaced only by a signal with **stronger** evidence. That makes the outcome independent of hook process ordering.

## Allowlisted attrs

`source`, `reason`, `permission_mode`, `model`, `interrupted`, `hook_blocked`, `agent_type`, `trigger`, `gap_count`, `gap_reason`, `stale_after_s`, `exit_code`, `outcome_basis`, `start_missing`, `effort`, `input_keys` (field *names* only). Anything else is dropped.

## Identity

- `sessions.id = "s_" + sha256(host_id, harness, session_id)[:20]`. Native ids are never assumed globally unique.
- `trace_id = sha256("trace", session_row_id)[:32]` (W3C 128-bit). Span ids are 64-bit hashes of (session, turn id) or (session, tool_call_id).
- `parent_span_id` is set only when grounded: tool → turn when the source supplies a turn id, else tool → session root. Never inferred from timestamps.
- `events.seq` (SQLite `AUTOINCREMENT`) is the canonical order and the stream cursor.

## Wire envelope (shim → daemon)

Newline-delimited JSON over the Unix socket. The reply is one line: `ok`, `busy`, `rejected` or `paused`.

```json
{"v":1,"envelope_id":"660f…","received_at":"…Z","harness":"claude","host_id":"local","raw":{…native hook JSON…}}
```

`raw` exists only in memory and on the socket. Offline spool files carry `events` (already sanitized) instead of `raw`.

## Stream messages (WebSocket `/api/v1/stream?after_seq=N`)

`hello {cursor, paused}` · `delta {seq, event, session, spans[], links[], replay?}` · `resync_required {reason}` · `status {paused}`.
