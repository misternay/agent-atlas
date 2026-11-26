# Security and privacy

## Data flow

Native hook JSON → `atlas hook` (memory) → Unix socket → adapter (memory) → `security.SanitizeEvent` → SQLite / WebSocket / spool / optional OTLP.

Raw payloads never touch disk. If the daemon is unreachable or busy, the shim runs the **same** normalize + sanitize code locally and spools only the sanitized events (`spool.Append` refuses envelopes that contain `raw`).

## What is stored

Event type, native hook name, timestamps, ids, tool name and category, and a bounded, secret-redacted target label: the program name and subcommand (e.g. `npm test`, never arguments), a workspace-relative or `~`-collapsed path, the MCP server and tool, or a URL host. Also status, evidence and outcome basis, duration, the workspace path with home collapsed, model and permission mode.

## Never stored

Prompts, tool output and responses, file contents, full command lines and arguments, environment variables, transcript paths, assistant messages, auth headers and tokens. The Settings page lists both sets.

## Redaction

- Allowlist of attrs; everything else is dropped.
- Secret patterns: Anthropic/OpenAI-style keys, GitHub/GitLab/Slack tokens, AWS key ids, Google API keys, JWTs, Bearer/Basic auth, private-key headers, credentials in URLs, `key=value`/`--flag value` for password/secret/token/api-key/cookie names, and long high-entropy blobs.
- Home directory (and its `-Users-name-…` encoded form) collapsed to `~`.
- Labels are bounded (160 runes). Credential-shaped or oversized identifiers are hashed to preserve correlation.
- Shell-command inference is best-effort and keeps only plausible file operands (`[A-Za-z0-9_./~+-]` only): recognized heredoc bodies are skipped and ambiguous forms stop inference, grep/rg/sed/awk patterns are never treated as files, flag values (`head -n 5`) are skipped, and URLs are never file labels. Regression tests: `TestCommandArgumentsNeverLeakAsFiles`.
- Home collapse is case-insensitive (macOS volumes are case-insensitive).
- Tests: `internal/security/sanitize_test.go`. `tests/integration/TestNoRawContentInDatabaseFiles` greps the raw `atlas.db`, `-wal` and `-shm` bytes for every content marker in the fixtures.

## Local access

| Area | Implementation |
|---|---|
| Ingest | Unix socket, mode 0600, inside the 0700 Atlas home or a private per-user directory under `/tmp` for long paths. The shim checks owner and permissions before sending raw events. The listener refuses to replace a live daemon's socket. 4 MB line limit and 3 s deadline per connection |
| HTTP | Bound to loopback only; atlasd refuses non-loopback addresses |
| Auth | The CLI manages the daemon through a separate Unix control socket and uses a 256-bit Bearer token (0600) for protected API requests. `atlas open` provides a single-use 60-second code in a URL fragment. The browser exchanges it for a separate session stored in tab-scoped, origin-scoped session storage. The browser sends it in an API authorization header or a same-origin WebSocket handshake. The session expires after seven days, a daemon restart, or closing the tab. |
| Host/Origin | The dashboard accepts only loopback Host addresses on its bound port. Browser requests with an Origin must match the request host. |
| CSRF | Non-GET requests require `X-Atlas-CSRF: 1` (cannot be sent cross-site without a preflight). Preflights are always refused; no CORS headers are ever sent |
| Headers | Strict CSP (`default-src 'self'`), `X-Frame-Options: DENY`, `no-referrer`, COOP/CORP same-origin, `no-store` |
| Files | Home 0700; DB, WAL, token, config, spool files 0600 |
| Export | OTLP export is off by default. `/api/v1/export/preview` shows exactly what would be sent. The exporter only exists when the user enables it in `config.json` |
| Lifecycle | `atlas pause`/`resume` pauses collection. `atlas clear-history --yes` deletes saved rows and advances a durable cutoff so pending pre-clear events cannot repopulate history. Existing sanitized spool files are removed by the normal drain. `atlas uninstall-hooks` restores unchanged input when a hook-free backup is available. |

## Behavior guarantees

- The hook never writes stdout, never returns a decision, and always exits 0. A 2.5 s hard ceiling exists in `cmd/atlas`, and internal timeouts are much shorter. It cannot approve, deny or alter tool calls.
- The installer respects native trust: Codex hooks still need approval in Codex. It never suppresses permissions.
