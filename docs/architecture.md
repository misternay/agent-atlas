# Architecture

```
Claude Code / Codex hooks ──stdin──▶ atlas hook (shim) ──unix socket──▶ atlasd
                                         │ (daemon down/busy)               │
                                         └─▶ sanitize locally ─▶ spool ─────┤ (drained on start + every 15s)
atlasd: ingest.Server → normalize (adapter + security) → Pipeline (single writer)
        → sqlite.Commit (events + projector deltas, one tx) → Hub.Publish (WebSocket)
        loopback REST /api/v1/* · WS /api/v1/stream · embedded React dashboard
        private Unix control socket for atlas CLI and daemon shutdown
        maintenance: stale sweep (30s), spool drain (15s), retention + WAL checkpoint (10m)
```

## Packages

| Package | Role |
|---|---|
| `internal/event` | Versioned event contract and shim envelope |
| `internal/adapters/{claude,codex}` | Capability-driven adapters: native payload → events (raw fields read in memory only) |
| `internal/security` | Allowlist, secret redaction, path collapsing, bounds |
| `internal/normalize` | Adapter registry. Envelope → validated sanitized events (shared by daemon and shim) |
| `internal/correlate` | Deterministic projector: events → sessions, spans, resource links |
| `internal/storage/sqlite` | WAL SQLite, migrations, single serialized writer, read pool, queries, retention |
| `internal/ingest` | Unix socket server, writer pipeline (batch ≤256 / 25 ms), gaps, stale sweep, spool drain |
| `internal/spool` | Bounded offline buffer of sanitized envelopes (one file each) |
| `internal/shim` | `atlas hook` logic |
| `internal/api` | REST, WebSocket hub with resumable cursor, auth/CSRF/Host guard, UI serving |
| `internal/graph` | Server-side Agent→Tool→Resource projection (`/api/v1/graph`) |
| `internal/telemetry` | OTLP/JSON mapping (GenAI semconv + `atlas.*` attrs) and opt-in exporter |
| `internal/install` | Order-preserving JSON merge for hooks, exact-restore uninstall, LaunchAgent |
| `internal/daemon` | Wiring, lifecycle, retention |
| `web/` | React + TypeScript + React Flow + ELK.js + TanStack Virtual + Zustand |

## Key decisions

- **Write-before-publish.** Deltas are broadcast only after the transaction commits. `events.seq` is the cursor. A WebSocket client subscribes *before* reading the max sequence, then catches up from storage, so no delta is lost or duplicated. Cursors that fall before retained history (or more than 5000 events behind) get `resync_required`. `after_seq=0` is a real cursor (catch up from the start); omit it for live-only. Catch-up re-sends every span an event touched (e.g. all spans closed by `session.ended`). After any snapshot reload the client restarts the stream from the snapshot cursor, so nothing committed during the fetch is lost.
- **One projector for live and replay.** `correlate.Projector.Apply(seq, event)` depends only on the event stream plus committed rows loaded on a cache miss. Replay runs a fresh projector over stored events. The stale sweeper commits synthetic `tool.stale` events instead of mutating rows, which keeps replay deterministic.
- **Deletions run on the writer.** Clear-history and retention run through `Pipeline.Exclusive` on the writer goroutine; the projector cache is reset before the next commit, the history `generation` is bumped and every client gets `resync_required`. Clients send `?gen=` on reconnect, so one that missed the broadcast still resyncs. Clear-history writes its cutoff in the same transaction as deletion and rejects pre-clear queued or spooled events at commit.
- **Projector cache vs. transactions.** The projector loads committed rows through the read pool (the writer connection is held by the transaction). On commit failure the cache is reset and the batch is counted as a `telemetry.gap`.
- **Order independence.** Hook processes race, so terminal status follows evidence precedence instead of arrival order. Duplicate `event_id`s are skipped before insert, so no sequence numbers are burned.
- **No OTel SDK.** SQLite is authoritative and OTLP is a pure projection (`/api/v1/traces/{id}?format=otlp`). Pinned to OTLP JSON 1.5 and semconv 1.37, with `atlas.attrs.version=1`.
- **Pure-Go SQLite** (`modernc.org/sqlite`). No cgo; one static binary each for `atlas` and `atlasd`.

## UI

- **Live:** a session sidebar grouped by harness and workspace (virtualized). The React Flow canvas loads ELK layered layout when graph data arrives, throttles layout to ≥400 ms, and recomputes it when node or edge identities change; updates are batched about every 150 ms. Default live window is 60 s plus all running calls; older calls fold into group nodes and large resource lists collapse. Edges are styled by evidence: solid = observed, dashed = derived, dotted = inferred. The Inspector shows status, evidence, outcome basis, resources with their source event, and the span's stored source events.
- **History:** sessions and tool-call search (time, harness, workspace, session id, tool, category, status, resource, evidence). The trace explorer has a waterfall (concurrent spans overlap on their own rows), the inspector, and a read-only replay scrubber.
- **Settings:** pause/resume collection, storage and retention, what is stored and what is never stored, the harness coverage matrix with observed vs configured hooks, and clear history.
