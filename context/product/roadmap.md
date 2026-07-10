# Product Roadmap: Slack MCP Server

_This roadmap outlines our strategic direction based on user needs and project goals. It focuses on the "what" and "why," not the technical "how."_

---

### Phase 1 — Core Slack Access & Connectivity (Foundation) ✅

_Complete. An LLM can securely read Slack — history, replies, search (with reaction filters), channels, users, and unreads — across four token modes (stealth, user OAuth, bot), three transports (stdio, SSE, HTTP), and every distribution format (Go binary, npm, Docker, DXT). Tool calls stay fast and reliable via per-workspace caching (TTL + background refresh) and tier-aware rate limiting with 429 retry._

---

### Phase 2 — Acting on Slack & Self-Hosting ✅

_Complete. Opt-in write tools (off by default) let the assistant post and draft messages with Markdown → native Slack rich_text formatting, add/remove reactions, mark as read, join/leave channels, and download attachments. Workspace management covers user groups and saved items ("Save for Later"). A macOS LaunchAgent runs the server as an always-on, self-hosted local service._

---

### Phase 3 — Broader Coverage & Smoother Onboarding 🔜

_What comes next: close coverage gaps and drive down time-to-first-tool-call._

- [ ] **Expanded Slack Surface**
  - [ ] **Group-DM (MPIM) Support:** Complete multi-person IM handling so the assistant can read and act in group DMs, not just channels and 1:1s. Currently scaffolded but incomplete.

- [ ] **Frictionless Onboarding**
  - [ ] **Guided Token Extraction & Setup:** Reduce the number of setup steps and the failure rate when supplying Slack tokens, directly targeting the time-to-first-tool-call and setup-success-rate metrics.
  - [ ] **Setup Diagnostics:** A preflight/health check that verifies the token and connectivity before the first tool call, so users catch misconfiguration early instead of on a failed call.
