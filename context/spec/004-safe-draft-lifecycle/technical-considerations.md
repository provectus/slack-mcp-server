# Technical Specification: Safe Slack Draft Lifecycle

- **Functional Specification:** [functional-spec.md](./functional-spec.md)
- **Status:** Completed
- **Author(s):** Aleksandr Makarov

---

## 1. High-Level Technical Approach

The existing `conversations_draft_message` tool is extended in place — **zero new MCP tools**. The default behavior of the tool flips from "silently overwrite whatever draft sits at the destination" to a **compare-then-consent protocol driven by the calling agent**:

- On every call the handler lists drafts and looks for one at the target destination. If one exists and the caller has not asserted an overwrite, the handler **changes nothing** and returns the existing draft's content — both as a best-effort readable rendering and as normalised Slack block JSON.
- The **agent** (not the server) decides provenance by content matching: if the returned normalised JSON is byte-identical to what the agent last wrote (which every prior successful response handed back to it), the draft is the agent's own untouched work and it re-calls with the overwrite assertion, no user interaction needed. Any difference — or an unknown origin — requires the agent to show the user the readable text and get explicit consent before re-calling with the assertion.
- The server stays **stateless**: no draft registry, no on-disk provenance store. The agent's conversation is the memory of what it wrote; the server's job is to return content in a form that makes exact comparison possible and restoration lossless.

Affected systems: the draft handler (`pkg/handler/conversations.go`), the rich-text converters (`pkg/handler/draft_richtext.go` plus one new sibling file), the edge drafts client (`pkg/provider/edge/drafts.go`), tool registration/schema (`pkg/server/server.go`), and the live-probe test files under `pkg/provider/edge/`.

Two facts from the live investigation shape the design. First, the root cause of the historical upsert miss was **not reproduced** — every reachable mechanism (the `is_active` filter, `client_msg_id` mismatch, human edits, update races) was eliminated by live probes, and the miss never recurred at machine speed. The design therefore does not depend on knowing the cause; it is built to **fail safe** (see invariants in §2.8). Second, thread targeting **works today** (live-verified, including visual confirmation in the Slack UI), so §2.4 of the functional spec is met by preserving current behavior and adding regression coverage, not by a fix.

---

## 2. Proposed Solution & Implementation Plan (The "How")

### 2.1 Tool contract — parameters

`conversations_draft_message` keeps its existing parameters and gains two. No existing parameter changes meaning for current callers' inputs.

| Parameter | Type | Status | Semantics |
|---|---|---|---|
| `channel_id` | string | existing, required | unchanged |
| `thread_ts` | string | existing, optional | unchanged |
| `text` | string | existing, required | Draft body. For the new `application/json` content type it carries the verbatim block JSON (see §2.4). |
| `content_type` | string | existing, extended | `text/markdown` (default), `text/plain`, and new: **`application/json`** — verbatim Slack rich_text block JSON, used for lossless restore. |
| `overwrite` | boolean | **new**, default `false` | Assertion: "I have seen the draft currently at this destination (or at `draft_id`) and I am authorized to replace it." Without it, an existing draft is never modified. |
| `draft_id` | string | **new**, optional | Target a specific known draft instead of resolving by destination. See §2.5. |

**Why extend `content_type` rather than add a `blocks` parameter:** the three modes (markdown, plain, raw blocks) are mutually exclusive ways of interpreting one body; an enum on the existing parameter encodes that exclusivity structurally, adds zero new schema surface beyond one enum value and a description sentence, and avoids the ambiguity of a call that supplies both `text` and `blocks`. This matters doubly here because tool-schema size is the constraint that shaped the whole feature.

### 2.2 Result contract

The CSV `draftResult` row (`pkg/handler/conversations.go:304-309`) is replaced by a JSON result. CSV cannot carry nested block JSON; drafts move to a single JSON object marshaled into the text result (`mcp.NewToolResultText`). The struct (working name `draftActionResult`, in `pkg/handler/conversations.go`):

| Field | Type | Meaning |
|---|---|---|
| `action` | string | One of `created`, `replaced`, `existing_draft_found`, `conflict`. |
| `draft_id` | string | The id of the draft that now exists at the destination, **confirmed by re-list** (§2.6) — never an assumed id. |
| `channel_id`, `thread_ts` | string | Destination as resolved. |
| `draft` | object | The draft's current content: `{"text": <readable rendering>, "blocks_json": <normalised blocks>, "last_updated_client": <string>}`. Present on every action. |
| `displaced` | object | Same shape as `draft`; present only on `replaced` — the content that was overwritten, returned so it can be shown and restored (§2.3 of the functional spec). |
| `note` | string | Human/agent-readable statement of what happened and, for `existing_draft_found`/`conflict`, what the agent must do next (compare `blocks_json`; identical → re-call with `overwrite=true`; different → ask the user, showing `text`). |

`last_updated_client` is included as a **weak corroborating hint only** (live finding: this server reports as `"Chrome"` via the utls UA mimicry; the desktop app as `"Slack SSB Mac (Atom)"`). The `note` and the tool description must both state that provenance is decided by content comparison, never by this field — a user browsing Slack in Chrome also produces `"Chrome"`.

**`existing_draft_found` is a successful tool result, not a Go error.** The handler's current idiom of returning `error` (e.g. `conversations.go:383`) is wrong for this outcome: it is the expected common case of the protocol, and the caller — an LLM — must receive structured content (`blocks_json` to compare, `text` to show the user) and act on it. mcp-go renders Go errors as protocol-level tool errors, which clients may truncate or treat as failure, and which carry a string, not structure. Go errors remain reserved for genuine failures: disabled tool, denied channel, bad parameters, wrong credential type, transport/API errors.

### 2.3 Normalisation — making content comparable and restorable

Live finding: blocks do not round-trip byte-identically — Slack injects a server-generated `block_id` into each block and reorders JSON keys; after canonicalising key order, the injected `block_id` is the only difference. Normalisation removes exactly those two artifacts and nothing else.

New file **`pkg/handler/draft_normalize.go`** (sibling of `draft_richtext.go`), two functions:

- `normalizeDraftBlocks(raw json.RawMessage) (json.RawMessage, error)` — unmarshals the blocks array into generic `any` values, deletes every `block_id` key (recursively, for future-proofing; today Slack injects it only at the top block level), and re-marshals with `encoding/json`, whose map-key sorting yields a canonical key order. The result is deterministic: normalising the blocks the handler sent and normalising what `drafts.list` returns for the same draft produce byte-identical output. **Everything else is preserved verbatim** — element types this server's converter never emits (user/channel mentions, emoji elements, anything future Slack clients add) pass through untouched, which is what makes restore lossless for human-authored drafts.
- `renderDraftBlocksText(raw json.RawMessage) string` — best-effort readable rendering for the `text` field: decodes into `slack.Blocks` and walks rich_text elements into plain text with structural hints (newlines between sections, list markers, `>` for quotes, fenced code). On decode failure it degrades to a placeholder telling the reader to consult `blocks_json`. This is explicitly a **display form**: there is deliberately no rich_text→markdown converter (rejected as lossy), so `blocks_json` is the single authoritative representation and both the `note` field and the tool description say so.

Normalisation is applied to every `blocks_json` the tool returns (`draft` and `displaced` alike) and to `application/json` input before sending (so a caller pasting back a previously returned `blocks_json` sends exactly what comparison saw).

### 2.4 Restore path — `content_type: application/json`

When `content_type` is `application/json`, the handler:

1. Validates `text` parses as a JSON array of blocks in which every element has `"type": "rich_text"` (the drafts composer accepts only rich_text; anything else is rejected with an explicit error rather than silently dropped by Slack).
2. Runs `normalizeDraftBlocks` on it.
3. **Bypasses `markdownToRichTextBlock` and `draftContentLoss` entirely** — the input is already-valid Slack blocks, not markdown; the loss check is meaningless (and would false-positive) on content that was never markdown.
4. Proceeds through the same lookup/consent/write flow as the other content types. Restoring over the displacing draft is an overwrite like any other and requires `overwrite=true` (which the agent already holds consent for, having just been authorized to displace).

### 2.5 Handler flow and the `draft_id` parameter

`ConversationsDraftMessageHandler` flow becomes:

1. `parseParamsToolDraftMessage` — env gate, channel allow/deny policy, parameter validation. **This must complete successfully before any `DraftsList` call** (see §2.7).
2. Build the outgoing blocks per `content_type` (markdown/plain: existing converters and loss check unchanged; json: §2.4).
3. `DraftsList` (existing `draftsListLimit = 100`, `is_active=true` — live finding 1 confirmed this filter does not hide just-updated drafts; no change).
4. Resolve the target:
   - **No `draft_id`:** `findDraftForDestination` (unchanged: skips sent, deleted, and scheduled drafts; thread-scoped via `normalizeTS`).
   - **With `draft_id`:** locate that id in the listed drafts. Not found → explicit error (stale or foreign id; nothing is written). Found → its destination must match the requested `channel_id`/`thread_ts` (compared via `normalizeTS`), otherwise an explicit error: `draft_id` is a targeting refinement, not a retargeting mechanism, and the destination match guarantees the channel-policy check from step 1 covered the draft actually being touched.
5. **Scheduled-draft guard:** if the resolved draft has `DateScheduled != 0`, refuse with an explicit error — regardless of `overwrite`, regardless of how it was resolved. `findDraftForDestination` already skips scheduled drafts on the destination path; the `draft_id` path performs its own lookup and therefore needs this check explicitly so a known id cannot bypass the guard.
6. Act:
   - Existing draft found, `overwrite=false` → **write nothing**; return `action: existing_draft_found` with the draft's normalised content.
   - Existing draft found, `overwrite=true` → `DraftsUpdate` with the freshly listed `client_msg_id`/`last_updated_ts`; return `action: replaced` with `displaced` carrying the pre-update content (captured from the step-3 listing, normalised).
   - No draft found → `DraftsCreate`; return `action: created`. This branch runs even when `overwrite=true` — the assertion permits replacing, it does not require something to replace.
7. Confirm by re-list (§2.6) and marshal the result.

### 2.6 Truthful action reporting — confirmation by re-list

The current code returns `existing.ID` after `DraftsUpdate` — an id it assumed, never confirmed; `drafts.update`'s response is parsed only as `baseResponse` and the live probes did not establish that it echoes the draft. Rather than depend on an undocumented response shape, the handler confirms state the same way for both write paths: after a successful `DraftsCreate`/`DraftsUpdate`, it calls `DraftsList` once more and locates the resulting draft (by the id `drafts.create` returned, or the updated id). The confirmed listing supplies the `draft_id`, `last_updated_client`, and the as-stored `blocks_json` for the result — so what the agent memorises for future comparison is what Slack actually holds, not what we believe we sent. If the confirming re-list cannot find the draft, the handler reports that honestly as an error naming the write that succeeded and the confirmation that failed (never a fabricated success). Cost: one extra edge call per write, acceptable for a consent-gated, low-frequency tool. (As an implementation note, `DraftsUpdate` may additionally be extended to decode a `draft`/`draft_id` field from its response if one exists in practice, but the re-list is the contract; the response-shape shortcut is an optimisation only.)

### 2.7 Permission and credential gating

- **Ordering:** the policy check (`SLACK_MCP_DRAFT_MESSAGE_TOOL` enablement + `isDraftChannelAllowed`) already runs inside `parseParamsToolDraftMessage`, which the handler calls before any Slack traffic (`conversations.go:360` vs. `:401`). The read path this feature adds must not disturb that: no `DraftsList` — and therefore no draft text read or revealed — may happen before the parse step succeeds. This ordering becomes a tested invariant (§4), since the refusal response now carries draft content and a gating regression would leak private text.
- **Credential type:** registration already skips the tool for OAuth tokens (`server.go:238`, `!provider.IsOAuth()`), so `xoxp`/`xoxb` sessions never see it. For defense in depth (e.g. misconfigured multi-transport setups) the handler additionally returns an explicit error when the active token is not a session token: "drafts require a browser-session token (xoxc/xoxd); the configured token type cannot access Slack's drafts API" — satisfying functional §2.5's "explains rather than failing obscurely".

### 2.8 Failure handling and invariants

- **`draft_has_conflict` (live finding 4):** a stale `client_last_updated_ts` — i.e. the user edited the draft in Slack between our list and our update — makes `drafts.update` fail with `draft_has_conflict`; the draft survives intact. The edge client maps this to an exported sentinel (`edge.ErrDraftConflict`, matchable via `errors.Is`) so the handler can distinguish it from transport errors. The handler responds by re-listing and returning a **successful** result with `action: conflict` carrying the draft's *current* content — semantically "the draft changed underneath you; the consent protocol restarts from this content". Nothing was destroyed (Slack guaranteed that); the agent gets fresh state to compare/show.
- **Fail-safe invariant (root cause unknown):** because the historical upsert-miss mechanism was never identified, the design must remain correct if it recurs. Invariant: **a missed destination match results in creating a new draft — a harmless, visible duplicate — never in overwriting anything.** Structurally guaranteed because the only mutating calls are `drafts.create` (purely additive) and `drafts.update` guarded by three independent conditions: an `overwrite` assertion from the caller, a concurrency token (`client_last_updated_ts`) freshly obtained within the same handler invocation, and Slack's own server-side conflict rejection. `drafts.delete` is **not implemented** — it is not needed (restore is an update or create), and the functional spec forbids exposing deletion.
- Existing thread behavior is preserved untouched: `buildDraftDestinations` and the thread-scoped destination match are live-verified as correct; `DraftsUpdate` continues to re-send the same channel/thread destination, so a revision never migrates a threaded draft to the channel composer (functional §2.4).

### 2.9 Tool description — a deliverable, not an afterthought

The agent learns the compare-then-consent protocol **only** from the tool schema; no other channel exists. The rewritten `mcp.WithDescription` text in `pkg/server/server.go` must convey, in roughly this order:

1. Default is non-destructive: if a draft already exists at the destination, nothing is changed and its content is returned (`action: existing_draft_found`).
2. The protocol: compare the returned `blocks_json` against the `blocks_json` from your own last successful call for this destination. Byte-identical → it is your own untouched draft; re-call with `overwrite: true` without asking the user. Different, or you never wrote here → show the user the readable `text` and obtain explicit consent before re-calling with `overwrite: true`.
3. `blocks_json` is the authoritative content; `text` is display-only — **never** restore or compare from `text`. To restore displaced content exactly, re-call with `content_type: application/json` and the saved `blocks_json` as `text`.
4. `replaced` results include the displaced content for exactly that purpose.
5. Scheduled drafts are never replaced or deleted under any circumstances.
6. Requires a session token (xoxc/xoxd); drafts are saved, never sent.

The parameter descriptions for `overwrite` and `draft_id` restate their halves of the contract (assertion of seen-and-authorized; targeting-not-retargeting).

### 2.10 Edge client changes (`pkg/provider/edge/drafts.go`)

- `Draft` struct gains `Blocks json.RawMessage \`json:"blocks"\`` and `LastUpdatedClient string \`json:"last_updated_client"\`` (live finding 8: `drafts.list` already returns the full body; we currently just don't decode it). Other returned fields (`is_from_composer`, `date_created`, `file_ids`, …) stay undecoded until needed.
- `DraftsUpdate` error mapping for `draft_has_conflict` → `edge.ErrDraftConflict` (§2.8).
- No pagination change; `drafts.list` behavior confirmed correct by the probes.

### 2.11 Probe consolidation

The five ad-hoc probe files from the investigation — `drafts_live_probe_test.go`, `drafts_cause_probe_test.go`, `drafts_cleanup_probe_test.go`, `drafts_content_probe_test.go`, `drafts_afteredit_probe_test.go` in `pkg/provider/edge/` — are consolidated into **one** maintained, documented file (`drafts_probe_test.go`, build tag `liveprobe`, excluded from `make test` and CI as today). The consolidated file: keeps one probe per still-relevant behavior (upsert round-trip stability, threaded-destination round-trip, block normalisation ground truth, conflict-on-stale-token), removes the hardcoded draft ids left from the investigation (probes create and clean up their own drafts against a DM-to-self), keeps the shared helpers in the same file, and documents at the top what each probe establishes and the safety rules (session token required; never target a shared channel).

---

## 3. Impact and Risk Analysis

- **System Dependencies:** the undocumented `drafts.*` edge endpoints (session-token only); the `mark3labs/mcp-go` result model (structured success vs. protocol error, §2.2); the goldmark-based markdown converter for the unchanged write path; `slack.Blocks` unmarshalling for the readable rendering. No cache, transport, or rate-limiter changes beyond two extra tier-appropriate edge calls per draft write (the pre-write list already exists; the confirming re-list is new).

- **Risk: the unknown root cause is still out there.** The duplicate-draft miss was never reproduced; its mechanism is unidentified and may recur. *Mitigation:* the fail-safe invariant (§2.8) converts any recurrence into a visible, harmless duplicate instead of silent data loss; truthful `action` reporting means a recurrence is now observable ("created" when the agent expected "replaced") instead of concealed. This is a contained-severity risk, not an eliminated one, and the spec says so deliberately.

- **Risk: backward compatibility — the tool's default behavior changes.** Today's callers get silent replacement; after this change the same call returns `existing_draft_found` and writes nothing, and the response format moves from CSV to JSON. Any automation that parsed the CSV row or relied on unconditional overwrite will need the one-step adjustment of reading `action` and passing `overwrite: true`. *Mitigation:* the response is self-describing (`note` states exactly what to do next), the tool description teaches the new protocol to LLM callers with no code changes on their side, single-shot "create a draft where none exists" flows — the overwhelmingly common case — behave identically, and the release notes flag the behavior change prominently (fork release via `pv-v*` tag). The direction of the break is the safe one: the old behavior destroyed data, the new one withholds a write.

- **Risk: normalisation is load-bearing for consent.** If `normalizeDraftBlocks` ever produces different bytes for semantically identical content (a new Slack-injected field, a key-ordering edge), the agent sees a spurious mismatch and asks the user unnecessarily — annoying but safe. The failure mode is over-asking, never silent overwrite, because a mismatch can only tighten the gate. *Mitigation:* the normalisation ground-truth probe (§2.11) can be re-run against live Slack whenever a spurious-mismatch report arrives.

- **Risk: readable rendering mistaken for content.** An agent restoring from `text` would corrupt formatting. *Mitigation:* the description and `note` state that `blocks_json` is authoritative (§2.9); the restore path exists precisely so there is never a reason to round-trip through text.

---

## 4. Testing Strategy

Unit tests (testify, `make test`, no network):

- **Normalisation (`draft_normalize.go`) — table-driven:** for every formatting kind `markdownToRichTextBlock` can emit (bold, italic, bold+italic, inline code, fenced code blocks, ordered/unordered/nested lists, quotes, links, autolinks, headings), assert `normalize(sent blocks)` equals `normalize(sent blocks + injected block_id + shuffled key order)` — i.e. the two artifacts Slack adds are the only things removed. Separately: blocks containing element types our converter never emits (user mentions, channel mentions, emoji elements, unknown future types) pass through normalisation byte-preserved apart from `block_id`/key order — the property that makes human-draft restore lossless. Idempotence: `normalize(normalize(x)) == normalize(x)`.
- **Refusal path:** existing draft at destination, `overwrite` absent → result is a *success* with `action: existing_draft_found`, carries normalised `blocks_json` and readable `text`, and **no** `DraftsUpdate`/`DraftsCreate` call was made (mock records calls). Requires a narrow mock seam over the drafts operations the handler consumes (`DraftsList`/`DraftsCreate`/`DraftsUpdate`) — introduce the interface as part of this work.
- **Overwrite and restore paths:** `overwrite=true` with existing draft → update called with the freshly-listed concurrency token, result `replaced` with `displaced` populated; `application/json` input bypasses the markdown converter and `draftContentLoss` (a mock asserting the converter is never invoked), rejects non-rich_text block types, and round-trips a previously returned `blocks_json` unchanged.
- **Scheduled-draft guard:** destination lookup skips scheduled drafts (existing `findDraftForDestination` behavior, kept covered); `draft_id` pointing at a scheduled draft refuses even with `overwrite=true`; `draft_id` whose destination mismatches the requested channel/thread refuses.
- **Permission-gating order:** with the tool disabled or the channel denied, the handler returns the policy error and the mock provider proves `DraftsList` was never called — no draft text read or revealed.
- **Credential gating:** non-session token → the explicit "cannot access drafts" error.
- **`draft_has_conflict`:** edge client maps the API error string to `edge.ErrDraftConflict`; handler converts it into a successful `action: conflict` result with re-listed content, and never retries the write on its own.
- **Truthful reporting:** confirming re-list finds the draft → its id/content populate the result; re-list misses → honest error, no fabricated success.
- **Thread regression (functional §2.4):** table tests on `findDraftForDestination`/`normalizeTS` covering thread-scoped vs. channel-level matching and trailing-zero timestamp widths (extending existing coverage), plus destination preservation on the update path.

Live probes: the consolidated `drafts_probe_test.go` (§2.11) stays behind the `liveprobe` build tag, never runs in CI or `make test`, and covers the ground truths unit tests cannot: threaded-draft round-trip against a DM-to-self, block normalisation against real server responses, and the conflict error on a stale token.

---

## Change Log

- 2026-07-24 — §2.9's schema-size argument for extending `content_type` (rather than adding a `blocks` parameter) no longer holds: the tool description grew roughly fivefold and two parameters (`overwrite`, `draft_id`) were added. The structural-exclusivity argument — one content input, three interpretations, no ambiguity about which wins — still stands on its own.
