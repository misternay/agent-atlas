# Agent Atlas

See what your coding agent did, which files and MCP tools it touched, and how each tool call ended. Agent Atlas is a local observability tool for Claude Code, Codex CLI, and Codex Desktop.

- **Live graph** connects agent sessions, tool calls, files, and MCP tools as events arrive.
- **Searchable history** lets you filter saved tool calls and inspect their trace timelines.
- **Read-only replay** reconstructs a session from stored events without rerunning tools.
- **OpenTelemetry** lets you download a trace as OTLP JSON or opt in to forwarding completed traces to a collector.

## Quick start

Build from source on macOS. Live capture was tested on Apple silicon. You'll need Go 1.26+, Node.js 22.12+ on the 22.x line or Node.js 24+, pnpm 10, and at least one supported agent.

Clone the repository, then build and start Atlas.

```sh
git clone https://github.com/misternay/agent-atlas.git
cd agent-atlas
make all
bin/atlas start
```

Install the hooks for the agent you use. The installer backs up existing settings before merging the new entries.

| Agent | Install command | Next step |
| --- | --- | --- |
| Claude Code | `bin/atlas install-hooks --harness claude` | Start a new Claude Code session. |
| Codex CLI or Desktop | `bin/atlas install-hooks --harness codex` | Open `/hooks` in Codex CLI, review and approve the Atlas entries, then start a new session in CLI or Desktop. |

Ask the agent to run a tool, such as reading a file or running `pwd`. Then check that Atlas received events and open the signed-in dashboard.

```sh
bin/atlas doctor
bin/atlas open
```

Atlas listens on `127.0.0.1:7878` by default. Keep the binaries at a stable path because the installed hooks reference `bin/atlas`. See the [operations guide](docs/operations.md) for custom data paths, troubleshooting, and optional startup at login.

## Verified coverage

| Runtime | Version | Live check |
| --- | --- | --- |
| Claude Code | 2.1.332 | Completed a file read |
| Codex CLI | 0.172.0 | Paired the start and completion of a shell tool call |
| Codex Desktop | 26.1120.50957 | Paired the start and completion of a shell tool call in a Desktop session |

Atlas labels relationships inferred from shell commands as **inferred**. Tool calls with a missing completion can appear as **stale** or **unknown**. See the [compatibility matrix](docs/compatibility.md) for hook coverage by signal.

## Data and privacy

Atlas receives native hooks over a Unix socket, sanitizes event metadata, and writes it to local SQLite. The database stores fields such as event types, timings, statuses, and bounded target labels. Atlas drops prompts, tool output, and file contents before persistence.

The dashboard and API listen on loopback. Data stays local by default, under `~/Library/Application Support/AgentAtlas/`. Use `bin/atlas pause` and `bin/atlas resume` to control collection. History follows the configured retention policy. To delete it immediately, run `bin/atlas clear-history --yes` while the daemon is running.

Use these commands to remove the hooks and stop a manually started daemon.

```sh
bin/atlas uninstall-hooks
bin/atlas stop
```

See [security and privacy](docs/security.md) for redaction rules and [OTLP configuration](docs/operations.md#optional-opentelemetry-export) for forwarding traces.

## Development and contributing

Build with `make all` before running the test suite. The Go daemon embeds the React dashboard. The CLI and daemon entry points live in [`cmd/`](cmd/), the backend in [`internal/`](internal/), and the frontend in [`web/`](web/).

| Command | What it checks |
| --- | --- |
| `make test` | Go vet, backend and integration tests, TypeScript, and frontend tests |
| `make browser` | Sign-in, live graph, history, OTLP download, and replay in headless Chrome |
| `make bench` | Hook overhead, ingest throughput, WebSocket latency, and daemon memory |
| `make e2e` | A live Claude Code tool call, using a signed-in CLI and model quota |

`make browser` runs with a temporary Chrome profile. Set `CHROME_BIN` if Chrome is installed outside its default macOS path.

For a bug report, include your operating system, agent version, and steps to reproduce. Share synthetic or scrubbed fixtures. For a pull request, describe the behavior change and the checks you ran.

[Architecture](docs/architecture.md) · [Event schema](docs/event-schema.md) · [Verification and benchmarks](docs/readiness.md)

## License

Project licensing is pending.
