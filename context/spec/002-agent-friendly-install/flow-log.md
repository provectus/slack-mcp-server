# Flow Log: Agent-Friendly Install & Update

## 2026-07-14 — specs (functional)

- Feature: **Agent-Friendly Install & Update** — install/update the server from prebuilt release binaries without a Go toolchain, scriptable by agents (Claude Code); fork-native release channel; config-change warnings on update.
- Source: free-text description passed to /implement-feature.
- Branch: `feat/agent-friendly-install` (from `fork/master`).
- Produced: `context/spec/002-agent-friendly-install/functional-spec.md` — approved by user via gate.
- Decisions: macOS full support (binary + optional LaunchAgent setup); Linux binary-only via same script; Windows binaries published, no script. Update is a separate user-invoked script (server never self-updates), one command check+apply with check-only mode. Config-change warning driven by release marking. No npm/DXT/Docker publishing from fork.
- Next stage: specs (technical) — /awos:tech.

## 2026-07-14 — specs (technical)

- Produced: `technical-considerations.md` — approved after one revision round.
- Explored via Explore agent + release-infra specialist; key repo facts verified: no `--version` flag exists; `make release` pushes tags to `origin` (upstream) — bug to fix; release-image.yaml pushes ghcr.io/korotovsky — delete.
- User decisions: tags `pv-v*` starting pv-v1.0.0; `CONFIG-CHANGE:` release-notes marker; delete npm/DXT targets AND `npm/` dir + `manifest-dxt.json` (no upstream compatibility concern); Linux systemd user-unit service support added (functional spec amended, Change Log entry); installer prompts for updater install with `--with-updater`/`--no-updater`; URLs hardcoded (no override knobs), tests use curl PATH shim; required tests: mid-update rollback at each stage + e2e vs mocked GitHub API.
- Fact correction: user assumed repo private; verified PUBLIC via `gh repo view` — unauthenticated API OK, `$GITHUB_TOKEN` honored.
- Next stage: /awos:tasks.

## 2026-07-14 — tasks

- Produced: `tasks.md` — 7 slices (version flag → release pipeline → install.sh → update.sh → service setup → CI+docs → QA regression). Agents: go-mcp-backend, release-infra, testing-expert.
- Flow friction noted: inner /awos:tasks skill mandates its own review gate; delivery flow defines no tasks gate — user reconfirmed no-gate. Execution note for future runs: skip the skill's Step-5 review question, write tasks.md and continue.
- Next stage: commit specs, then /awos:implement.
