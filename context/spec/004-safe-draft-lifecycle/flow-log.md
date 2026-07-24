# Flow Log — 004-safe-draft-lifecycle

## Feature

**Safe Slack draft lifecycle — non-destructive drafts with correct thread targeting.**

Defect-driven, not a roadmap item. Source: investigation of Claude Code session transcripts where the assistant destroyed or duplicated the user's Slack drafts.

---

## 2026-07-23 — Investigation (pre-spec)

Evidence gathered from `~/.claude/projects/**/*.jsonl`. Three defects confirmed in `conversations_draft_message` (`pkg/handler/conversations.go:351-442`, `pkg/provider/edge/drafts.go`):

1. **Destroys hand-written drafts.** The upsert matches on destination only and cannot distinguish MCP-authored from human-authored drafts. Slack keeps no server-side draft history and the repo exposes no read path, so loss is unrecoverable and invisible. Evidence: session `52948daa-3087-428d-82fb-adccd7af453c` (citation), turn 742 — user: *"Remember - never ever create drafts if I've not asked! Is it possible to return draft which I wrote myself already?"*; turn 757 — assistant: *"my call overwrote it. Slack keeps no draft history server-side, and I have no read-draft or restore tool."*

2. **Create-vs-replace is nondeterministic and concealed.** The handler returns only `draft_id,channel_id,thread_ts`, never the action taken, so the model reports "replaced in place" after creating a duplicate. Draft-ID churn observed:

   | session | calls | draft ids | outcome |
   |---|---|---|---|
   | `f4499d3b` citation | 4 (one destination) | `Dr0BKY2QE4C8`, `Dr0BKY2QE4C8`, `Dr0BK993DCCA`, `Dr0BJU0X3G31` | replaced once, then missed twice |
   | `99f50204` awos | 2 (channel-level, C09GCR80NC8) | `Dr0BJ2724B8A`, `Dr0BJ08968ES` | missed |
   | `4f36e0fa` awos | 3 | all distinct | — |
   | `a4e2e975`, `6753b6be` awos | 2 each | stable | replaced correctly |

   Hypothesis to verify, **not** to assume: the upsert misses immediately after a `drafts.update`, suggesting `drafts.list` with `is_active=true` (`edge/drafts.go:135-166`) does not return a just-updated draft.

3. **Thread targeting unproven.** Session `f4499d3b` turn 185 — user: *"retarget this not to the channel but to thread to Anchel's post"*. Every call had already passed the correct `thread_ts` (`1784625417.164569`), so neither the user's instruction nor the model's parameters were at fault. `buildDraftDestinations` (`edge/drafts.go:24-30`) puts `thread_ts` in the destinations payload and nothing else; sufficiency is unproven. May also be fully explained by defect 2 (three live duplicates, one possibly channel-level).

No reverse converter exists: only `markdownToRichTextBlock` (`draft_richtext.go:41`). `flattenRichTextContent:380` strips to plain text and serves only the content-loss check. Markdown round-tripping is therefore lossy in both directions — the write path already refuses conversions it detects as lossy (`conversations.go:378-383`).

---

## 2026-07-23 — Specs stage

**Produced:** `context/spec/004-safe-draft-lifecycle/functional-spec.md` (Status: Draft).

**Branch:** `feat/draft-safety`, created from `fork/master`.

**Decisions taken (user-approved):**

- Clobber policy: **refuse** when the destination holds a draft the MCP cannot prove it wrote; return its full text; explicit override required to proceed.
- Pre-existing drafts (any that predate this change) are treated as **user-authored** — refuse. No grandfathering.
- Displaced text is returned **in the tool result only**. No on-disk backup — prevention, not archival, is the protection.
- Restore uses an **exact round trip** of Slack's own block JSON, not Markdown. Markdown round-tripping was rejected as lossy. Read returns both a readable rendering and the verbatim JSON; the write path accepts that JSON back unchanged.
- Thread targeting is **live-verified** against a DM-to-self, never a shared channel. Fix in this same PR if broken, with a regression test.
- Draft reading/replacing stays under the existing `SLACK_MCP_DRAFT_MESSAGE_TOOL` opt-in and its per-channel allow/deny policy.
- Scheduled drafts are never touched, even under an authorized overwrite.

**Scope reversal (user pushback, same session).** The spec was first written with a full draft lifecycle: a standalone list/read tool and a delete tool. The user rejected both:

> re `The user can ask what is in their drafts, and can ask the assistant to clear one out.` - I've not asked for this. Explain why you added it? New MCP tool eats tokens on the each session so need to be stingy with it.

Sustained on review. **Delete** was solving a symptom — orphan drafts exist only because the upsert is broken, so fixing the upsert removes the need. **Read** is load-bearing (§2.1 cannot show the user what it refuses to destroy without it) but does not need its own tool: the read happens inside the existing handler and its result is returned in the refusal. Final scope is **zero new MCP tools** — `conversations_draft_message` is extended in place. Former §2.3 (list/read) and §2.5 (delete) were cut; the spec renumbered to five requirement groups, 22 acceptance criteria.

**Next stage:** `/awos:tech` — technical spec, including the live thread-targeting check and verification of the `drafts.list`/`is_active` hypothesis.

---

## 2026-07-23 — Live investigation (during tech stage)

Five build-tagged probes (`liveprobe` tag, excluded from `make test`) run against real Slack, against the user's own DM-with-self (`D0A8P7UEQV8`) only — never a shared channel. All probe drafts were deleted afterwards; `drafts.list` confirmed 0 active drafts on cleanup.

### Findings — established fact

| # | Finding |
|---|---|
| 1 | `drafts.list?is_active=true` **does** return a just-updated draft, at +0s/+2s/+5s. `client_msg_id` preserved; only `last_updated_ts` advances. `is_active=false` comparison found **no** unsent drafts hidden by the filter. **Hypothesis (a) disproven.** |
| 2 | Five consecutive upsert rounds against one destination → **exactly 1 draft**. The existing upsert logic is correct at machine speed. |
| 3 | Updating with a foreign/mismatched `client_msg_id` does **not** fork the draft. Disproven as a cause. |
| 4 | A stale `client_last_updated_ts` returns `draft_has_conflict`; the draft survives. Errors rather than duplicating — not the cause, but a real race to handle. |
| 5 | A human editing the draft in the Slack desktop UI preserves draft id, `thread_ts`, destination and `client_msg_id`. Disproven as a cause. |
| 6 | `thread_ts` round-trips intact **and the user visually confirmed the draft renders as a genuine thread reply**. **Threading works today.** §2.4 is satisfied for newly-created drafts; the original mis-targeting report is attributed to the duplicate drafts, the first of which came from a different MCP server (`claude_ai_Slack`), outside this repo's control. |
| 7 | `drafts.list` returns the **full draft body** plus `is_from_composer`, `date_created`, `last_updated_client`, `team_id`, `user_id`. The `Draft` struct decodes only a subset today. |
| 8 | Blocks do **not** round-trip byte-identically: Slack injects a server-generated `block_id` per block and reorders keys. After canonicalising, the injected `block_id` is the **only** difference — nothing material is altered. `destinations` comes back enriched with `broadcast` and `user_ids`. |
| 9 | `last_updated_client` is `"Chrome"` for MCP-written drafts (a consequence of the utls/UA mimicry) and `"Slack SSB Mac (Atom)"` for the user's desktop app. **Weak signal only** — a user browsing Slack in Chrome produces `"Chrome"` too. Corroborating hint, never the provenance decision. |
| 10 | `drafts.delete` exists, requiring `draft_id` + `client_last_updated_ts`. Not exposed — the spec forbids a delete tool. |

### Root cause: UNRESOLVED

Every reachable mechanism was eliminated; the upsert miss does not reproduce. This is recorded as an open unknown, **not** as a solved problem. The design must not depend on knowing it.

### Design decision (user-proposed, adopted)

Provenance is decided by **content matching performed by the calling agent**, not by server-side state. The MCP server stays stateless; the agent holds the memory of what it wrote.

> "What if when We want to create a draft We first check which drafts are here and If there is something in place where I want to create a new draft then MCP tool just do nothing and Returns draft which already exists. Next agent Checks that this draft was created by it with binary matching And if at least one character doesn't match, then make a request to user for consent to overwrite it. If draft was created by some 3d party then ask consent as well. Skip consent only if agent is owerwriting its own draft."

This is strictly better than the server-side provenance ledger first considered: no local persistence, no restart-survival problem, and a user edit naturally reads as "not mine → ask". It requires the returned blocks to be canonicalised with `block_id` stripped (finding 8) so the comparison is meaningful.

**Fail-safe invariant** (forced by the unresolved root cause): a missed destination match must result in *creating* a new draft — a harmless duplicate — never in overwriting something.

---

## 2026-07-23 — Tech spec + tasks

**Produced:** `technical-considerations.md` (approved) and `tasks.md` — 10 slices, all delegated to `go-mcp-backend` except the release-notes task (`release-infra`) and the final regression slice (`testing-expert`).

**Design as specced:** `conversations_draft_message` gains `overwrite` (bool, default `false`) and `draft_id`; `content_type` gains `application/json` for verbatim restore. The CSV result becomes JSON carrying `action` (`created`/`replaced`/`existing_draft_found`/`conflict`), a re-list-confirmed `draft_id`, and `draft`/`displaced` objects each with readable `text` plus authoritative `blocks_json`. New `pkg/handler/draft_normalize.go` strips Slack's injected `block_id` and canonicalises key order so the agent's byte-comparison is meaningful.

**Decisions taken at this stage:**

- `existing_draft_found` and `conflict` are **successful** tool results, not Go errors. The caller is an LLM that must read structured content and act on it; mcp-go renders Go errors as opaque strings clients may truncate.
- `content_type: application/json` chosen over a separate `blocks` parameter — the modes are mutually exclusive, an enum encodes that structurally, and schema size is the constraint that shaped the whole feature.
- **Clean break to JSON**, user-approved. No CSV shim, no dual response shape. The tool is opt-in, off by default and fork-distributed; CSV cannot carry nested block JSON anyway. Flagged for release notes via the repo's `CONFIG-CHANGE:` convention.
- Every write is confirmed by a post-write re-list rather than trusting `drafts.update`'s unverified id echo — one extra edge call per write, accepted for a consent-gated tool.
- `draft_id` is targeting, not retargeting: its destination must match the requested channel/thread, which also keeps the channel-policy check covering the draft actually touched.

**Claims spot-checked against the code** (per the flow's rule that a subagent report is a claim, not a fact): `parseParamsToolDraftMessage` at `conversations.go:360` does precede `DraftsList` at `:401`; `server.go:238` does gate registration on `!provider.IsOAuth()`. Both hold, so the spec's gating-safety argument stands.

**Next stage:** `/awos:implement`.

---

## 2026-07-23 — Implementation

**Slice 1 (edge client reads draft body) — done.** `Draft` gains `Blocks` and `LastUpdatedClient`; `ErrDraftConflict` sentinel added, mapped inside `DraftsUpdate` only (via `errors.As` on `*APIError`), leaving `baseResponse.validate` and every other endpoint untouched. Two tests added.

Claims spot-checked: the conflict mapping is scoped to `DraftsUpdate` as reported (`drafts.go:258-265`), and `ErrDraftConflict` is a plain sentinel wrapped with `%w`. Both hold.

### Repo defect found: `make test` silently skips most tests

`Makefile:59` is `$(GO) test -count=1 -v -run=".*Unit.*" ./...` — only tests whose **name contains "Unit"** ever execute. This is also what the CI "Unit Tests" check runs. Pre-existing tests such as `TestBuildDraftDestinations` and `TestPadDraftTS` in `pkg/provider/edge/drafts_test.go` have therefore never gated anything, locally or in CI.

Consequence for this feature: every test added must be named `TestUnit*` or it is decorative. All slice agents are being told this explicitly.

This is a **pre-existing repo defect, not a defect in this feature**, and widening the filter would make an unknown number of currently-unrun tests start running — plausibly red — which is out of scope here. Recorded for the maintainer as a follow-up.

---

## 2026-07-24 — Verify + local review

**Verify:** all 22 acceptance criteria marked `[x]`; both specs `Status: Completed`. Evidence: 208 unit tests (0 fail), and four live probes green against real Slack (self-DM, self-cleaning). Coverage gap caught during verification — §2.4's "a revision stays in its thread" was captured by the mock but never asserted; added `TestUnitDraftLifecycleThreadedRevisionStaysThreaded` (red-validated) before marking complete. Product context needs no update (no new service/persistence/dependency).

**Local review** (independent `pr-review-toolkit:code-reviewer`, diff-only, fixed prompt) → `review.md`. Verdict: approve with changes, no blocker. 3 medium, 6 low, 3 nits. User accepted ALL findings. Applied across two focused agents (safety-critical first, then mechanical) after the first combined attempt timed out having written nothing:

- **M1/M2 (multi-destination)** — read-side matchers match on any destination; write side (`buildDraftDestinations`) always rebuilds a single-element array. So overwriting a multi-destination draft dropped the others (data loss, §2.3), and a draft targeting an allowed + a denied channel was reachable via the allowed one and its content returned (policy leak, §2.5). Fixed by refusing multi-destination drafts with a Go error placed before any content payload is built, on both resolution paths. Verified by reading the handler: guard at conversations.go:579, first `draftContentPayload` at :594.
- **M3** — re-list confirmed existence, not content; the note asserted "confirmed" regardless. Now diffs stored vs sent blocks and reports a mismatch honestly (no extra API call).
- **L4** — null/absent blocks turned the refusal into an un-actionable dead end; now treated as empty content so the agent can still review/consent.
- **L1/L2/L3/L5/L6, N1/N2/N3** — test seams moved from mutable package globals to handler struct fields; `draft_has_conflict` matched by `strings.Contains`; `content_type` gained `mcp.Enum`; list-window truncation now noted; renderer paragraph spacing; defensive `ThreadTS` source; spec change-log note that §2.9's schema-size argument no longer holds.

Post-fix: 208 tests (0 fail), `go vet ./...` and `go vet -tags liveprobe ./pkg/provider/edge/` clean, `make build` ok, live probes still green, no leftover test mutations.

**Next stage:** commit-push, then PR against provectus/slack-mcp-server.
