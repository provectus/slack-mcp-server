# 003 — Service binary flip (local dev ↔ release)

## Goal

Let a developer run the same background service (launchd/systemd), with the same config and credentials (`~/.ssh/slack_tokens`), while flipping between the released binary (`~/.local/bin/slack-mcp-server`) and a locally built debug binary (`<repo>/build/slack-mcp-server`). The pin must be file-based — no reliance on ambient environment variables, which differ per user and per invoking process.

## Design

### 1. Binary resolution in `run-with-tokens.sh`

New resolution order:

1. `$SLACK_MCP_BIN` — explicit per-invocation override (kept for compatibility).
2. `~/.local/share/slack-mcp-server/current` — a symlink, the persistent file-based pin. A pin that exists but is not executable (e.g. broken symlink) is an error, mirroring the `SLACK_MCP_BIN` handling — a stale pin must fail loudly, not silently fall back.
3. `~/.local/bin/slack-mcp-server` — curl-installed release binary.
4. `<repo>/build/slack-mcp-server` — repo build (with the existing build-if-missing fallback).

No symlink means today's behavior. The pin applies identically to every invoker: launchd, systemd, or a manual terminal run.

### 1b. Installer owns the pin (amendment)

`install.sh --with-service` previously rendered `SLACK_MCP_BIN=<installed binary>` into the plist / systemd unit environment. That env var outranks the pin, so an installer-managed service would ignore `make service-local`. Instead, service setup now:

- creates the pin symlink `~/.local/share/slack-mcp-server/current -> <installed binary>` (correct for any `--prefix`), and
- no longer renders `SLACK_MCP_BIN` into the service environment.

The service always resolves through the pin; the Makefile targets flip it. `SLACK_MCP_BIN` stays available as a manual per-invocation override.

### 2. Makefile targets (macOS + Linux)

- `service-local` — `make build`, point the `current` symlink at `<repo>/build/slack-mcp-server`, restart the service, print status.
- `service-release` — run the curl installer if `~/.local/bin/slack-mcp-server` is missing, point the symlink at it, restart, print status. `slack-mcp-update` keeps working: it swaps the binary at that path in place, so the pin survives updates.
- `service-status` — print the symlink target (or "no pin"), the service running state, and the resolved binary's `--version`.
- `reinstall-service` — deprecated alias for `service-local`.

Restart is `launchctl kickstart -k` on Darwin, `systemctl --user restart` on Linux.

### 3. Migration (developer machine, one-time)

Adopt the installer-managed service: run `scripts/install.sh --with-service` so the plist and run script are the installer-rendered copies (`~/.local/share/slack-mcp-server/`), replacing the repo-checkout plist. Then `make service-local` pins back to the repo build for development. `~/.ssh/slack_tokens` is read by both flows and is untouched.

### 4. Docs

README "Running as a background service" section documents the three targets and the symlink pin.

## Out of scope

- No test harness exists for `run-with-tokens.sh`; the resolution change is verified manually (flip both ways, check `service-status` and that the SSE port answers).
- Windows: no service support there today; unchanged.
