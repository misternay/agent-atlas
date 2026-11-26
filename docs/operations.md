# Agent Atlas operations

## Setup and upgrades

Build with `make all` from the project root. Keep `bin/atlas` and `bin/atlasd` together at a stable path. If you move the binaries, run `atlas install-hooks` from the new path with the same Atlas home. The installer uses its previous record to update the exact old hook command. If the record is absent, provide the old binary path to `atlas uninstall-hooks --atlas-bin /absolute/path/to/old/atlas` before reinstalling.

When upgrading a running older build, stop it with the old `atlas` binary before replacing the binaries. The new `atlas` uses the private control socket for daemon commands. Reopen dashboard tabs with `atlas open` after upgrading.

The installer honors `CLAUDE_CONFIG_DIR` and `CODEX_HOME`. To work on individual settings files, use `--claude-settings` and `--codex-hooks` on `install-hooks`, `uninstall-hooks` and `doctor`. A failed target returns a nonzero exit status. Other selected targets may already have succeeded, so read the command output before retrying.

## Start, diagnose, and stop

| Goal | Command or observation |
|---|---|
| Start manually | `bin/atlas start`; add `--foreground` for terminal logs |
| Check server and ingestion | `bin/atlas status` and `bin/atlas doctor` |
| Sign in to the browser | `bin/atlas open`; `--print` prints a short-lived sign-in URL |
| Control collection | `bin/atlas pause`, `bin/atlas resume`; the dashboard's Pause view controls rendering only |
| Enable after macOS login | `bin/atlas launchd install`; check `launchctl print gui/$(id -u)/com.agentatlas.atlasd` |
| Disable after login | `bin/atlas launchd uninstall`; use `bin/atlas stop` for manually started instances |
| Remove hooks | `bin/atlas uninstall-hooks`; specify `--harness claude` or `--harness codex` for one profile |
| Clear stored traces | `bin/atlas clear-history --yes` |

Use a single Atlas home for a collection instance. Set `ATLAS_HOME` before starting `atlasd` and before launching the agent CLI so its hook process inherits that value. The `atlasd.log`, `config.json`, `atlas.db`, and installation state are under that directory. A launchd installation captures the current Atlas home in the plist. Use `atlas launchd install --dir /path --no-load` to generate and inspect a plist without registering it.

If `doctor` lists hooks as installed but shows no observed events, check native trust in Codex and start a new harness session. A missing or busy daemon can temporarily leave sanitized envelopes in `spool/`; the daemon drains that directory on start and periodically while running. If the dashboard is disconnected but status works, reload its tab and run `atlas open` again if prompted.

## Verify Codex in an isolated profile

Run these commands from the built project in one shell. They create a separate Codex profile and Atlas home. A fresh `CODEX_HOME` needs its own Codex sign-in before running a model-backed session.

```sh
atlas_smoke=$(mktemp -d /tmp/atlas-codex-smoke.XXXXXX)
export ATLAS_HOME="$atlas_smoke/atlas" CODEX_HOME="$atlas_smoke/codex"
mkdir -m 700 "$ATLAS_HOME" "$CODEX_HOME" "$atlas_smoke/work"
printf '{"addr":"127.0.0.1:0"}\n' > "$ATLAS_HOME/config.json"
bin/atlas start
bin/atlas install-hooks --harness codex
codex -C "$atlas_smoke/work"
```

In Codex, sign in if prompted, review the generated commands in `/hooks`, and approve the Atlas hooks you want to test. Run a harmless tool call in the temporary workspace, such as `pwd`. Exit Codex and run `bin/atlas doctor` in the same shell. A successful probe shows `codex observed` with a recent timestamp and reports that `SessionStart`, `PreToolUse`, and `PostToolUse` have been seen. Open the session in Live or History to verify the matching tool start and completion. Use `bin/atlas stop` when finished. Close the shell to discard the environment overrides.

## Recover or remove hook configuration

To recover a harness config, use `uninstall-hooks` first. Atlas restores the exact previous bytes when the file still matches what Atlas wrote and the backup contains no Atlas hooks. After user edits or a reinstall with active hooks, it removes recorded Atlas commands and preserves unrelated settings. Every modification creates its own backup with mode `0600`. Original backup filenames are printed by the installer.

## Start at login

Place the binaries outside macOS-protected folders such as `Documents` before registering the service. During verification, an agent using the binaries in `Documents` failed to open the control socket and produced an empty startup log. The same build launched and passed health checks from an unprotected directory.

```sh
mkdir -p "$HOME/.local/bin"
install -m 0755 bin/atlas bin/atlasd "$HOME/.local/bin/"
"$HOME/.local/bin/atlas" launchd install
"$HOME/.local/bin/atlas" status
```

If a manually launched Atlas daemon is already running, stop it before loading the service. Run the installer again from the new binary path for any hooks you already configured. To remove the service, run `"$HOME/.local/bin/atlas" launchd uninstall`.

## Change the port or retention

Create a private `config.json` under the Atlas home and restart the daemon after editing it. Unspecified values use defaults. The address must be loopback.

```json
{
  "addr": "127.0.0.1:7878",
  "retention_days": 14,
  "max_db_bytes": 1073741824,
  "spool_max_bytes": 33554432
}
```

A large active session may exceed the size target until it becomes eligible for pruning. For isolated local testing, use another `ATLAS_HOME` with `"addr": "127.0.0.1:0"`. `atlas start` displays the bound port and `atlas open` uses that port. Hooks resolve the Unix socket from the same home they were launched with.

Use the same absolute `ATLAS_HOME` spelling for the daemon and harness. For long paths, Atlas uses `/tmp/agentatlas-<uid>` for its control and ingest sockets. This directory must be owned by the current user and have mode `0700`. Launcher and terminal processes can use different `TMPDIR` values.

## Optional OpenTelemetry export

In History, open a trace and use **Download OTLP JSON** to see the format and sanitized attributes. For continuous export, add the fields below to `config.json` and restart. Choose an endpoint you control. The exporter posts completed traces in commit order and treats redirects as errors.

```json
{
  "export_enabled": true,
  "export_preview_ack": true,
  "export_endpoint": "http://127.0.0.1:4318/v1/traces"
}
```

Atlas retries failed exports. If the receiver accepts a request and Atlas stops before persisting its export cursor, the receiver can see a duplicate. Configure deduplication at the receiver if required. Clearing local history does not remove traces previously delivered to a receiver.
