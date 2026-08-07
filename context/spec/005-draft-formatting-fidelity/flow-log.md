# Flow Log — 005-draft-formatting-fidelity

## Feature

**Draft & Message Formatting Fidelity — bullet-character lists and Slack mention references in drafted/posted messages.**

Defect-driven, not a roadmap item. Source: user report via `/implement-feature` on 2026-08-07 — drafts render `•` lists as plain lines with a stray blank line above, and person/channel references stay as raw codes (`<@D0BJE66FPU5>`).

---

## 2026-08-07 — Investigation (pre-spec)

Both defects reproduced locally against `markdownToRichTextBlock` (`pkg/handler/draft_richtext.go:41`), the converter shared by `conversations_draft_message` and `conversations_add_message` (`content_type: text/markdown`, `pkg/handler/conversations.go:275`):

1. **Bullet-character lists are not lists.** Input `"Intro:\n\n• a\n• b\n\nOutro."` yields a single `rich_text_section` whose text runs are `"Intro:"`, `"\n\n"`, `"• a"`, `"\n"`, `"• b"`, `"\n\n"`, `"Outro."` — goldmark does not treat `•` as a list marker, so the paragraph-join at `draft_richtext.go:66` supplies the blank line the user reported. Input `- a` does produce a correct `rich_text_list` (style `bullet`, indent 0), so the gap is marker recognition only.
2. **Mentions are not resolved.** Input `"cc <@U03MT3U0F6E> and <#C123ABC> <!here>"` yields plain text elements carrying the raw codes; goldmark's raw-HTML handling additionally fragments `<!here>` into `"… <"` + `"!here>"`. `renderDraftBlocksText` (`draft_normalize.go:107`) echoes `<@ID>` back, so the readback is unresolved too.

Prevalence, from local Claude Code transcripts (49 recorded draft/add-message calls): 25 used `•` bullets, 8 used `- `, 11 contained `<@…>`/`<#…>` references.

**Next stage:** specs.

---

## 2026-08-07 — Specs: functional spec

Produced `context/spec/005-draft-formatting-fidelity/functional-spec.md` (Status: Draft) via `/awos:spec` Creation Mode. Approved at the flow's Step 4 gate.

Decisions taken in the interview:

- People, channels and user groups become live mentions; `@here` / `@channel` / `@everyone` stay inert plain text, so the assistant can never arm a workspace-wide alert.
- An unresolvable or unverifiable reference refuses the write (drafting and posting alike) with an explanation naming the offending reference; nothing is written and existing drafts are untouched.
- Readback shows resolved `@name` / `#channel` / `@group`.
- Accepted list markers extend to `•` `◦` `‣` `⁃` `·` and the `1)` numbered form, on top of the existing `-` `*` `+` `1.`.
- No new on-disk persistence: users and channels come from the existing caches; a user group is looked up live only when one is actually referenced.

**Next stage:** specs — technical considerations (`/awos:tech`).

---

## 2026-08-07 — Specs: technical considerations and tasks

Produced `technical-considerations.md` (design drafted by the `go-mcp-backend` specialist, synthesised by the orchestrator) and `tasks.md` — six slices, all Go tasks on `go-mcp-backend`, release notes on `release-infra`, the final regression slice on `testing-expert`. No `general-purpose` fallbacks; no new capability needed, so `/awos:hire` was not required. Approved at the flow's Step 4 tech gate; the task list is flow-accepted per the flow's Step 4.3 override, so the `<!-- not-user-reviewed -->` marker was removed without a separate review question.

Chosen design: a pre-parse source rewrite (bullet markers → `-`, reference literals → `U+E000`-delimited index placeholders) plus a post-parse placeholder split, with validation in the handler above the drafts API calls.

Four spot-checks run against the specialist's report before accepting it — all confirmed:

- `"Intro:\n1) a\n2) b\n"` already yields `style: "ordered"` with an empty loss result. goldmark implements CommonMark's `)` ordered marker, so **no `1)` rewrite will be written**; rewriting it to `1.` would strip the `1` from the `got` side while the `want` side still tokenised it, manufacturing a false content-loss refusal. That functional-spec criterion becomes a regression test.
- `<!subteam^S0123ABC|@eng>` is parsed by goldmark as a **link** element, not text — which is what rules out post-processing assembled text elements and forces the pre-parse placeholder design.
- `RichTextSectionUserGroupElement` carries only `Type` and `UsergroupID` (slack-go v0.19.0), so no display name can be embedded — Slack resolves it client-side.
- The draft tool is registered only for non-OAuth session tokens (`pkg/server/server.go:238`), so the mention work is token-agnostic only where it must be: `conversations_add_message`.

Assumptions recorded in the tech spec beyond the functional spec (surfaced to the user at the gate): broadcasts render as the text `@here` rather than the literal `<!here>`; the verbatim paths (`application/json`, raw `blocks`, `text/plain`) are exempt from validation to protect the lossless-restore contract; a new `provider.ErrUserNotFound` sentinel is needed because `PatchUser` cannot today distinguish "no such user" from "network failed".

**Next stage:** commit specs, then implement.

---

## 2026-08-07 — Commit specs

`a7d7569 docs(spec): add 005-draft-formatting-fidelity functional and technical spec` on branch `fix/draft-formatting-fidelity` (branched from `fork/master` at `1455b73`). Branch type is `fix/` per §2 of the delivery flow, which mirrors the Conventional-Commit type — this is defect-driven work, not a roadmap feature.

**Next stage:** implement.

---

## 2026-08-07 — Implement

All six slices delivered via `/awos:implement`. Delegation was **per slice** rather than per task line: every task inside a slice edits the same files and must land consistently, and the slice is already defined as the unit that leaves the tree runnable with its own verify step. Each agent received its slice's task lines verbatim and reported per task; each slice was spot-checked by the orchestrator (`make test`, plus a converter probe or a source read) before its checkboxes were ticked.

Three findings during implementation that changed the work:

1. **`make test` filters on `.*Unit.*`.** The Makefile target is `go test -count=1 -v -run=".*Unit.*" ./...`, and CI runs `make test`. Slice 1's tests were named `TestMarkdownToRichTextBlock_*` / `TestDraftContentLoss_*` and therefore **never executed** — the false-positive tripwire for the loss check was silently dead. Caught during Slice 2 and fixed by renaming every new test to the project's `TestUnit…` convention; every later slice was told about it explicitly.
2. **Typed-nil Slack client in demo mode.** `ApiProvider` holds a typed-nil `*MCPSlackClient`, so `ap.Slack()` returns a non-nil interface over which every call panics. Slice 3's new live-fallback lookups in the readback path segfaulted on an existing acceptance test until gated behind a `slackReachable()` check, mirroring the nil-client check `IsOAuth`/`IsBotToken` already use.
3. **Directory sharing.** Slice 3 built a fresh `mentionDirectory` per call, so validation and readback would each have paid for their own `usergroups.list` fetch. Slice 4 builds one directory at the top of the handler and passes it to both.

**Next stage:** verify.

---

## 2026-08-07 — Live-workspace verification, and two defects it caught

The user approved live verification against the real workspace (throwaway draft plus one sent probe, both in a DM to self). The freshly built binary was driven over a stdio MCP handshake with the real session tokens — deliberately not the installed server, which runs the old code.

**Defect 1 — the originally reported case was still broken.** `conversations_draft_message` with `text: "bad ref check <@D0BJE66FPU5>"` returned `action: "replaced"` and wrote the raw code into the draft. Root cause: the user alternation was `<@([UW][A-Z0-9]{2,})…>`, so a `D…` code did not match the grammar **at all** — no `mentionRef` was created, `validateMentions` had nothing to check, and the literal survived as plain text. The strictness of the grammar made the malformed reference invisible to the very gate meant to catch it.

Worth recording *why* the model produces this ID: `users_search` returns a `DMChannelID` column alongside `UserID`, and the model had been pasting the DM channel ID into a person reference.

Fix: widened the `<@…>`, `<#…>` and `<!subteam^…>` alternations to `[A-Z][A-Z0-9]{2,}` and classify by prefix **after** matching, adding `mentionBadUser` / `mentionBadChannel` / `mentionBadUserGroup` kinds that are never converted to an element and are refused by `validateMentions` above any workspace lookup. Genuine near-misses (`<@lowercase>`, `<@U>`) still pass through as ordinary text.

**Defect 2 — the broadcast guarantee leaked through the notification fallback.** `conversations_add_message` attaches the caller's raw markdown as Slack's fallback `text` field. The blocks correctly held an inert `@here` text element, but the `text` field carried the literal `<!here>` — which is Slack's own canonical encoding of a live broadcast. Confirmed live: the posted message came back from `conversations.history` with `<!here>` in its `Text`. Fixed with `neutralizeBroadcasts`, applied to the fallback text on the blocks path and, after the first fix, on the conversion-error path too — the latter posts text with **no** blocks, so nothing renders the literal inert there and it was the worse of the two.

The drafts path needed no equivalent fix: `draftsCreateForm` / `draftsUpdateForm` carry no `text` field at all, only marshalled blocks.

**Live results after both fixes** (all four claims the tech spec flagged as unsettleable offline):

| Claim | Outcome |
| --- | --- |
| Composer renders a section immediately followed by a list with no blank line | Confirmed — list starts directly beneath the intro line, native Slack bullets |
| `drafts.create` accepts a `usergroup` element and renders it as a live group mention | Confirmed — `@docker-mentors` renders as a highlighted mention |
| A text element reading `@here` is genuinely inert | Confirmed — renders as plain unhighlighted text **beside three highlighted mentions in the same draft**, which is the decisive contrast |
| `usergroups.list` failure maps to could-not-verify, not does-not-exist | Partly — a session token carries the scope, so the not-found path was exercised live (`<!subteam^S0NOSUCH99>` refused correctly); the `missing_scope`/`paid_only` mappings remain unit-tested only, since a stealth token cannot produce them |

Evidence is a screenshot of the rendered draft. It was **deliberately not committed** to `docs/screenshots/` as `/awos:verify` prescribes: the capture is of the user's real Slack and shows colleagues' DM names, private channel names and unrelated message content, and `docs/screenshots/` is not git-ignored, so committing it would publish internal information to the fork. The screenshot was delivered to the user in-session instead.

Two throwaway artifacts remain in the user's self-DM for them to clear — the server exposes no draft-delete tool, and deleting their messages is not an action taken on their behalf.

**Next stage:** local review.

---

## 2026-08-07 — Verify

`/awos:verify` ran over `005-draft-formatting-fidelity`: all 29 tasks `[x]`, **26 acceptance criteria** marked verified, and both `functional-spec.md` and `technical-considerations.md` moved to `Status: Completed`. No roadmap item to tick — the work is defect-driven. Product context needs no update: no new technology, no new persistence, and the Phase 2 roadmap wording ("Markdown → native Slack rich_text formatting") remains accurate.

Evidence split: §2.1–§2.3 and §2.5 rest on the live render plus the acceptance suite; §2.4 rests on the acceptance suite, whose refusal-ordering test asserts the fake drafts API recorded **zero** calls. The `testing-expert` slice RED-validated its 15 acceptance tests by reverting nine behaviours one at a time in a throwaway copy of the tree and confirming that exactly the claiming tests failed each time.

Known coverage gap, left open deliberately: the per-class error-code mapping for the **user** and **channel** classes (`channel_not_found` ⇒ not-found; `missing_scope` / `not_in_channel` ⇒ could-not-verify) runs behind `ApiProvider.Slack()`, and `ApiProvider.client` is unexported with no constructor accepting a fake `SlackAPI`. Closing it needs a production seam, which was out of scope for a test-only slice. The user-group class *is* covered against the real mapping.

**Next stage:** local review.

---

## 2026-08-07 — Local review

Static gate first: `go fmt ./...`, `go vet ./...` and `make test` all clean. Then an independent review by `pr-review-toolkit:code-reviewer`, dispatched with the flow's fixed verbatim prompt and no run-time focus areas — the orchestrator had just driven the implementation, so naming focus areas would have imported the author's bias.

**Review file:** `context/spec/005-draft-formatting-fidelity/review.md` (git-ignored via `.gitignore:10`, so it stays local).

**Verdict: approve with changes** — 0 critical, 4 important, 4 below the reporting bar. All four important findings were kept after a manual keep/drop gate, and each was independently reproduced by the orchestrator before being acted on:

1. **`PatchUser` throttle regressed unrelated read paths.** The Tier3 (1.2s) limiter was applied *inside* `PatchUser`, but `PatchUser` has two callers outside the mention gate — `conversations.go:2816` and `:2901`, both on the `conversations_history` / `conversations_replies` / search render path. A history page with N uncached users would have paid ~1.2s each. Fixed by moving the throttle to the two `mentionDirectory` callers and leaving `PatchUser` unthrottled.
2. **Per-lookup limiter construction made the burst mitigation vacuous.** `tier.Limiter()` calls `rate.NewLimiter` on every invocation (`pkg/limiter/limits.go:16`), so building one per reference gave every lookup a fresh full burst and no cross-lookup throttling — and validation and readback did not share one. Fixed with three package-level limiters.
3. **A reference in ≥4-column-indented list content bypassed both conversion and the AC 2.4 gate.** Reproduced: for `"- a\n  - b\n\n    please review <@U0BOGUS1> thanks\n"` the line scanner called the line indented code while goldmark called it list content, so `refs_lifted=0`, `collected=0`, the raw code shipped into a live list item, and `validateMentions` had nothing to check. Same class as the `<@D0BJE66FPU5>` defect: a reference invisible to its own gate. The tech spec's "not a list-item continuation" clause (§2.2 step 2) had never been implemented. Fixed with **both** halves — a backstop in `collectMentionRefs` that verifies any well-formed reference still sitting in live text, and the missing list-context tracking in the line scanner. Confirmed after the fix: `refs_lifted=1`, `collected=1`, and the reference converts to a live `user` element, so AC 2.2 is restored rather than merely degraded to a refusal.
4. **`content_type: text/plain` fired a real workspace broadcast.** The review flagged an unresolved contradiction between the `text/plain` branch (raw passthrough, "literal means literal") and the conversion-error branch (neutralised), both under `MsgOptionDisableMarkdown()`, and noted the live matrix had never covered `text/plain`. Settled by a throwaway self-DM send: the `text/plain` message rendered as a **live amber `@here` pill and fired a real mention notification**, while the blocks-path message beside it rendered `@here` as plain inert text. `MsgOptionDisableMarkdown()` does not neutralise `<!here>`. AC 2.3 is unconditional ("in any form"), so `neutralizeBroadcasts` now applies to `text/plain` too, with the live evidence recorded in the branch comment; non-broadcast references still pass through verbatim there.

Also applied from below the bar: `splitPlaceholders` could fold an unresolved placeholder into the next literal span, the one path that could have leaked a `U+E000` rune to Slack — now structurally impossible.

Recorded and deliberately **not** fixed: `slackErrorCode` is duplicated across `pkg/handler` and `pkg/provider`, and a fenced code block nested ≥4 columns inside a list item is dropped entirely without `draftContentLoss` noticing — the latter is pre-existing on `fork/master`, unchanged by this diff, and deserves its own issue.

Three of the four important findings were defects the acceptance suite passed over, and two of them (3 and 4) were live-only or reproduction-only discoveries. The pattern worth carrying forward: every defect found in this feature was a reference or literal that some layer treated as invisible.

**Next stage:** commit and push.
