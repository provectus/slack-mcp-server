# System Architecture Overview: Slack MCP Server

A self-hosted Model Context Protocol (MCP) server, written in Go, that gives any AI assistant secure, install-free access to Slack. It runs entirely on the user's machine and supports browser-session ("stealth") tokens so no bot install, extra scopes, or third-party data routing is required.

---

## 1. Application & Technology Stack

- **Language & Runtime:** Go 1.25.0 — compiled to a single static binary (module `github.com/korotovsky/slack-mcp-server`); version metadata stamped in at build time via ldflags.
- **MCP Framework:** `github.com/mark3labs/mcp-go` v0.44.0 — provides the MCP server, tool/resource registration, and transport implementations.
- **Transports:** stdio (default, for local MCP clients), SSE (`server.NewSSEServer`, default host `127.0.0.1:13080`), and streamable HTTP (`server.NewStreamableHTTPServer`). Selected at runtime via the `-t/--transport` flag.
- **Tooling Layer:** A tool registry exposing read tools, opt-in write tools (off by default, env-gated), user-group management, and saved-items tools. Tools are capability-gated by the active Slack token type.

---

## 2. Data & Persistence

- **Data Store:** No database or ORM. Persistence is an on-disk JSON file cache of workspace users and channels, purpose-built to keep AI tool calls fast.
- **Isolation & Integrity:** Cache files live under `os.UserCacheDir()/slack-mcp-server/`, namespaced by Slack TeamID for multi-workspace isolation, and are written atomically via temp-file rename.
- **Freshness:** Configurable TTL (`SLACK_MCP_CACHE_TTL`, default 24h) with background refresh; per-file overrides (`SLACK_MCP_USERS_CACHE`, `SLACK_MCP_CHANNELS_CACHE`) and a `--no-cache` bypass.

---

## 3. Infrastructure & Deployment

- **Distribution Formats:** Go binary (cross-compiled darwin/linux/windows × amd64/arm64), npm (platform-specific packages with a launcher selecting the binary via `optionalDependencies`), Docker (multi-stage `golang:1.25` build → `alpine:3.22` production, with a Delve-enabled dev stage), and a DXT desktop extension (`@anthropic-ai/dxt`, `manifest-dxt.json`).
- **Local Service Mode:** macOS LaunchAgent (`com.slack-mcp-server.plist` + `run-with-tokens.sh`) running the server as an always-on local service (RunAtLoad / KeepAlive), loading tokens from a local file.
- **CI/CD:** GitHub Actions — unit tests on push/PR, tagged releases (binaries + DXT + npm), multi-arch container image published to `ghcr.io`, Trivy filesystem vulnerability scanning, and Dependabot dependency updates.

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
