# Agent Atlas readiness review

Verification ran on 2026-11-26 on Apple M3 Pro, macOS arm64, with Go 1.27.4, Node 24.21.0, and pnpm 10.15.0.

## Runtime verification

| Surface | Observed result |
|---|---|
| Build and tests | `make all`, `make test`, and `go test ./... -count=1` passed. The frontend suite passed all 12 tests. The uncached integration suite exercised real binaries and isolated homes. |
| CLI lifecycle | `TestCLILifecycleOnEphemeralPort` exercised start, authenticated status, pause/resume, creation of a sign-in URL on the bound port, embedded HTML serving, stop, and rejection of an unknown harness. |
| Long Atlas home | `TestLongAtlasHomeWorksAcrossTempDirectories` committed a hook event directly and verified `atlas status`, observed hook names in `atlas doctor`, and `atlas stop` from a process with a different `TMPDIR` than the daemon. A temporary LaunchAgent also passed authenticated health under the same conditions and was removed after the check. |
| Claude Code 2.1.332 | `make e2e` ran with isolated settings and the Read tool; Atlas recorded a Claude session and a completed Read span. |
| Codex CLI 0.172.0 | The app server reported all 10 Atlas entries trusted in the main profile after user approval. A read-only `pwd` call produced `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop`, and `SessionEnd`. `atlas doctor` reported all six, and the session trace paired the tool events as one completed Bash span. |
| Codex Desktop 26.1120.50957 | A user-started `pwd` call produced `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, and `Stop`. The matching session metadata identified `codex_work_desktop` and runtime `0.171.0-alpha.13.2`. Atlas recorded one completed Bash span with event sequences 10 and 11. |
| Dashboard | `make browser` passed in headless Chrome 158. It signed in, received synthetic events over the live stream, opened the persisted trace in History, rendered a completed timeline span, downloaded its OTLP JSON, stepped through Replay, and showed the recorded hook names in Settings. |
| macOS LaunchAgent | Generated plist passed `plutil -lint`; a temporary label launched the daemon with binaries staged outside Documents, reported healthy, and was unloaded and removed. |
| OTLP | OpenTelemetry Collector 0.166.0 accepted and decoded a synthetic trace from the real daemon over loopback after explicit opt-in. |

## Code and defensive security review

| Finding | Resolution and evidence |
|---|---|
| Installer could collide on backup names, adopt unrelated hook commands, restore stale hooks, and exit successfully on target failure. | Unique private backups, exact hook ownership, serialized state changes, rollback on state-write failure, and nonzero CLI status. Regression tests cover same-second reinstall, user edits, foreign hooks, symlinks, and prior behavior. |
| Daemon shutdown trusted a stale numeric PID. CLI requests sent the persistent token over HTTP to the configured listener. | Local CLI control uses a private Unix socket, checks ownership and mode, and holds an exclusive daemon lock through shutdown. Browser HTTP remains loopback. |
| Browser session cookies were shared across loopback ports. | One-time exchange, origin-scoped tab storage, and same-origin request credentials. The long-lived local API token is separate. Defensive tests verify one-time redemption, session/token separation, and WebSocket protocol negotiation. |
| Clearing SQLite could leave older events queued for replay. | The delete transaction persists the cutoff and advances stream generation. The writer discards pre-clear events, including after restart. Sanitized spool files are removed during the normal drain. |
| Shell inference could mistake heredoc contents for file labels, and secret-like IDs could survive sanitization. | Track recognized delimiters and stop at ambiguous syntax. Hash oversized and credential-like IDs. Verified fixture sanitization, sanitized spool and database privacy checks. |
| WebSocket failures could trigger repeated immediate reconnects after a snapshot refresh. | Bound retries with backoff, resume from the fresh cursor, and ignore superseded snapshots. The fake-timer regression checks a reachable HTTP API with a failed WebSocket handshake. |
| OTLP could follow redirects to another endpoint. | Disable redirects and verify the exporter reports them without forwarding payloads. |
| Five published advisories appeared in the Vitest 3 development dependency tree. | Upgraded to Vitest 4.1.12. `pnpm audit`, `pnpm audit --prod`, and `govulncheck ./...` reported no known vulnerabilities at verification time. `go test -race ./internal/...` passed. |

The security review covered access controls, privacy before persistence, local control, and dependency advisories. Tests exercised defensive behavior and did not establish the result of an independent penetration test. The local OS account and JavaScript served at the dashboard origin remain part of the trust boundary. Older databases and offline spool files may contain metadata retained by a previous release's redaction rules.

After Desktop validation, a fresh run passed the targeted access-control, one-time sign-in, database and offline-spool privacy, and OTLP destination checks.

## Performance

| Measure | Design target | Observed |
|---|---:|---:|
| Hook wall time including spawn, p95 | ≤ 50 ms | 11.60 ms across 100 calls |
| Collector receipt to WebSocket receipt, p95 | Component of ≤ 500 ms UI target | 31.56 ms |
| Local socket send to positioned Chrome graph node, p95 | ≤ 500 ms | 445 ms across 24 warm sequential starts, p50 385 ms |
| Paced burst ingestion | ≥ 100 events/s | 1,000 events in 4.750 s, 211 events/s |
| Daemon RSS | < 150 MB idle | 18,176 KiB idle in the browser run, 28,848 KiB after the latest burst |

`make bench` measures the hook and backend paths. `make browser` starts a temporary daemon and Chrome profile. The browser timing starts when sending each event to the local ingest socket and ends at the first animation frame with its visible, positioned DOM node. It samples sequential starts. Burst-time browser frame latency and saturation throughput were outside this measurement.

ELK layout is loaded on demand. The initial JavaScript chunk fell from 1,935 kB to 496 kB uncompressed, or 597 kB to 157 kB gzip. The ELK chunk is 1,423 kB uncompressed. The Live view retains in-memory rows between snapshots, and the replay slider folds events from the beginning on each seek. Replay reports truncation above 100,000 events. Very long browser sessions and large replay traces remain scalability limits.

## Rollout status

- Manual local use and authenticated dashboard are verified in temporary Atlas homes. The main Codex profile now has the Atlas entries, and the manually started daemon is healthy at `127.0.0.1:7878`. The default startup service is not registered.
- Live tool start and completion are verified in both Codex CLI and Desktop with the approved main profile. See the [compatibility matrix](compatibility.md) for coverage by signal.
- Claude's dedicated `PermissionDenied` hook payload is still unobserved. See [compatibility](compatibility.md) for how inferred denials are labeled.
