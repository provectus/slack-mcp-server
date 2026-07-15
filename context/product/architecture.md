# System Architecture Overview: Slack MCP Server

A self-hosted Model Context Protocol (MCP) server, written in Go, that gives any AI assistant secure, install-free access to Slack. It runs entirely on the user's machine and supports browser-session ("stealth") tokens so no bot install, extra scopes, or third-party data routing is required.

---

## 1. Application & Technology Stack

- **Language & Runtime:** Go 1.25.0 — compiled to a single static binary (module `github.com/korotovsky/slack-mcp-server`); version metadata stamped in at build time via ldflags and introspectable via the `--version` flag (`pkg/version.String()`, works with no tokens configured).
- **MCP Framework:** `github.com/mark3labs/mcp-go` v0.44.0 — provides the MCP server, tool/resource registration, and transport implementations.
- **Transports:** stdio (default, for local MCP clients), SSE (`server.NewSSEServer`, default host `127.0.0.1:13080`), and streamable HTTP (`server.NewStreamableHTTPServer`). Selected at runtime via the `-t/--transport` flag.
- **Tooling Layer:** A tool registry exposing read tools, opt-in write tools (off by default, env-gated), user-group management, and saved-items tools. Tools are capability-gated by the active Slack token type.

---

## 2. Data & Persistence

- **Data Store:** No database or ORM. Persistence is a set of on-disk JSON file caches — workspace users, channels, and per-channel member rosters — purpose-built to keep AI tool calls fast.
- **Isolation & Integrity:** Cache files live under `os.UserCacheDir()/slack-mcp-server/`, namespaced by Slack TeamID for multi-workspace isolation, and are written atomically via temp-file rename.
- **Cache Tiers:** Three tiers share the same snapshot/refresh machinery. Users and channels are each a single workspace-wide snapshot. The **channel-members** tier is a per-channel roster map (`channelID → {memberIDs, fetchedAt}`) held in one workspace file, populated lazily per channel on first request, with a per-channel in-flight guard so concurrent first-time requests don't duplicate the fetch. Member names and bot/deactivated status are resolved from the users snapshot at read time.
- **Freshness:** Configurable TTL (`SLACK_MCP_CACHE_TTL`, default 24h) with background refresh. Users and channels expire by cache-file age; the channel-members tier expires **per entry** by each roster's own `fetchedAt`, so channels refresh independently. Per-file overrides: `SLACK_MCP_USERS_CACHE`, `SLACK_MCP_CHANNELS_CACHE`, `SLACK_MCP_CHANNEL_MEMBERS_CACHE`; `--no-cache` bypass.

---

## 3. Infrastructure & Deployment

- **Distribution:** fork-native GitHub Releases on `provectus/slack-mcp-server`, triggered by `pv-v*` tags (`release.yaml`, ubuntu runner): six cross-compiled binaries (darwin/linux/windows × amd64/arm64) plus `checksums.txt` and `LICENSE`, with auto-generated release notes. The fork publishes no npm, DXT, or Docker artifacts — those remain upstream-only channels. A release is marked configuration-changing by `CONFIG-CHANGE: <note>` lines in its release-notes body.
- **Install & Update:** curl-able `scripts/install.sh` (agent-friendly: platform detection, sha256 verification against the release `checksums.txt`, install to `~/.local/bin`, `--version` pin, `--prefix`, `--with-updater`/`--no-updater` prompt pre-answers, `--with-service`) and `scripts/update.sh`, installed alongside the binary as `slack-mcp-update` (default check+apply, `--check` mode, atomic swap with `.bak` rollback, `CONFIG-CHANGE:` warnings scanned from release notes, machine-readable `INSTALLED=/LATEST=/RESULT=/CONFIG_CHANGES=` output, exit codes 0/10/1, service restart after update).
- **Local Service Modes:** always-on SSE service via `run-with-tokens.sh` (token file `~/.ssh/slack_tokens`; binary resolution `$SLACK_MCP_BIN` → `~/.local/share/slack-mcp-server/current` pin symlink (created by the installer, repointed by `make service-local`/`service-release`) → `~/.local/bin` → repo `build/`), run as a macOS LaunchAgent (RunAtLoad / KeepAlive; `com.slack-mcp-server.plist` stays a placeholder template for source builds, the installer renders its own) or a Linux systemd user unit rendered by the installer (`Restart=on-failure`, linger for start-at-boot).
- **CI/CD:** GitHub Actions — unit tests on push/PR, scripts CI (`shellcheck` on `scripts/*.sh` + `run-with-tokens.sh` and the plain-sh test harness `scripts/test/run.sh`, on ubuntu and macos runners), Trivy filesystem vulnerability scanning, Dependabot dependency updates, and the tag-triggered release workflow.

---

## 4. External Services & APIs

- **Slack API Clients:** `github.com/slack-go/slack` v0.19.0 for the standard Web API, plus a custom "edge" client (`pkg/provider/edge/`) for Slack's internal / Enterprise-Grid edge API, with fallback from edge to the standard client.
- **Authentication:** Four token modes — `xoxc`/`xoxd` (stealth, browser session), `xoxp` (user OAuth), and `xoxb` (bot) — supplied via environment variables, with tools capability-gated by token type. Browser-session extraction is handled by `github.com/rusq/slackauth` (driving a headless browser via go-rod + playwright-go) and `github.com/rusq/slackdump/v3`.
- **Stealth Transport & Access Control:** `github.com/refraction-networking/utls` provides TLS-fingerprint mimicry (injecting User-Agent and cookies over HTTP/2). The SSE/HTTP transports are protected by Bearer API-key auth (`SLACK_MCP_API_KEY`).

---

## 5. Observability & Reliability

- **Logging:** `go.uber.org/zap` structured logging, with TTY-aware output formatting.
- **Rate Limiting & Retry:** `golang.org/x/time/rate` tiered token-bucket limiters aligned to Slack API tiers, plus a generic retry layer that throttles and retries on HTTP 429 — keeping tool calls reliable under Slack rate limits.
- **Testing:** `github.com/stretchr/testify` for unit tests across the handler, provider, limiter, and text packages, run via `make test`.
