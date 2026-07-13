# Flow Log: List Channel Members

Feature title: **List Channel Members** (List channel members and persist them in cache)

---

## specs — functional-spec

- Produced: `context/spec/001-channel-members/functional-spec.md` (Status: Draft → approved at gate).
- Decisions: member rows = user ID + display name + real name (resolved via users cache); channel input = ID or #name; return full roster (no pagination); optional filter to exclude bots and/or deactivated; per-workspace persisted cache with TTL + background refresh + explicit refresh.
- Branch: `feat/channel-members-cache` (from `fork/master`).
- Next: `/awos:tech` (technical-considerations.md).

## specs — technical-considerations

- Produced: `context/spec/001-channel-members/technical-considerations.md` (drafted by `go-mcp-backend`, claims spot-checked against api.go/channels.go/server.go/slack-go).
- Key decision: third cache tier — per-channel roster map in one workspace-scoped JSON file with per-entry TTL; `SlackAPI` interface gains `GetUsersInConversationContext` (edge+standard already implement it). No new tech → no hire.
- Approved at gate.
- Next: `/awos:tasks`.

## specs — tasks

- Produced: `context/spec/001-channel-members/tasks.md` (`<!-- not-user-reviewed -->`, no skip-tests). 3 slices: (1) provider fetch+cache, (2) `channels_members` tool, (3) Feature Testing & Regression.
- Agents: `go-mcp-backend` (impl+verify), `testing-expert` (QA). No general-purpose, no missing MCPs.
- Per parent flow, /awos:tasks has no gate.
- Next: commit specs, then `/awos:implement`.

## commit-specs

- Committed spec dir: `b64d9c8 docs(spec): add 001-channel-members functional and technical spec` on `feat/channel-members-cache`.

## implement

- All 3 slices / 10 tasks complete via `go-mcp-backend` (impl+verify) and `testing-expert` (Slice 3).
- Slice 1 (provider tier): `GetUsersInConversationContext` on SlackAPI+MCPSlackClient (edge/standard dispatch, standard paginates; edge-fork param `edgeslack.GetUsersInConversationParameters`); cache types `ChannelMemberRoster`/`ChannelMembersCache` (per-channel map, one workspace file, per-entry TTL); provider methods `RefreshChannelMembers`/`GetChannelMembers`/`fetchAndStoreChannelMembers`/`spawnBackgroundMembersRefresh`/`ForceRefreshChannelMembers`; boot wiring in `cmd/.../main.go` newChannelsWatcher; 429 via `limiter.CallWithRetry`. 9 provider tests, race-clean.
- Slice 2 (tool): `ToolChannelsMembers="channels_members"` registered (read-only, unconditional) in server.go; `ChannelsMembersHandler` + `ChannelMember` CSV struct in channels.go; `resolveChannelID` extracted to a shared pkg-level func (conversations handler delegates, no drift); README tool+env docs + feature-support row flipped to ✅. Handler tests + real stdio `tools/list` handshake confirmed registration.
- Slice 3 (QA): acceptance tests closing 3 handler gaps (unknown-channel not-found, not-ready sentinel, refresh_cache rate-limit-serves-cached) via a test-only exported seam (`testseam_channels_members_test.go`, `_test.go` only — no production surface); RED-validated gap #1 empirically. `@spec: 001-channel-members` + `@regression` annotated.
- Also fixed: stale `TestValidToolNames` count assertions (broke when channels_members was added to ValidToolNames).
- State: `make test` green, `go vet ./...` clean. Uncommitted: production code (api.go, channels.go, conversations.go, server.go, main.go, README.md) + test files.
- Next: `/awos:verify`, then local review.

## verify

- Verdict: PASS. All 18 acceptance criteria marked `[x]`; functional-spec + technical-considerations Status → Completed.
- Evidence: live stdio `tools/list` handshake (channels_members registered, params [channel_id(req), exclude_bots, exclude_deactivated, refresh_cache]) + per-AC integration/acceptance tests (handler + provider). Non-visual feature → no screenshots. Real-token live run not done (deployed launchd binary predates the tool; local token access blocked by classifier — not essential).
- No discrete roadmap checkbox to tick (free-text feature); README feature-support row already ✅.
- Product drift noted: architecture.md §2 omits the new third cache tier — suggest `/awos:architecture` update (not blocking).
- Next: local review (make test + reviewer subagent).

## local-review

- Static gate: `make test` green, `gofmt` clean.
- Reviewer: `pr-review-toolkit:code-reviewer` (flow named a bare `code-reviewer` which does not exist — patched, see below).
- Review file: `context/spec/001-channel-members/review.md`.
- Verdict: **Approve with changes**. Findings: 0 critical, 2 important, 3 informational.
- Manual keep/drop gate (AskUserQuestion): user chose to fix **both** important findings and evaluate the informational ones.
  - F1 (fixed): per-page rate-limit + retry — `fetchAndStoreChannelMembers` now drives the cursor loop wrapping each `conversations.members` page in `CallWithRetry` (a late-page 429 resumes from its cursor, not page 1); `MCPSlackClient.GetUsersInConversationContext` became a single-page pass-through. New test `TestChannelMembersIntermediatePage429Resumes`.
  - F2 (fixed): first-run gather now claims `refreshingMembers` via `LoadOrStore` — concurrent first callers get `ErrChannelMembersNotReady` (no duplicate fetch) and AC #18's "still preparing" path is reachable. New test `TestChannelMembersFirstRunConcurrentGuard`.
  - Informational (3): none applied — swallowed cache-write error is intentional resilience (matches users/channels); unsynchronized `edgeFailed` is a pre-existing cross-cutting pattern (out of scope); `IsReady` returning a Go error is codebase convention. Rationale recorded.
- Re-verified: race tests + `make test` green, gofmt clean.
- Flow defect fixed this run (self-improvement): reviewer agent id corrected to `pr-review-toolkit:code-reviewer` in `.claude/commands/implement-feature.md`; recorded in `delivery-flow.md` §10 Local Customizations.
- Next: commit + push `feat/channel-members-cache` to fork, open PR. (Flow-log frozen here — PR is about to open.)




