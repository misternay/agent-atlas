# Compatibility (Phase 0 spike results)

The original capture ran on **2026-11-23**, macOS arm64. On **2026-11-26**, Claude Code 2.1.332 passed a live Read session. Codex CLI and Codex Desktop each ran a real `pwd` call with Atlas hooks approved in the main profile. [Current readiness results](readiness.md)

| Harness | Version tested | How validated | Result |
|---|---|---|---|
| Claude Code CLI | 2.1.330 | Real `claude -p` runs with an isolated `--settings` file whose hooks call a capture script, then `atlas hook` (live e2e: `make e2e`) | **Observed.** Fixtures in `testdata/claude-hooks/` |
| Codex CLI | 0.172.0 | Native `codex exec` with user-approved Atlas hooks in the main profile | **Observed.** Session and turn lifecycle plus tool start and successful completion. Other signals in the matrix are based on the schema. |
| Codex Desktop | 26.1120.50957 (`0.171.0-alpha.13.2` bundled runtime) | User-started tool call, matched by session ID to Codex metadata with originator `codex_work_desktop` | **Observed.** Session start, turn start and end, and successful tool start and completion. |

## Codex hook approval

Codex runs a hook after the user reviews and trusts it from `/hooks`. The live CLI test followed that approval flow. Project-local `.codex/hooks.json` also requires project trust.

**To validate Codex on your machine:**
1. Run `atlas install-hooks --harness codex`.
2. Start Codex, open `/hooks`, and review and approve the Atlas hooks.
3. Start a new Codex CLI or Desktop session and run a tool call.
4. Run `atlas doctor`. The `codex observed` line lists which configured hooks have actually been seen.

The Settings page shows the same information under "Harness coverage".

## Capability matrix

| Signal | Claude Code 2.1.330 (observed) | Codex 0.170 (documented) | Atlas behavior |
|---|---|---|---|
| Session start/end | `SessionStart` (`source`), `SessionEnd` (`reason`) | `SessionStart`, `SessionEnd` | Session opened/closed. Placeholder session when start is missing (`start_observed=false`) |
| Turn | `UserPromptSubmit` → `prompt_id`; `Stop`/`StopFailure` | `UserPromptSubmit`/`Stop` → `turn_id`; `Interrupt` | Turn span (parent of its tool calls) |
| Tool start | `PreToolUse` (`tool_use_id`) | `PreToolUse` (`tool_use_id`) | Span opened |
| Tool success | `PostToolUse` (`duration_ms`) | `PostToolUse` | `completed` (observed) |
| Tool failure | `PostToolUseFailure` (`error`, `is_interrupt`) | None. Only an explicit `exit_code≠0`/`success:false` in `tool_response` | `failed`: observed (Claude) or derived (Codex exit code) |
| Denied by permission rule | **No dedicated hook fired.** `PreToolUse` fires, then no Post hook. `PostToolBatch` reports `"Permission to use … has been denied."` | `PermissionRequest` only | `denied` with evidence **inferred** (text classification) |
| Blocked by another PreToolUse hook | `PreToolUse` then no Post hook; `PostToolBatch` reports `"PreToolUse:<Tool> hook error: …"` | — | `denied`, **inferred** |
| `PermissionDenied` hook | Registered; not observed in the spike (fires for auto-mode classifier denials) | — | `denied`, observed, when it fires |
| Result reported, outcome unclear | `PostToolBatch` entry for a call with no Post hook | — | `unknown`, **derived** |
| MCP calls | `mcp__<server>__<tool>` via Pre/PostToolUse (observed with a local stdio MCP server) | `mcp__…` tool names | MCP resource edge (`connects_to`), observed |
| Subagents | `SubagentStart`/`SubagentStop` | `SubagentStart`/`SubagentStop` (`agent_id`, `agent_type`) | Recorded as events. No lineage (deferred, MVP+) |
| Hosted/internal tools | Not guaranteed | Not guaranteed | Not shown; coverage caveat in UI |

## Observed quirks that shaped the design

- **Correlate by `tool_use_id` only.** A PreToolUse hook (here `rtk`) rewrote `tool_input.command` from `ls …` to `rtk ls …` between Pre and Post.
- **Payloads carry no timestamp or event id.** The shim stamps `received_at` and an envelope id, and normalized event ids derive from it, so spool replay deduplicates.
- **Hooks run in parallel processes.** A `PostToolBatch` can be committed before the matching `PostToolUse`. Status precedence by evidence (observed > derived > inferred) makes the result order-independent.
- **Codex code mode** wraps tool calls in an `exec` host (`tools.exec_command`, `tools.apply_patch`). Per-tool hooks fire for the inner tools.
- **Unix socket paths are limited to 104 bytes on macOS.** For long Atlas homes, the daemon and clients derive the same short paths in a private, per-user directory under `/tmp`, including when their `TMPDIR` values differ.

## Hook overhead (measured)

`atlas hook` wall time including process spawn, with atlasd running: **p50 6.2 ms, p95 9.4 ms** over 100 runs (`go test ./tests/integration -run TestHookOverhead -v`). With atlasd down, the shim sanitizes and spools locally; the worst case seen in tests was well under the 1.5 s bound.
