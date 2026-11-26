# Test fixtures

All fixtures are scrubbed: usernames, home paths and temp paths are replaced with `/Users/dev/...`.
Content fields (prompts, tool output, file contents) are replaced with marker strings prefixed `SECRET-` or `SYNTHETIC-`
so redaction tests can assert that none of them reach the database.

| File | Basis |
|---|---|
| `claude-hooks/session-real-2.1.330.jsonl` | Real Claude Code 2.1.330 hook payloads (spike run): Read, Bash success, Bash failure (`PostToolUseFailure`), calls blocked by another PreToolUse hook (no terminal hook; reported via `PostToolBatch`), Stop, SessionEnd. |
| `claude-hooks/session-real-mcp-denied-2.1.330.jsonl` | Real Claude Code 2.1.330 payloads: MCP tool call (`mcp__atlasecho__echo`), a call denied by a permission rule (no PermissionDenied hook fired; denial visible only in `PostToolBatch` text). |
| `codex-hooks/session-documented-synthetic.jsonl` | Synthetic payloads shaped after `codex-rs/hooks/src/schema.rs`. Not captured live (Codex hook trust blocked unattended capture). Includes concurrent calls, exit-code failure, apply_patch, MCP, secrets in arguments, and a call with no completion. |
