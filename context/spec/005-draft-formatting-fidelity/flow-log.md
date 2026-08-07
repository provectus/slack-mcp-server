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
