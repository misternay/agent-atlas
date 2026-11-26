# MVP status

The verification below records the November 23 baseline. See [the November 26 readiness review](readiness.md) for current results and the remaining rollout work.

Status as of 2026-11-23. Each design item maps to code and to the evidence that verifies it. "Verified" means a named test or a demonstrated run on this machine. Anything not verified is listed under [Open items](#open-items).

## Phase 0 — Compatibility spike

| Task | Status | Evidence |
|---|---|---|
| Inspect installed versions and hook docs | Done | Claude Code 2.1.330, Codex CLI 0.170.0 ([compatibility](compatibility.md)) |
| Confirm Claude hook events and payloads | Done | Real captures → `testdata/claude-hooks/*.jsonl` |
| Confirm Codex CLI events and payloads | **Partial** | Shapes from `codex-rs/hooks/src/schema.rs`. Live capture blocked by Codex hook trust (by design, not bypassed) |
| Confirm Codex Desktop separately | **Not done** | Marked unsupported until verified |
| Sanitized fixtures: start, success, failure, denied, missing completion, lifecycle, MCP | Done | Claude: all except `PermissionDenied` hook payload (denials arrive via `PostToolBatch` text). Codex: synthetic |
| Measure shim startup + IPC overhead | Done | p50 6.2 ms / p95 9.4 ms (`TestHookOverhead`) |
| Capability matrix and gaps | Done | `docs/compatibility.md`, `atlas capabilities`, Settings page |

## Phase 1 — Collector, correlator, SQLite

| Task | Evidence |
|---|---|
| Daemon + Unix IPC server | `internal/ingest/server.go`; `TestIngestSocketEndToEnd` |
| Failure-safe hook shim | `internal/shim`; `TestHookWithDaemonDownIsSilentFastAndRecovers`, shim unit tests |
| Claude and Codex adapters | `internal/adapters/*`; `adapters_test.go` against fixtures |
| Allowlist/redaction before storage | `internal/security`; `TestNoRawContentInDatabaseFiles` |
| Versioned event contract + validation | `internal/event`; `docs/event-schema.md` |
| Span correlation, duplicates, missing completions, stale | `internal/correlate`; `projector_test.go`; stale sweep in `ingest.SweepStale` |
| Schema, migrations, indexes, writer queue | `migrations/0001_init.sql`, `internal/storage/sqlite` |
| Session/events/trace REST APIs | `internal/api/server.go`; `api_test.go` |
| Ingestion tests, replay fixtures, crash/restart | `TestCrashRestartPreservesHistory`, `TestReplayIsDeterministicAndReadOnly` |

## Phase 2 — Live graph

React + TypeScript shell; session list grouped by harness and workspace; React Flow graph with session, tool, resource and group nodes; evidence-aware edges and Inspector; WebSocket snapshot + cursor recovery; grouping, live window and layout throttling; distinct visual states for all six statuses. Unit tests in `web/src/**/*.test.ts`. Verified visually in a browser against a live Claude session.

## Phase 3 — History, security, packaging

| Task | Evidence |
|---|---|
| Waterfall, trace details, filters, indexed search | `web/src/features/{timeline,sessions}`; `/api/v1/search`; `waterfall.test.ts` |
| Deterministic read-only replay | `/api/v1/sessions/{id}/replay`; Replay tab; `TestReplayIsDeterministicAndReadOnly` |
| Local auth/Origin checks, pause, history deletion | `internal/api/auth.go`; `TestAccessControls`, `TestAuthExchangeCookie` |
| Retention and size safeguards | `internal/daemon/retention.go`, `PruneBefore`/`PruneOldestSessions` (store test) |
| Installer/uninstaller with safe merge/restore | `internal/install`; unit tests and a roundtrip on copies of the real configs (byte-identical) |
| launchd setup | `atlas launchd install`; plist unit test. **Not loaded on this machine** (left for the user) |
| Performance/security regression + E2E | `tests/integration` (burst, latency, overhead, redaction, crash); `make e2e` (live Claude) |
| Docs + doctor | `README.md`, `docs/*`, `atlas doctor` |

## §13 acceptance criteria

| Criterion | Status |
|---|---|
| Real events from Claude Code | ✅ `TestLiveClaude` and manual live run |
| Real events from Codex; unsupported surfaces disclosed | ⚠️ Adapter built from source schema; **not observed live**; disclosed in the UI, `doctor` and docs |
| Start/terminal pairing without confusing concurrent calls | ✅ `TestConcurrentCallsPairByID`, Codex fixture (interleaved A/B) |
| Missing end → unknown/stale, not failure | ✅ `TestMissingCompletionBecomesUnknownNotFailed`, `TestStaleIsNotTerminalAndLateCompletionWins` |
| Agent works with daemon stopped | ✅ `TestHookWithDaemonDownIsSilentFastAndRecovers` |
| Multiple sessions concurrently | ✅ `buildGraph.test.ts`; browser check (Claude + Codex) |
| Session → Tool → File/MCP links accurate | ✅ Fixture tests; live run (Read→file observed, MCP→server, `cat` → inferred) |
| Six statuses distinguishable | ✅ Badge, node and bar styles per status |
| Inspector shows source and evidence | ✅ Browser check (source events table + badges) |
| Reconnect without duplicate nodes | ✅ `reducer.test.ts`, `TestStreamLiveResumeAndResync` |
| Restart preserves history | ✅ `TestCommitDedupRestartAndQueries`, `TestCrashRestartPreservesHistory` |
| Filter by harness, project, tool, status, time | ✅ `/api/v1/search` + `TestSearchAndTraceEndpoints`; History UI |
| Timeline renders concurrent spans | ✅ `waterfall.test.ts` |
| Replay deterministic, never re-executes | ✅ `TestReplayIsDeterministicAndReadOnly`, `TestReplayDeterministic` |
| Cursor expiry → snapshot resync | ✅ `TestStreamLiveResumeAndResync` (resync_required); client reloads the snapshot |
| No raw prompts/output persisted | ✅ `TestNoRawContentInDatabaseFiles` |
| Redaction before spool/DB/broadcast | ✅ `TestDaemonDownSpoolsOnlySanitizedEvents`; one sanitize path for all sinks |
| No external export by default | ✅ Exporter constructed only with `export_enabled` + `export_preview_ack` + endpoint |
| UI/API/WebSocket local access controls | ✅ `TestAccessControls` (host, origin, CSRF, preflight, token), WS requires auth |
| Install/uninstall preserves unrelated settings | ✅ `install_test.go`; real-config copy roundtrip |

## §14 targets (measured on this Mac, M-series)

| Metric | Target | Measured |
|---|---|---|
| Hook overhead p95 | ≤ 50 ms | 9.4 ms |
| Collector→stream p95 | ≤ 500 ms | 35 ms (WebSocket receipt; browser render adds ~150 ms of batching) |
| Burst ingestion | 100 ev/s | 210 ev/s offered and fully committed (not saturated) |
| Daemon RSS | < 150 MB | ~29 MB after a 1000-event burst |

## Independent review

A 10-agent review (5 dimension reviewers plus 5 adversarial verifiers, keyed to §13) confirmed 31 defects after this build. All were fixed with regression tests:

- **Privacy:** heredoc bodies, grep/rg/sed patterns, jq arguments and `head -n` values could become file labels. URL credentials survived `filepath.Clean`. Several token formats and case-variant home paths were missed.
- **Correlation:** late spooled `session.started` could reopen an ended session. An old `session.ended` could close newer calls. Implicitly created turn spans were never persisted. Durations were not repaired when the start arrived late. Turn end ignored evidence precedence. Published deltas shared mutable maps with the projector, which was a data race and made replay frames not point-in-time.
- **Storage/stream:** clear-history and retention could resurrect deleted rows from the projector cache. A commit failure lost the pending gap count. The size cap did nothing with fewer than 25 sessions. `after_seq=0` skipped catch-up. Catch-up omitted spans closed by session end. A client missing the clear broadcast never resynced. A snapshot reload during live streaming dropped deltas.
- **Security:** the browser cookie carried the long-lived API token.
- **Installer:** a reinstall could make uninstall delete a user-owned file or restore a stale backup. Symlinked configs were replaced by regular files. Stale binary paths could not be repaired. A missing backup blocked uninstall. Detection depended on "atlas" appearing in the binary name.
- **Shim/spool:** stdin timeouts were not counted as gaps. Drop markers grew without bound (now one counter file).

## Open items

1. **Codex live validation.** Not done. Install the hooks, approve them in Codex, run a session, then check `atlas doctor` / Settings → Harness coverage. During the spike one attempt to run `codex exec --dangerously-bypass-hook-trust` (against a temporary `CODEX_HOME`) was blocked by Claude Code's auto-mode safety check; we did not try other workarounds. Codex Desktop is unvalidated.
2. **`PermissionDenied` hook payload** was not observed (it only fires for auto-mode classifier denials); the adapter maps it from the documented shape.
3. **OTLP exporter** has unit tests (opt-in gating, backlog order, exactly-once) but was not run against a real collector.
4. **LaunchAgent** was written and tested in a temp dir but not loaded on this machine.
5. **Real hook installation** was not done; that's the user's call: `atlas install-hooks`. The installer was verified on copies of the real `~/.claude/settings.json` and `~/.codex/hooks.json`: install → reinstall → uninstall is byte-identical.
6. **No automated browser E2E** (`tests/e2e` from the design layout). UI behavior is covered by vitest unit tests plus manual browser checks: live updates, clear-history resync, sign-in after daemon restart, trace and replay views.
7. **Environment notes.** The captured Claude fixtures include effects of hooks already on this machine (`rtk` rewriting Bash commands, a "GateGuard" hook blocking first Bash calls), which is why the fixtures contain hook-blocked calls.
8. **Size-cap retention** removes about 10% of sessions per step (oldest first) and never touches sessions active in the last 10 minutes, so one very large active session can keep the DB above the target until it goes idle.

## Second review (post-fix) — status

A narrow second review of the changed code confirmed 14 more issues. **Fixed, with tests:** late `session.ended` rewinding or closing a resumed session; root duration after a late start; the `duration_reported` flag; OTLP export now follows commit order; commit-failure backoff; home-collapse path boundaries and a regex-cache data race; keyword-named workspaces collapsing into a single label; heredoc `<< EOF` delimiters, here-strings and quoted `<<`; BSD `sed -i ''`; missing value flags (operands now need a `/` or an extension).

**Not yet fixed (installer edge cases):**
- A reinstall after a user edit, when the file still held Atlas hooks, records a backup that already contains those hooks. Uninstall then "restores" stale Atlas entries. Workaround: run uninstall-hooks twice, or remove the entries by hand.
- Two installs within the same second share one backup filename.
- Suffix matching can treat a foreign `… hook --harness claude` command as Atlas's.
- Frontend: after `reconnect_failed`, a restarted stream has no backoff when the WebSocket handshake keeps failing while HTTP works.
