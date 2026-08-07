# Tasks — 005-draft-formatting-fidelity

Slices are ordered so the converter is correct before anything talks to Slack. Slices 1–2 are pure, network-free changes to `markdownToRichTextBlock`; Slice 3 adds the read-only name resolver; Slice 4 adds the refusal gate. After every slice `make build` and `make test` must be green and both write tools remain usable.

---

- [x] **Slice 1: Bullet-character lines become real Slack lists**

  > End-to-end value on its own: a draft written with `•` renders as a native list with no stray blank line above it. No mention work, no network.

  - [x] Create `pkg/handler/draft_source.go` with the `draftSource` type (Text field only at this stage) and `preprocessDraftSource`, implementing the line-scan bullet rewrite: fenced-code state machine (``` ``` ``` and `~~~`, matching fence char and run length), indented-code state, and the `^([ \t]{0,3})([•◦‣⁃·])([ \t]+)(.*)$` rewrite to `- ` preserving the other groups byte-for-byte. **[Agent: go-mcp-backend]**
  - [x] Wire `preprocessDraftSource` into `markdownToRichTextBlock` ahead of the goldmark parse, keeping the exported signature unchanged so the `markdownToRichTextBlockFn` seam and the `conversations_add_message` call site still compile. **[Agent: go-mcp-backend]**
  - [x] Create `pkg/handler/draft_source_test.go`: table test over all five markers; `•a` (no space) left alone; mid-sentence `·` left alone; markers inside fenced, `~~~` and 4-space-indented blocks left alone; unclosed fence; fence nested in a list item; three leading spaces rewritten, four not. **[Agent: go-mcp-backend]**
  - [x] Extend `pkg/handler/draft_richtext_test.go`: `"Intro:\n\n• a\n• b\n\nOutro."` yields `[section("Intro:"), rich_text_list(bullet, indent 0)[a,b], section("Outro.")]` with **no** `"\n\n"` text run between intro and list; the `-`/`*`/`+`/`1.` forms still produce lists unchanged; a `draftContentLoss` table asserting an empty result for every bullet marker. **[Agent: go-mcp-backend]**
  - [x] Add a regression test asserting `"Intro:\n1) a\n2) b\n"` already yields `style: "ordered"` with an empty loss result, and that no code rewrites `1)` — the tech spec establishes goldmark handles it and that rewriting it would manufacture false content-loss refusals. **[Agent: go-mcp-backend]**
  - [x] Verify: run `make build` and `make test`; confirm the new tests pass and no existing handler test regressed. Delete any scratch probe files created during the check. **[Agent: go-mcp-backend]**

---

- [x] **Slice 2: Slack references become live mention elements**

  > A draft containing `<@U…>`, `<#C…>` or `<!subteam^S…>` produces real `user`/`channel`/`usergroup` elements; `@here`/`@channel`/`@everyone` stay inert text. Still no network — validation lands in Slice 4.

  - [x] Create `pkg/handler/draft_mentions.go` with `mentionKind`, `mentionRef`, the four-alternation reference grammar from the tech spec (user-ID class mirroring `provider.slackUserIDPattern`), `parseMentionRef`, `canonicalLiteral()` and `element()`. `element()` must return a plain `RichTextSectionTextElement` reading `@here`/`@channel`/`@everyone` for broadcasts — `RichTextSectionBroadcastElement` is never constructed by the converter. **[Agent: go-mcp-backend]**
  - [x] Extend `preprocessDraftSource` to lift every reference literal out of the source into `draftSource.Refs`, replacing each with a `U+E000 <index> U+E000` placeholder, skipping fenced and indented code; return an error when the raw input already contains `U+E000`. Add `expandPlaceholders` and `splitPlaceholders`. **[Agent: go-mcp-backend]**
  - [x] Add `splitSectionElements` and wire it into `markdownToRichTextBlock` so every emitted section's element list has placeholders expanded into text/user/channel/usergroup elements. Pass the `draftSource` into `codeBlockText` and `inlineNodeText` so code spans, preformatted blocks and link labels expand placeholders back to the original literal instead of converting. **[Agent: go-mcp-backend]**
  - [x] Keep `draftContentLoss` balanced: add `canonicalizeReferences` and apply it to the `want` side, and extend `flattenRichTextContent` with the four mention element types each emitting `canonicalLiteral()`. Both halves are required — either one alone produces spurious "conversion dropped content" refusals. **[Agent: go-mcp-backend]**
  - [x] Create `pkg/handler/draft_mentions_test.go`: all nine grammar forms plus near-misses (`<@lowercase>`, `<@U>`, `<!subteam^X123>`, unterminated `<#C123`); `canonicalLiteral` round-trips; surrounding punctuation and spacing byte-identical after splitting; and the §2.3 enforcement assertion that broadcasts never produce a `RichTextSectionBroadcastElement`. **[Agent: go-mcp-backend]**
  - [x] Extend `pkg/handler/draft_richtext_test.go`: mentions inside a list item, blockquote and heading; a mention in a backtick code span expanding to the literal and **not** becoming an element; a fenced block containing `<@U123>` surviving verbatim; a placeholder surviving a goldmark round-trip intact in paragraph/heading/list-item/blockquote/emphasis/link-label positions; a global assertion that no emitted string contains `U+E000`; and a `draftContentLoss` table covering every reference form including labelled ones. Update the existing `flattenRichText` helper with the four new element cases or it will silently under-report. **[Agent: go-mcp-backend]**
  - [x] Verify: run `make build` and `make test`; confirm loss-check tests are green (this is the false-positive tripwire) and that `text/plain` and `application/json` behaviour is untouched. Delete any scratch probe files. **[Agent: go-mcp-backend]**

---

- [x] **Slice 3: Readback shows mentions by name**

  > The assistant reading a draft back sees `@dana` / `#general` / `@eng` instead of raw codes, so it can tell the user who it tagged. Read-only — nothing new is written to Slack.

  - [x] Create `pkg/handler/mention_directory.go` with the `mentionNamer` interface and the naming half of `mentionDirectory`: users from `ProvideUsersMap()` (falling back to `PatchUser`), channels from `ProvideChannelsMaps()` (falling back to `GetConversationInfoContext`), user groups from a per-call memoised `GetUserGroupsContext(IncludeDisabled(true), IncludeUsers(false), IncludeCount(false))` issued **only** when a group is actually referenced. `Name` never errors — it returns `""` when unknown. **[Agent: go-mcp-backend]**
  - [x] Thread `ctx` and a `mentionNamer` through `renderDraftBlocksText` → `renderRichTextElement` → `renderRichTextSectionElements`, emitting the resolved name and falling back to `canonicalLiteral()` when unknown. Add the `mentions` field and `mentionDir()` accessor to `ConversationsHandler` (nil ⇒ build from `apiProvider`), matching the existing `drafts`/`isOAuthTokenFn` seam idiom, and make `draftContentPayload` a method taking `ctx`. **[Agent: go-mcp-backend]**
  - [x] Extend the `DISPLAY ONLY` comment block in `pkg/handler/draft_normalize.go` to record that mention names are resolved live and are therefore not stable across calls — two readbacks of the same draft can differ if a user is renamed, which makes the readback doubly unsuitable as a comparison or restore source. Leave `normalizeDraftBlocks` and `parseVerbatimDraftBlocks` untouched. **[Agent: go-mcp-backend]**
  - [x] Extend `pkg/handler/draft_normalize_test.go`: stub-namer rendering for user/channel/usergroup; unknown name falling back to the canonical literal rather than being omitted; a human-authored draft's existing `RichTextSectionBroadcastElement` still rendering `@here`; and an assertion that `normalizeDraftBlocks` output is byte-identical before and after this change, since the lossless-restore contract must not move. **[Agent: go-mcp-backend]**
  - [x] Verify: run `make build` and `make test`; confirm the readback resolves names against the stub and that the drafts provenance comparison still uses `blocks_json` and never `text`. Delete any scratch probe files. **[Agent: go-mcp-backend]**

---

- [x] **Slice 4: An unverifiable reference refuses the write**

  > The safety slice. A bad reference stops the draft or the post before any drafts API call runs, so an existing draft can never be damaged by a refusal.

  - [x] Add the exported `provider.ErrUserNotFound` sentinel in `pkg/provider/api.go` (mirroring the `ErrRefreshRateLimited` idiom), returned by `PatchUser` when `GetUsersInfo` yields an empty result or Slack answers `users_not_found`/`user_not_found`, and route `PatchUser` through `limiter.CallWithRetry` at the appropriate tier. **[Agent: go-mcp-backend]**
  - [x] Add the verification half of `mentionDirectory`: `errRefNotFound` / `errRefUnverifiable` sentinels, the `mentionVerifier` interface, and per-class mapping — user via `ErrUserNotFound`, channel via `channel_not_found` (with `missing_scope` and `not_in_channel` mapping to **could-not-verify**, since an invisible private channel is not proof of absence), user group via the memoised list (`missing_scope`/`paid_only`/`not_allowed_token_type`/429 ⇒ could-not-verify). Memoise negative results too. **[Agent: go-mcp-backend]**
  - [x] Add `collectMentionRefs` (deduplicated by ID, document order) and `validateMentions`, returning the first failure wrapped with the offending canonical literal and one of the two distinct message forms — each naming the reference and stating that nothing was written, and the could-not-verify form naming the missing scope where applicable. **[Agent: go-mcp-backend]**
  - [x] Wire `validateMentions` into `ConversationsDraftMessageHandler` in the `text/markdown` branch after `checkDraftContentLoss` and **above** the `DraftsList` call, with a comment recording that the ordering is a contract. Wire it into `ConversationsAddMessageHandler`'s `text/markdown` branch as a hard return — the existing "conversion error ⇒ fall back to plain text" behaviour stays for conversion errors only. Leave `application/json`, the raw `blocks` parameter and `text/plain` exempt. **[Agent: go-mcp-backend]**
  - [x] Extend `pkg/handler/conversations_draft_test.go` with the keystone test: an unresolvable reference returns an error naming it, and the fake `draftsAPI` recorded **zero** `DraftsList`/`DraftsCreate`/`DraftsUpdate` calls — run twice, once with no pre-existing draft and once with the fake pre-loaded with a draft at the destination, asserting it is untouched. Add: not-found vs could-not-verify producing textually distinct errors; `application/json` with a mention-bearing block asserting `Verify` was never called and the blocks pass through byte-identical; `text/plain` containing `<@U123>` preserved literally with no verification; and a message with zero references making zero provider calls. **[Agent: go-mcp-backend]**
  - [x] Extend `pkg/handler/conversations_addmessage_test.go` with handler-level coverage for both behaviour changes: a valid mention posting a block containing a `user` element, and an unresolvable mention returning an error with `PostMessageContext` never called. **[Agent: go-mcp-backend]**
  - [x] Verify: run `make build` and `make test`; confirm the zero-drafts-API-calls assertion holds and that no refusal path can reach the drafts client. Delete any scratch probe files. **[Agent: go-mcp-backend]**

---

- [x] **Slice 5: Tool descriptions, release notes, and live-workspace confirmation**

  > Makes the new behaviour discoverable to the calling model and confirms against real Slack the four claims no fake can settle.

  - [x] Update the `conversations_draft_message` and `conversations_add_message` tool descriptions in `pkg/server/server.go` to state that common bullet characters and Markdown dashes both produce native lists, that `<@…>`/`<#…>`/`<!subteam^…>` become live mentions that notify, that `@here`/`@channel`/`@everyone` are written as inert text, and that an unrecognised reference refuses the write without touching an existing draft. **[Agent: go-mcp-backend]**
  - [x] Write `context/spec/005-draft-formatting-fidelity/release-notes.md` covering the three caller-visible changes to `conversations_add_message` — an unresolvable reference now fails where it previously posted a raw code, valid references now post as live mentions that notify people, and `•` lines now change the posted block structure — following the format used by `context/spec/004-safe-draft-lifecycle/release-notes.md`. **[Agent: release-infra]**
  - [x] Verify against the live workspace, using a throwaway draft in a DM to self and **only after confirming with the user that a real draft may be created**: (1) that Slack's composer renders a section immediately followed by a list with no blank line between them; (2) that `drafts.create` accepts a `usergroup` element and the composer renders it as a live group mention — `drafts.create` is undocumented and has rejected subtly-wrong payloads with `invalid_message` before; (3) that a text element reading `@here` is genuinely inert and is not re-parsed into a broadcast on send; (4) that a real `usergroups.list` failure maps to "could not verify" and never to a spurious "does not exist". Delete the throwaway draft afterwards. **[Agent: go-mcp-backend]**

---

- [x] **Slice 6: Feature Testing & Regression**

  > Verifies the whole feature end-to-end against functional-spec.md, run after all implementation slices are complete.
  - [x] Read functional-spec.md acceptance criteria in full. Generate acceptance-level tests that verify the entire feature as a whole — not individual slices. Cover applicable layers (unit for pure logic, integration for service interactions, e2e for user flows) based on the project's testing stack. Write tests with RED validation (must fail before implementation is confirmed done). Annotate each test with `@spec: 005-draft-formatting-fidelity` and `@regression` if suitable for long-term regression. **[Agent: testing-expert]**
  - [x] Run all generated tests. All must pass. Fix any failures before proceeding. **[Agent: testing-expert]**
