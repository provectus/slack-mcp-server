# Technical Specification: Draft & Message Formatting Fidelity

- **Functional Specification:** [`functional-spec.md`](./functional-spec.md)
- **Status:** Draft
- **Author(s):** `go-mcp-backend` specialist, orchestrated via `/implement-feature`

---

## 1. High-Level Technical Approach

Both defects live in one place: `markdownToRichTextBlock` (`pkg/handler/draft_richtext.go:41`), the markdown → `rich_text` converter shared by `conversations_draft_message` and `conversations_add_message` (`content_type: text/markdown`). Fixing it there makes both tools inherit the fix, and leaves the `text/plain` and verbatim `application/json` paths untouched.

The converter gains a stage on each side of goldmark:

1. **Preprocess the source** (new) — one line-oriented scan of the raw markdown that rewrites recognised unicode bullet markers to `- ` at line start, and lifts every Slack reference literal out into a side table, replacing it with an index placeholder. Both rewrites skip fenced and indented code.
2. **Parse** — goldmark, unchanged, over the preprocessed source.
3. **Split placeholders** (new) — every emitted section's element list is walked; text elements carrying placeholders are cut into text / `user` / `channel` / `usergroup` elements. `@here`/`@channel`/`@everyone` are deliberately re-emitted as an ordinary text run, never as a broadcast element.
4. **Validate** (new, in the handler) — every live mention is checked against the workspace before any drafts API call; the first unresolvable reference refuses the whole write.
5. **Readback** — `renderDraftBlocksText` gains a name resolver so mentions display as `@dana` / `#general` / `@eng`.

Rewriting bullets *before* the parse is what makes goldmark emit a genuine `rich_text_list` through its own well-tested list machinery. Lifting references out *before* the parse is what stops goldmark from fragmenting, linkifying, or swallowing them.

---

## 2. Proposed Solution & Implementation Plan (The "How")

### 2.1 Component Breakdown

#### New files

**`pkg/handler/draft_source.go`** — pre-parse source rewriting; the only place that mutates markdown before goldmark sees it.

| Contract | Responsibility |
| --- | --- |
| `type draftSource struct { Text string; Refs []mentionRef }` | The document goldmark parses, plus the index-addressed reference table in source order |
| `preprocessDraftSource(markdown string) (draftSource, error)` | Bullet normalisation + reference lifting; errors on a `U+E000` collision |
| `(draftSource) expandPlaceholders(text string) string` | Restores original literals — used by code spans and preformatted blocks, which must never see a converted mention |
| `(draftSource) splitPlaceholders(text string) []sourceSpan` | Cuts a text run into alternating literal / reference spans |

**`pkg/handler/draft_mentions.go`** — the reference grammar and the element↔literal correspondence. Pure, no I/O.

| Contract | Responsibility |
| --- | --- |
| `type mentionKind` (`mentionUser`, `mentionChannel`, `mentionUserGroup`, `mentionBroadcast`) | Reference classes |
| `type mentionRef struct { Kind; ID, Range, Label, Literal string }` | One parsed reference |
| `(mentionRef) canonicalLiteral() string` | Label-free textual form (`<@U123>`, `<#C123>`, `<!subteam^S123>`, `@here`) — the shared vocabulary for the loss check and readback fallback |
| `(mentionRef) element(...) slack.RichTextSectionElement` | The rich_text element it becomes; broadcasts return a plain text element reading `@here` — inert by construction |
| `splitSectionElements(src, elems) []slack.RichTextSectionElement` | Expands placeholders in one section's element list |
| `collectMentionRefs(rtb) []mentionRef` | Every live mention in a built block, deduplicated, document order |
| `canonicalizeReferences(s string) string` | Rewrites reference literals to canonical form; used by `draftContentLoss` on the `want` side |

**`pkg/handler/mention_directory.go`** — the only component that talks to Slack for mentions. One instance per tool call, memoising the user-group list so validation and readback share a single fetch.

| Contract | Responsibility |
| --- | --- |
| `errRefNotFound`, `errRefUnverifiable` | Sentinels backing the two-way error split required by AC 2.4 |
| `mentionVerifier` / `mentionNamer` interfaces | Split so tests can stub verification and naming independently |
| `mentionDirectory` + `newMentionDirectory(ap, log)` | Cache-first resolution with live fallback; memoises positive **and** negative results per call |
| `validateMentions(ctx, rtb, v) error` | Verifies every live mention, returns the first failure wrapped with the offending canonical literal |

#### Modified files

**`pkg/handler/draft_richtext.go`**

- `markdownToRichTextBlock` — signature **unchanged**, preserving the `markdownToRichTextBlockFn` test seam and the `conversations_add_message` call site. Stays pure: no `ctx`, no provider, no network.
- `codeBlockText` / `inlineNodeText` — take the `draftSource` so they expand placeholders back to literals. Mandatory: without it a `` `<@U123>` `` code span leaks a private-use rune into the draft.
- `flattenRichTextContent` — gains cases for the four mention element types, each writing `canonicalLiteral()`.
- `draftContentLoss` — computes `want` from `canonicalizeReferences(input)`.

**`pkg/handler/draft_normalize.go`**

- `renderDraftBlocksText` takes a `ctx` and a `mentionNamer`, threaded into `renderRichTextElement` / `renderRichTextSectionElements`; the four mention cases emit the resolved name, falling back to `canonicalLiteral()`.
- `normalizeDraftBlocks` and `parseVerbatimDraftBlocks` are **untouched** — the authoritative blocks path stays byte-exact.
- The existing `DISPLAY ONLY` comment is extended: mention names are resolved live and are therefore **not stable across calls**, which makes the readback doubly unsuitable as a comparison source.

**`pkg/handler/conversations.go`**

- `ConversationsHandler` gains a `mentions` field (interface satisfying both `mentionVerifier` and `mentionNamer`; nil ⇒ build from `apiProvider`) plus a `mentionDir()` accessor, matching the existing seam idiom (`drafts`, `isOAuthTokenFn`).
- `draftContentPayload` becomes a method taking `ctx`, so readback can name mentions.
- `ConversationsDraftMessageHandler` — in the `text/markdown` branch, after `checkDraftContentLoss`, call `validateMentions` and return on error.
- `ConversationsAddMessageHandler` — the existing "conversion error ⇒ fall back to plain text" behaviour is preserved for *conversion* errors; a `validateMentions` failure is a **hard return**, not a fallback.

**`pkg/provider/api.go`**

- New exported sentinel `ErrUserNotFound` (mirroring the existing `ErrRefreshRateLimited` idiom), returned by `PatchUser` when `GetUsersInfo` yields an empty result or Slack answers `users_not_found` / `user_not_found`. Today those are indistinguishable from a network failure, and AC 2.4's "does not exist" vs "could not check" split depends on telling them apart.

### 2.2 Logic — bullet-marker normalisation

**Chosen: a pre-parse source rewrite. Rejected: a goldmark parser extension.**

A `parser.BlockParser` registered against `•◦‣⁃·` would have to reimplement goldmark's `listParser` + `listItemParser` pair, which together own continuation lines, lazy continuation, loose/tight detection, nested-indent arithmetic, and blockquote interaction. goldmark exposes no "treat this rune exactly as `-`" hook — the marker set is hard-coded in `parseListItem`. That means owning a fork of goldmark's most subtle block parser to avoid changing one character per line. The rewrite reaches the same output through goldmark's own machinery and is trivially testable as a string→string function.

The rewrite is a line scan, not a document-wide regex:

1. **Fence state** — a line whose trimmed form opens/closes a ``` ``` ``` or `~~~` fence toggles state (matching fence char and run length). Inside a fence: emit verbatim.
2. **Indented-code state** — ≥4 leading spaces (or a tab), not a list-item continuation, preceded by a blank or indented line. Inside: emit verbatim.
3. **Otherwise** — match `^([ \t]{0,3})([•◦‣⁃·])([ \t]+)(.*)$` and rewrite group 2 to `-`, preserving groups 1, 3, 4 byte-for-byte.

Traps, addressed:

- **Mid-sentence bullets.** The pattern is `^`-anchored with at most three leading spaces (CommonMark's marker-indent budget). `Options: • a • b` and `x·y` are untouched. This is the whole defence and it suffices.
- **No space after the marker.** `[ \t]+` is required, so `•a` stays verbatim — rewriting it would silently change the user's visible text to `-a`.
- **`draftContentLoss` balance.** `tokenizeWords` splits on `!IsLetter && !IsNumber`, and all five markers (`•◦‣` Po, `⁃` Pd, `·` Po) are separators, never tokens. The marker contributes zero words on both sides, so **no loss-check change is needed for bullets**. Verified locally; worth a regression test per marker.
- **The "no empty line before the list" criterion follows automatically.** Verified: `"Intro:\n\n- a\n- b\n"` already produces `[section("Intro:"), rich_text_list]` with **no** `"\n\n"` text run — `flushInline` emits the pending section and the list becomes its own top-level element. The stray blank line is purely an artifact of the `•` form, where the source blank line survives as a literal `"\n\n"` run inside one section. Turning the lines into a real list consumes it. Whether Slack's *composer* inserts vertical space between a section and a following list is a live-render question — see §4.

> **Finding that narrows scope: `1)` already works.** CommonMark defines an ordered marker as digits followed by `.` **or** `)`, and goldmark implements it. Verified: `"Intro:\n1) a\n2) b\n"` already yields `style: "ordered"` with items `a`, `b` and an empty loss result. **No rewrite of `1)` will be implemented** — rewriting it to `1.` would strip the `1` from the visible text on the `got` side while the `want` side still tokenised it, manufacturing a false "content lost" refusal. The functional spec's `1)` criterion is satisfied today and becomes a **regression test**.

### 2.3 Logic — mention parsing

**The scan happens pre-parse over the raw source; the split completes post-parse. Not a post-pass over assembled elements.**

Post-processing assembled text elements is the intuitive design and it is wrong. Observed behaviour of goldmark on `<…>` forms:

| Input | goldmark output |
| --- | --- |
| `<!here>` | two text elements: `"cc … <"` + `"!here> "` |
| `<@U03MT3U0F6E>` | split position **varies with context** — whole in a bare paragraph, split inside a list item |
| `<!subteam^S0123ABC\|@eng>` | a **link element** (`url == text == "!subteam^S0123ABC\|@eng"`) — the autolink parser claims it, because the `@` reads as an email |

The last case is not a quirk to patch around: a text-element post-pass misses it entirely and the draft ships a bogus link. And AC 2.3 requires `@here` to be "never mangled, split across lines, or partly swallowed" — the only way to guarantee that is to ensure goldmark never sees the `<`.

So references are lifted out before parsing and replaced with an index placeholder `U+E000 <decimal> U+E000`. A placeholder is a private-use rune plus ASCII digits — no character in it is inline-special to CommonMark, so goldmark treats it as an ordinary indivisible text run in every position: paragraph, heading, list item, blockquote, emphasis span, link label.

Obligations that follow:

- **Collision guard.** `preprocessDraftSource` errors if the input already contains `U+E000`, and the handler surfaces it as a refusal. Restoring a user-typed `U+E000` as part of a placeholder would be a silent content change; refusing is honest, and real Slack messages effectively never contain it.
- **Code spans and code blocks expand, never convert.** The line scanner skips fenced and indented code so no placeholder is created there. Inline backtick spans are invisible to a line scan, so `` `<@U123>` `` does get one — `inlineNodeText` must expand, and `splitSectionElements` must expand rather than convert when a run carries `Style.Code`.
- **No element merging anywhere.** The `got`-side tokenisation is unchanged for all non-mention text, so this design introduces no new class of `draftContentLoss` false positive.

**Reference grammar** — one alternation, applied to raw source and to `canonicalizeReferences`:

```
<@([UW][A-Z0-9]{2,})(?:\|([^>]*))?>
<\#([CGD][A-Z0-9]{2,})(?:\|([^>]*))?>
<!subteam\^(S[A-Z0-9]{2,})(?:\|([^>]*))?>
<!(here|channel|everyone)(?:\|([^>]*))?>
```

The user-ID class mirrors `provider.slackUserIDPattern` so the two never disagree about what an ID looks like.

| Source form | Becomes | Validated against | Readback |
| --- | --- | --- | --- |
| `<@U123>` / `<@W123>` | `RichTextSectionUserElement` | users cache → `PatchUser` | `@dana` |
| `<@U123\|dana>` | same; label **dropped** | same | `@dana` (current name, not the stale label) |
| `<#C123>` | `RichTextSectionChannelElement` | channels cache → `conversations.info` | `#general` |
| `<#C123\|general>` | same; label dropped | same | `#general` |
| `<!subteam^S123>` | `RichTextSectionUserGroupElement` | live `usergroups.list` | `@eng` |
| `<!subteam^S123\|@eng>` | same; label dropped | same | `@eng` |
| `<!here>`, `<!channel>`, `<!everyone>` | **plain text element** reading `@here` / `@channel` / `@everyone` | not validated | same |
| anything else in `<…>` | untouched — goldmark's existing behaviour | — | — |

Dropping `|label` is not a loss: the `user`/`channel`/`usergroup` elements carry **only** an ID (confirmed against slack-go v0.19.0), and Slack renders the current display name client-side. This is also why the functional spec's "showing that person's display name" is satisfied by Slack rather than by us — we must not, and cannot, embed a name.

**Assumption (beyond the functional spec): broadcasts render as `@here`, not `<!here>`.** Emitting the source literal would be inert but shows the user the characters `<!here>`, failing AC 2.3's "visible and readable". A text run reading `@here` is inert (Slack does not re-parse text inside a `rich_text` text element into a broadcast), reads correctly, and converges with what happens when the assistant simply types `@here` as prose. It is token-neutral for the loss check. `RichTextSectionBroadcastElement` is therefore **never constructed by the converter** — it appears only on the readback path, for human-authored drafts.

**`draftContentLoss` balance for mentions** — two symmetric halves, both required:

- `want`: canonicalise references in the input first, stripping `|label` (so `<@U123|dana>` stops contributing a `dana` token the block can never produce).
- `got`: `flattenRichTextContent` emits `canonicalLiteral()` for each mention element type.

Without both halves this is the single most likely source of spurious "conversion dropped content" refusals.

### 2.4 Validation & error contract

**Where it lives.** `validateMentions` is called from the handlers, not from `markdownToRichTextBlock`. Keeping the converter pure preserves the `markdownToRichTextBlockFn` seam, keeps the `conversations_add_message` call site compiling unchanged, and keeps every conversion unit test network-free.

**Ordering — this is the AC 2.4 guarantee.** In the draft handler:

```
parse params → OAuth guard → convert markdown → draftContentLoss → validateMentions → marshal blocks → DraftsList → …
```

`validateMentions` sits in the `text/markdown` branch, above the `DraftsList` call. When a refusal returns, nothing — not `drafts.list`, not `drafts.create`, not `drafts.update` — has run. An existing draft at the destination is not read, not listed, and categorically not modified. This ordering is a contract, not an accident: it carries a comment saying so and an acceptance test asserting the fake drafts API recorded **zero** calls.

The loss check runs before validation deliberately — it is local and free, validation may hit the network, and a conversion bug should not cost API calls.

| Class | Resolution path | "Does not exist" | "Could not verify" |
| --- | --- | --- | --- |
| User `U…`/`W…` | users cache → `PatchUser` | `provider.ErrUserNotFound` | any other error — 429 after retries, transport, `invalid_auth` |
| Channel `C…`/`G…`/`D…` | channels cache → `GetConversationInfoContext` | `channel_not_found` | anything else. **`missing_scope` / `not_in_channel` are could-not-verify** — a private channel the token cannot see is not proof of absence |
| User group `S…` | live `GetUserGroupsContext(IncludeDisabled(true), IncludeUsers(false), IncludeCount(false))`, memoised per call | list fetched and no entry matches | the list call failed — `missing_scope`, `paid_only`, `not_allowed_token_type`, 429 |
| Broadcast | — | never validated | — |

On the user-group path specifically:

- `usergroups.list` is the only enumerating read slack-go exposes; there is no `usergroups.info`. `GetUserGroupMembersContext` would give a cheaper single-ID probe but returns no name, and the readback needs the handle. One memoised list call serves validation *and* naming. Zero calls are made when a message references no group — satisfying "live only when actually referenced" and "no new persistence".
- **Capability-gated in practice.** The draft tool is registered only for `xoxc`/`xoxd` (`pkg/server/server.go:238`), where the browser session carries full user scopes. `xoxp` needs `usergroups:read`; `xoxb` likewise, and returns `missing_scope` otherwise — which only affects `conversations_add_message`. That failure is a clean "could not verify" and the error text names the missing scope. No `IsOAuth`/`IsBotToken` pre-gate is added: attempting and reporting beats refusing preemptively, since many bot installs do carry the scope.
- All three lookups go through `limiter.CallWithRetry` at the appropriate tier. `PatchUser` currently does not — a pre-existing gap this work exposes on the write path.

**Error messages** — distinct per branch, each naming the offending reference by canonical literal, each stating that nothing was written:

```
reference <@U0BOGUS1> does not exist in this workspace (no such user);
nothing was written — correct the reference and retry.

reference <!subteam^S0123ABC> could not be verified — the workspace could not be
consulted (usergroups.list failed: …). This is "could not check", not "does not
exist"; nothing was written. Retry, or remove the reference.
```

The split matters: AC 2.4 requires the assistant to distinguish a self-correctable mistake (fix the ID) from a transient environment problem (retry).

**Assumption (beyond the functional spec): the verbatim paths are exempt from validation.** `content_type: application/json` exists to restore a draft byte-for-byte, and its blocks came from Slack itself — a human-authored draft mentioning a since-deactivated colleague would become permanently unrestorable if validated, breaking the documented `displaced.blocks_json` restore promise. The same reasoning exempts `conversations_add_message`'s raw `blocks` parameter, and `text/plain`, where literal means literal.

---

## 3. Impact and Risk Analysis

### System dependencies

goldmark v1.7.16 (behaviour relied upon, not extended), slack-go v0.19.0 (all four `RichTextSection*` element types confirmed present), the `ApiProvider` users and channels caches, `GetUserGroupsContext`, `GetConversationInfoContext`, and the `limiter` package.

### Risks & mitigations

**`draftContentLoss` false positives — highest-likelihood regression.** A false positive is a hard refusal to create a draft, which is worse than the formatting bug being fixed.

- Labelled references contributing an unreachable token → canonicalise on the `want` side.
- Mention elements contributing nothing on the `got` side → extend `flattenRichTextContent`.
- Placeholder digits leaking into either side → structurally impossible (the `want` side canonicalises the *original* source, and every `got`-side path converts or expands), but worth an explicit assertion that no output string anywhere contains `U+E000`.
- The bullet rewrite is provably token-neutral — verified.

**The verbatim path must stay protected.** It never touches the converter, and the exemption above keeps validation off it. The one place it *is* affected is the readback, which is display-only and explicitly permitted by AC 2.5. The risk is a later "improvement" turning the readback into a restore source; mitigated by extending the `DISPLAY ONLY` comment to note that resolved names are not stable across calls. The provenance comparison uses `blocks_json`, never `text`, and must stay that way.

**Bot / OAuth token modes.** The draft path is unchanged for `xoxp`/`xoxb` — the tool is not registered for them and the handler re-checks. `conversations_add_message` *is* available to them, so the mention work must be token-agnostic. Users and channels caches work in all four modes; user groups do not, so a bot without `usergroups:read` now gets a hard refusal on a message it would previously have posted with a raw code in it. Mitigation: the error names the scope. No silent degrade-to-plain-text — that reintroduces exactly the defect this spec exists to fix.

**Cache-miss storm.** A message mentioning many users just after a cold start could fire one `users.info` per unknown ID. Mitigated by deduplicating refs by ID before verification, memoising positive **and** negative results for the tool call's duration, the normally-warm users snapshot, and routing lookups through `limiter.CallWithRetry` so a burst throttles rather than earning a 429 on the write path.

**Behaviour changes existing `conversations_add_message` callers will notice** — all three belong in the release notes:

1. A message containing an unresolvable reference **now fails** where it previously posted with the literal code visible. Intended (AC 2.2/2.4 extend to the post path), but breaking for any caller passing unverified IDs.
2. Valid references now post as **live mentions, which notify people**. That is the point of the feature, but it is a real change in blast radius and belongs next to the `@here` guarantee.
3. Lines starting with `•` now become real lists, changing the block structure of posted messages.

The `MsgOptionText` fallback still carries the raw markdown including raw `<@U…>` codes; Slack resolves those server-side for the notification, so no change is needed there.

**goldmark version sensitivity.** The design's dependence is now confined to "a placeholder of `U+E000` plus digits is not inline-special" — far more stable than depending on how goldmark fragments `<!subteam^…|…>`. Cheap insurance: assert a placeholder survives a round-trip intact in paragraph, heading, list-item, blockquote, emphasis-span and link-label positions.

---

## 4. Testing Strategy

**Unit — `pkg/handler/draft_source_test.go` (new).** `preprocessDraftSource` as a string→string table, which is where the fence state machine gets pinned: each of the five markers; `1)` left alone (regression against re-adding a rewrite); `•a` with no space left alone; mid-sentence `·` left alone; markers inside ``` ``` ```, `~~~` and 4-space-indented blocks left alone; an unclosed fence; a fence inside a list item; three leading spaces rewritten, four not; the `U+E000` collision guard returning an error.

**Unit — `pkg/handler/draft_mentions_test.go` (new).** All nine grammar forms plus near-misses (`<@lowercase>`, `<@U>`, `<!subteam^X123>`, unterminated `<#C123`); `canonicalLiteral` round-trips; `splitSectionElements` leaving surrounding punctuation and spacing byte-identical; and the enforcement point for §2.3 — `@here`/`@channel`/`@everyone` produce a text element and **never** a `RichTextSectionBroadcastElement`.

**Unit — extend `draft_richtext_test.go`.** End-to-end converter cases: the `•` list yielding `[section, list, section]` with **no** `"\n\n"` run between intro and list; a list item carrying bold/italic/code/link *and* a mention; mentions in a blockquote and a heading; a mention inside a backtick code span expanding back to the literal and **not** becoming an element; a fenced block containing `<@U123>` surviving verbatim; a global assertion that no emitted string contains `U+E000`. Plus a dedicated `draftContentLoss` table — empty result for every bullet marker and every reference form including labelled ones. This is the false-positive tripwire. The existing `flattenRichText` helper needs the four new element cases or it will silently under-report.

**Unit — extend `draft_normalize_test.go`.** Readback against a stub namer: user/channel/usergroup rendering as `@dana` / `#general` / `@eng`; an unknown name falling back to the canonical literal (AC 2.5 — never silently omitted); an existing `RichTextSectionBroadcastElement` in a human-authored draft still rendering `@here`; and an assertion that `normalizeDraftBlocks` output is byte-identical before and after this change, since the lossless-restore contract must not move.

**Unit — extend `conversations_addmessage_test.go`.** It currently exercises the converter directly rather than the handler. Add handler-level coverage for the two behaviour changes: a valid mention posting a block with a `user` element, and an unresolvable mention returning an error with `PostMessageContext` never called.

**Unit — extend `conversations_draft_test.go`**, using the existing seams (`drafts`, `isOAuthTokenFn`, `markdownToRichTextBlockFn`, `draftContentLossFn`) plus the new `mentions` field:

- **The AC 2.4 keystone test** — unresolvable reference ⇒ error naming the reference, and the fake drafts API recorded **zero** `DraftsList`/`DraftsCreate`/`DraftsUpdate` calls. Run twice: once with no pre-existing draft, once with the fake pre-loaded with one at the destination, asserting it is untouched.
- Not-found vs could-not-verify producing textually distinct errors.
- `application/json` with a mention-bearing block: `Verify` never called, blocks pass through byte-identical.
- `text/plain` containing `<@U123>`: no element, no verification, literal preserved.
- A message with zero references makes zero provider calls.

**Acceptance — extend `conversations_draft_acceptance_test.go`.** One test per functional-spec criterion, tagged to it: bullets→list, mentions→elements, `@here` inert, refusal leaves the destination untouched, readback names. This file is what `/awos:verify` reads, so criterion-to-test traceability matters more here than assertion depth.

**Needs a live-Slack check, not a unit test.** Four claims no fake can settle:

1. **Whether Slack's composer renders a section immediately followed by a list with no blank line** — the user-visible half of the "no empty line" criterion. Unit tests can only prove no literal `"\n\n"` run is emitted.
2. **Whether the drafts API accepts a `usergroup` element and the composer renders it as a live group mention.** `user` and `channel` are well attested in drafts; `usergroup` is the least certain, and `drafts.create` is undocumented and has already been seen to reject subtly-wrong payloads with `invalid_message`.
3. **That a text element reading `@here` is genuinely inert** — that sending the draft does not cause Slack's client to re-parse it into a broadcast. This is the safety-critical claim in AC 2.3 and the one least suitable to take on trust.
4. **Real `usergroups.list` behaviour per token type** (`paid_only`, `missing_scope`) mapping to "could not verify" rather than a spurious "does not exist".

Items 2 and 3 are verified against the live workspace before the PR merges, using a throwaway draft in a DM to self.
