# Product Definition: Slack MCP Server

- **Version:** 1.0
- **Status:** Proposed

---

## 1. The Big Picture (The "Why")

### 1.1. Project Vision & Purpose

Give any AI assistant secure, self-hosted, install-free access to Slack — so an LLM can read, search, and act on your workspace without a bot install, extra scopes, or data ever leaving your machine.

The core problem: connecting an AI assistant to Slack normally requires installing and approving a Slack app, granting scopes, and (for hosted connectors) trusting a third party with your data. Many workspaces block app installs outright, and many users won't send their Slack data to a SaaS. This project removes that friction by running locally and using browser-session tokens ("stealth mode"), so the AI gets real Slack access while everything stays on the user's own machine.

### 1.2. Target Audience

Two overlapping groups:

- **Developers and power users who self-host their own infrastructure** — people comfortable running a Go binary, Docker container, npm package, or desktop extension locally, who want to wire Slack into an AI workflow on their own terms.
- **The open-source community** — contributors and users of a community-driven, MIT-licensed project with onboarding-friendly, staged setup documentation.

### 1.3. User Personas

- **Persona 1: "Dana the AI-tooling developer"**
  - **Role:** Developer wiring Slack into a Claude / Cursor workflow or an internal AI agent.
  - **Goal:** Give an LLM real, working access to Slack without begging IT for a bot app.
  - **Frustration:** Workspace admins block app installs; hosted connectors route data through a third party.

- **Persona 2: "Sam the self-hoster"**
  - **Role:** Power user running the server locally (e.g. via macOS LaunchAgent or Docker) for personal Slack automation.
  - **Goal:** Query, search, and triage their own Slack from an LLM.
  - **Frustration:** Wants everything on their own machine — no SaaS, no data leaving the local environment.

### 1.4. Success Metrics

- **Time-to-first-tool-call:** A new user goes from install to a working tool call in their MCP client quickly, with minimal setup steps.
- **Setup success rate:** A high percentage of users authenticate and connect successfully without needing to file an issue or ask for help.
- **Tool coverage and reliability:** Broad coverage of the Slack surface (read, write, user groups, saved items) with a low error / rate-limit failure rate on tool calls.

---

## 2. The Product Experience (The "What")

### 2.1. Core Features

- **Read tools** — conversation history and replies, message search (with reaction filters), channel listing, "my channels," user search, and unreads.
- **Write tools (opt-in, off by default, enabled per environment variable)** — post messages, draft messages, add/remove reactions, mark as read, join/leave channels, and fetch attachment data.
- **User-group management** — list, create, update, and manage membership of Slack user groups.
- **Saved items ("Save for Later")** — list, update, and clear completed saved items.
- **Flexible connectivity** — three transports (stdio, SSE, HTTP) and four token modes (`xoxc`/`xoxd` stealth, `xoxp` user OAuth, `xoxb` bot), with tools capability-gated by token type.
- **Performance caching** — per-workspace on-disk caching of users and channels with configurable TTL and background refresh, purpose-built to keep AI tool calls fast.

### 2.2. User Journey

A user installs the server (via Go binary, `npx`, Docker, or the DXT desktop extension), authenticates by supplying Slack tokens through environment variables (extracting browser `xoxc`/`xoxd` tokens for stealth mode, or providing OAuth tokens), and runs it with a chosen transport — `stdio` for a local MCP client, or `sse`/`http` for a networked server. They connect from any MCP client (Claude Desktop/Code, Cursor, MCP Inspector), which discovers the registered tools and resources, and the LLM then invokes those tools to read, search, post, and manage Slack. Write tools stay disabled until explicitly enabled.

---

## 3. Project Boundaries

### 3.1. What's In-Scope for this Version

- A self-hosted MCP server exposing Slack read tools, opt-in write tools, user-group management, and saved-items tools.
- Multiple auth/token modes (stealth `xoxc`/`xoxd`, user `xoxp`, bot `xoxb`) with capability-gating by token type.
- Multiple transports (stdio, SSE, HTTP) and multiple distribution formats (Go binary, npm, Docker, DXT desktop extension).
- Per-workspace caching of users and channels for fast tool calls.

### 3.2. What's Out-of-Scope (Non-Goals)

- **A hosted/managed SaaS offering** — the product stays self-hosted only.
- **Non-Slack chat platforms** — no Microsoft Teams, Discord, or others; Slack only.
- **A standalone chat UI or frontend** — this is a server for MCP clients; the client provides the user interface.
