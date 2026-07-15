# Technical Specification: Service Binary Flip (Development ↔ Release)

- **Functional Specification:** [functional-spec.md](functional-spec.md)
- **Status:** Completed
- **Author(s):** Aleksandr Makarov

---

## 1. High-Level Technical Approach

Introduce a single, persistent, file-based **binary pin** — the symlink `~/.local/share/slack-mcp-server/current` — as the source of truth for which server binary the background service runs. `run-with-tokens.sh` (the launcher shared by launchd, systemd, and manual runs) resolves the pin uniformly, the installer creates it, and new Makefile targets repoint it. No per-process environment configuration is involved, so every invoker sees the same choice. Rationale and the alternatives considered (plist env var, pin file with a path) are in [design.md](design.md).

Affected systems: `run-with-tokens.sh`, `scripts/install.sh` (service setup), `Makefile`, shell test harness, README.

---

## 2. Proposed Solution & Implementation Plan (The "How")

### Binary resolution (`run-with-tokens.sh`)

Resolution order, first match wins:

| Priority | Source | Role |
|---|---|---|
| 1 | `$SLACK_MCP_BIN` env var | explicit per-invocation override (kept for compatibility) |
| 2 | `~/.local/share/slack-mcp-server/current` symlink | the persistent pin |
| 3 | `~/.local/bin/slack-mcp-server` | curl-installed release binary |
| 4 | `<repo>/build/slack-mcp-server` | repo build (existing build-if-missing fallback) |

A pin that exists but is not executable (broken symlink, deleted target) is a **hard error** with remediation text, mirroring the existing `SLACK_MCP_BIN` handling — a stale explicit choice must never silently fall through to a different binary.

### Installer (`scripts/install.sh`, `--with-service`)

- Service setup creates the pin: `ln -sfn <installed binary> <share-dir>/current`. This is correct for any `--prefix` (previously a custom prefix broke service resolution, which only looked in `~/.local/bin`).
- The rendered LaunchAgent plist and systemd user unit **no longer contain `SLACK_MCP_BIN`** — the env var outranks the pin, so baking it into service files would have made the service ignore flips.
- `slack-mcp-update` compatibility: the pin targets the *path* `~/.local/bin/slack-mcp-server`; the updater swaps that file in place, so release-mode pins survive updates with no updater changes.

### Makefile targets

| Target | Behavior |
|---|---|
| `service-local` | `make build` → repoint pin at `<repo>/build/slack-mcp-server` → restart → status |
| `service-release` | run `scripts/install.sh --with-updater` if the release binary is missing → repoint pin at `~/.local/bin/slack-mcp-server` → restart → status |
| `service-status` | print pin target (or "none"), service state, resolved binary `--version` |
| `service-restart` | internal; `launchctl kickstart -k` (Darwin) / `systemctl --user restart` (Linux), with setup instructions when no service is installed |
| `reinstall-service` | deprecated alias → `service-local` |

---

## 3. Impact and Risk Analysis

- **System Dependencies:** `run-with-tokens.sh` is copied to `~/.local/share/slack-mcp-server/` by the installer — the pin logic reaches installed machines only via a re-run of `install.sh --with-service` (or the next release's script). Existing installations keep working through resolution priorities 3–4 until then.
- **Existing installs with the old plist/unit:** service files rendered by older installers still carry `SLACK_MCP_BIN`, which outranks the pin; re-running `install.sh --with-service` re-renders them. Documented migration path.
- **Race on `make build`:** `build` runs `clean` first, so a `service-local` pin target briefly disappears mid-rebuild; KeepAlive could restart the service into the hard-error path during that window. Accepted: `service-local` restarts the service after the build completes.
- **Bootstrap race (observed on migration):** `launchctl bootstrap` immediately after `bootout` of a KeepAlive service can fail with error 5; the installer already treats this as a warning with manual recovery instructions.

## 4. Testing Strategy

- **Shell harness (`scripts/test/run.sh`, CI on ubuntu + macos):** `test_install_service.sh` gains an `assert_pin` helper; service-render cases assert the pin symlink exists and points at the installed binary, and that `SLACK_MCP_BIN` is absent from the rendered plist/unit; the whitespace-prefix case covers spaced `--prefix` values end-to-end (pin + `systemd-analyze --user verify`).
- **`run-with-tokens.sh`** has no automated harness (pre-existing gap); resolution verified manually by flipping both directions on a live macOS service and checking `service-status` plus an SSE `HTTP 200`.
- **Static:** `shellcheck` via scripts CI; Makefile targets smoke-tested by invocation.
