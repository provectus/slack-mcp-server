# Fix log — falsey-env-registers-tool

Bug ID: `falsey-env-registers-tool` (free-text; no ticket tracker in this project)

## Symptom

`shouldAddTool` (`pkg/server/server.go:101-118`) gates env-var-controlled tools on `os.Getenv(envVarName) != ""` when `SLACK_MCP_ENABLED_TOOLS` is empty. A falsey value such as `SLACK_MCP_ATTACHMENT_TOOL=false` is non-empty, so the tool is registered anyway. The handler-side check in `parseParamsToolFilesGet` (`pkg/handler/conversations.go:2654`) then rejects every invocation with `SLACK_MCP_ATTACHMENT_TOOL must be set to 'true', '1', or 'yes' to enable`. A user who explicitly disabled the tool still sees it advertised in their MCP client, and every call fails.

## Stage log

### fetch-bug — done

Bug arrived as free text during a scoping discussion about attachment downloads, not as a ticket. Two candidate defects were identified in `shouldAddTool`; the user selected this one (falsey env value registers a broken tool) via `AskUserQuestion`. The second candidate — non-empty `SLACK_MCP_ENABLED_TOOLS` silently overriding a tool's own enable env var — was explicitly deferred, because whether that is a defect at all depends on whether `SLACK_MCP_ENABLED_TOOLS` is meant to be a strict allowlist. It is recorded here so it is not lost.

Next: resume-detection.

### resume-detection — done

No prior flow log for this bug ID, so this is a fresh run. No open PR on `provectus/slack-mcp-server` covers it, and an upstream search of `korotovsky/slack-mcp-server` for `shouldAddTool` / `ENABLED_TOOLS` / `ATTACHMENT_TOOL` returned nothing.

Incidental finding, unrelated to this fix but worth acting on separately: fork issue #21 ("Add a tool to download/read file attachments from messages") is still open, but the `attachment_get_data` tool already implements what it asks for — `files.info` plus an authenticated fetch of `url_private_download`, text inline, binaries base64, size cap. The issue predates the feature reaching this fork. It should be closed or repurposed to cover the enablement and documentation gap instead.

Next: workspace.

### workspace — done

`context/` is an in-repo directory and reachable. Working tree was clean apart from an untracked `.DS_Store`, which is not in `.gitignore` — noted, not fixed here, since it is outside this bug's scope. Fetched `fork master` (at `10bb30c`) and branched from it.

Branch: `fix/falsey-env-registers-tool`

Next: diagnose.

### diagnose — done

Delegated to the `go-mcp-backend` specialist. Reproduction was executed, not asserted: `shouldAddTool("attachment_get_data", nil, "SLACK_MCP_ATTACHMENT_TOOL")` returns `true` for all four of `false`, `0`, `no`, `off`. The throwaway test used to prove it was deleted.

The surface sweep found the bug is wider than reported. Every `shouldAddTool` call site with a non-empty env var name is affected — six tools across five env vars:

| tool | env var | falsey behavior |
| --- | --- | --- |
| `conversations_add_message` | `SLACK_MCP_ADD_MESSAGE_TOOL` | register-then-fail |
| `conversations_draft_message` | `SLACK_MCP_DRAFT_MESSAGE_TOOL` | register-then-fail |
| `reactions_add` | `SLACK_MCP_REACTION_TOOL` | register-then-fail |
| `reactions_remove` | `SLACK_MCP_REACTION_TOOL` | register-then-fail |
| `attachment_get_data` | `SLACK_MCP_ATTACHMENT_TOOL` | register-then-fail |
| `conversations_mark` | `SLACK_MCP_MARK_TOOL` | **register-then-silently-allow** |

`conversations_mark` is the severe case and the orchestrator re-verified it directly rather than taking the report on trust: `SLACK_MCP_MARK_TOOL` is referenced at exactly one place in the codebase, `pkg/server/server.go:208`. `ConversationsMarkHandler` (`pkg/handler/conversations.go:962`) performs no env check, so a tool the operator explicitly disabled registers *and* every call succeeds, reaching the real write at `MarkConversationContext` (`conversations.go:1009`). It is the only write tool with no handler-side gate; the other four all have one.

The three channel-allowlist tools were checked for the opposite failure — a falsey value being read as a permissive allowlist — and they are safe. `isChannelAllowedForConfig` (`conversations.go:2153`) allows only on `""`, `"true"`, or `"1"`; `"false"` becomes a one-item allowlist matching no real channel ID, so it denies rather than grants.

No shared boolean-env helper exists; five call sites parse truthiness independently, and only `IsUnfurlingEnabled` (`pkg/text/text_processor.go:243`) recognizes falsey values at all. That makes `pkg/text` the natural home for one.

Two adjacent defects were found and deliberately left out of scope, recorded so they are not lost: `SLACK_MCP_ADD_MESSAGE_TOOL=yes` registers the tool but denies every channel, because `isChannelAllowedForConfig` does not accept `"yes"` while the attachment gate does; and `validateToolConfig` (`cmd/slack-mcp-server/main.go:255`) runs only for `SLACK_MCP_ADD_MESSAGE_TOOL`, not for the draft or reaction variants.

Next: classify.

### classify — done

**Verdict: conformance. No owning spec.**

Specs 001–005 do not cover tool-registration gating; `001-channel-members` mentions `SLACK_MCP_ENABLED_TOOLS` only because it added a tool to that list, which does not make it the owner of gating semantics. The contract that the code violates is documented in `README.md` — `SLACK_MCP_MARK_TOOL` and `SLACK_MCP_ATTACHMENT_TOOL` are both specified as enabling on `true`, `1`, or `yes`, which the falsey values plainly are not. The spec is correct and the code disagrees with it, so no `/awos:spec` amendment is due and the `amend-spec` stage will be skipped.

One documentation defect surfaced while reading that contract: README documents `SLACK_MCP_MARK_TOOL` twice, and the two rows contradict each other — line 420 says `true`, `1`, or `yes`, line 424 says `true` or `1`. Since it is the same env var this bug centers on, correcting it belongs in this fix rather than a separate pass.

Next: fix.

### fix — dispatched

Scope confirmed with the user via `AskUserQuestion`: the falsey guard alone would still leave non-falsey junk (`banana`, a whitespace-only value) silently enabling the `conversations_mark` write tool, so the handler-side gate is included rather than deferred. Delegated to `go-mcp-backend`: add `IsFalsey`/`IsTruthy` to `pkg/text`, apply the guard in `shouldAddTool`, add the missing gate to `ConversationsMarkHandler`, reuse the helper in `parseParamsToolFilesGet`, and de-duplicate the README row. `isChannelAllowedForConfig` is explicitly off-limits — its allow-set deliberately omits `"yes"`, and changing it would move channel-allowlist semantics for three tools.

Next: regression-test.

### fix — done

Four source changes plus a documentation correction, 40 lines total:

- `pkg/text/text_processor.go` (~310-329) — new `IsFalsey` and `IsTruthy`, both trimming and lowercasing. Falsey is `false`/`0`/`no`/`off`, truthy is `true`/`1`/`yes`. `pkg/text` was chosen because it already held the only falsey-aware parser in the codebase and imports no internal packages, so neither `pkg/server` nor `pkg/handler` gains a cycle.
- `pkg/server/server.go:113-116` — the env branch of `shouldAddTool` now trims and returns `v != "" && !text.IsFalsey(v)`. The `SLACK_MCP_ENABLED_TOOLS` precedence branches are untouched.
- `pkg/handler/conversations.go:965-979` — `ConversationsMarkHandler` gained the env gate it never had, mirroring `parseParamsToolFilesGet` so the two read alike.
- `pkg/handler/conversations.go:2684` — `parseParamsToolFilesGet` reuses `text.IsTruthy`; its error strings are unchanged byte-for-byte.
- `README.md` — the duplicate, self-contradicting `SLACK_MCP_MARK_TOOL` row is gone; the surviving row carries the accurate `true`/`1`/`yes` values, the disabled-by-default rationale, and the `SLACK_MCP_ENABLED_TOOLS` sentence.

The orchestrator read the diff rather than accepting the report, and checked the two things most likely to break: `isChannelAllowedForConfig` is untouched, so channel-allowlist semantics are unmoved; and the `SLACK_MCP_ADD_MESSAGE_MARK` auto-mark path (`conversations.go:361`) calls `MarkConversationContext` directly rather than through the gated handler, so it still works when `conversations_mark` is disabled — which is correct, since it is a different feature under a different env var.

One deliberate behavior change beyond the bug: `IsTruthy` trims and lowercases where the old inline check was exact-match, so `SLACK_MCP_ATTACHMENT_TOOL=TRUE` now enables the tool instead of registering it and failing every call. This is an improvement and matches the documented contract, but it is a change and is called out in the PR body.

Next: regression-test.

### regression-test — done

Tests were added in three packages, all named `TestUnit*` — `make test` runs `-run=".*Unit.*"`, so any other name is silently never executed. That trap is live in this repo: all ten pre-existing tests in `pkg/text/text_processor_test.go` are named `TestXxx` and have therefore never run. Renaming them is out of scope here but is worth its own change.

- `pkg/server/server_test.go` — `TestUnitShouldAddToolFalseyEnv` (13 subtests) and `TestUnitShouldAddToolEnabledToolsPrecedenceOverFalseyEnv` (2 subtests). Coverage deliberately includes channel allowlists and negated allowlists expecting `true`, to catch an over-broad fix, not just the falsey cases expecting `false`.
- `pkg/handler/conversations_mark_test.go` (new) — `TestUnitConversationsMarkHandlerGateBlocksWhenDisabled` (unset, `false`, `banana`) and `TestUnitConversationsMarkHandlerGateAllowsWhenEnabled` (`true`, and via `SLACK_MCP_ENABLED_TOOLS`).
- `pkg/text/text_processor_test.go` — `TestUnitIsFalsey` and `TestUnitIsTruthy`.

`ConversationsMarkHandler` has no test seam for the Slack client, unlike the add-message and draft handlers. Rather than fake the write or drop the "never writes" assertion, the fixture boots a real demo-mode `ApiProvider` whose `Slack()` wraps a nil client, so any call reaching `MarkConversationContext` panics before any network I/O; the helper recovers that panic and reports it as a reached/not-reached signal. No real Slack write can occur. The tradeoff is that the *allows* test asserts a panic does happen, so it would invert if demo mode ever returned a working client — acceptable given the absent seam, and documented in the file.

**Fail→pass was reproduced by the orchestrator, not merely reported.** With the three fixed source files stashed and the test files kept: `pkg/text` failed to build on `undefined: IsFalsey` / `undefined: IsTruthy`; `TestUnitShouldAddToolFalseyEnv` failed on exactly the seven falsey and whitespace subtests and no others; `TestUnitConversationsMarkHandlerGateBlocksWhenDisabled` failed all three subtests, reaching the Slack client every time because pre-fix there was no gate at all. After `git stash pop`, `make test` returned 519 passing assertions and zero failures.

Next: verify-criteria.

### verify-criteria — done

There is no owning spec, so the criteria under test are the README-documented contract. Because the fix changes tool *registration* — what the MCP client is shown — a unit test alone would not have demonstrated the user-visible behavior, so the binary was built and driven over a real stdio MCP handshake (`initialize` + `tools/list`) using demo credentials and `--no-cache`, which keeps it network-free and away from the service's cache files.

| config | expected | observed |
| --- | --- | --- |
| all gating vars unset | no write tools | none registered |
| `SLACK_MCP_MARK_TOOL=false`, `SLACK_MCP_ATTACHMENT_TOOL=false` | neither registered | none registered |
| `SLACK_MCP_MARK_TOOL=true`, `SLACK_MCP_ATTACHMENT_TOOL=1` | both registered | both registered |
| `SLACK_MCP_ADD_MESSAGE_TOOL=C123,C456` | registered | registered |
| `SLACK_MCP_MARK_TOOL="   "` | not registered | not registered |

Two notes on the live environment. The running service's config sets `SLACK_MCP_REACTION_TOOL`, `SLACK_MCP_MARK_TOOL`, and `SLACK_MCP_DRAFT_MESSAGE_TOOL` all to `true`, so this fix is a no-op for it. And `make build` overwrote the binary the launchd service is pinned to (`build/slack-mcp-server`) with a dirty branch build; the running process still holds the previous binary in memory, but the pin must be rebuilt from `fork/master` before this flow ends.

Next: amend-spec (skipped — conformance) then local-review.

### amend-spec — skipped

Classification was conformance against a correct contract, so there is no spec to amend.

Next: local-review.

### local-review — done

Static gate first: `go fmt`, `go build ./...`, and `make test` all clean. The AI review was dispatched to a fresh `code-reviewer` subagent with the fixed verbatim prompt, so it never saw this conversation's authorship reasoning.

Review file: `context/review-falsey-env-registers-tool.md`
**Verdict: request changes** — 1 High, 4 Medium, 5 Low, 1 operational.

The High was a regression this branch introduced, and the orchestrator confirmed it directly at `cmd/slack-mcp-server/main.go:41-43` rather than taking the report on trust. `--enabled-tools` resolved the allowlist with the env var only as a *fallback* and never wrote the resolved value back, so the new mark gate — which reads `SLACK_MCP_ENABLED_TOOLS` from the environment — refused every call for an operator who enabled the tool by flag. That configuration worked before this branch. The same gap already existed for the four sibling tools, which is presumably why the pattern was copied, but only `conversations_mark` was newly broken by it.

The review also surfaced that nine of the twelve tests in `pkg/server/server_test.go` do not match `make test`'s `-run=".*Unit.*"` filter and have therefore never executed — including `TestShouldAddTool_Matrix`, the test that should have caught the original bug.

The user chose to fix H1 plus all four Mediums via `AskUserQuestion`, leaving the five Lows as follow-ups, and settled the precedence question the review exposed: **`SLACK_MCP_ENABLED_TOOLS` wins.** A tool listed there is registered *and* callable regardless of its own env var. That resolves a contradiction the branch had created, where the new precedence test asserted the allowlist wins while the handler gates asserted the opposite; neither behavior was owned by anything before.

That decision produced a single governing invariant for this PR, which is what the fixes were written against: **a tool is never advertised to the MCP client but permanently broken — registration and the handler gate must agree in every combination.**

Fixes applied by a fresh agent working from the review file rather than a summary:

- **H1** — `resolveEnabledTools` (`cmd/slack-mcp-server/main.go:243-260`) now exports the resolved allowlist back to `SLACK_MCP_ENABLED_TOOLS`, so flag and env become indistinguishable to every handler gate. This also repairs the four pre-existing cases.
- **M1** — `shouldAddTool` split into two intents over a shared body (`pkg/server/server.go:101-136`): channel-scoped vars keep `!IsFalsey`, while the two boolean-only vars (`SLACK_MCP_MARK_TOOL`, `SLACK_MCP_ATTACHMENT_TOOL`) now require `IsTruthy` via `shouldAddBooleanTool`, so junk values stop registering an uncallable tool.
- **M1 (handler half)** — both `ConversationsMarkHandler` and `parseParamsToolFilesGet` now allow a call whenever `SLACK_MCP_ENABLED_TOOLS` lists the tool. `parseParamsToolFilesGet` previously consulted the allowlist only when its own var was empty, which contradicted the chosen rule; the two handlers now agree. Error strings are unchanged byte-for-byte.
- **M2** — the leaking `os.Unsetenv` calls in the new server tests now register a restore via `t.Setenv(key, "")` first.
- **M3** — coverage extended to all five env vars and both variants, plus a new `cmd/slack-mcp-server/main_test.go` pinning the flag/env resolution that H1 broke.
- **M4** — README gained one paragraph documenting the shared convention: the falsey spellings, which two variables are boolean-only, and that an `SLACK_MCP_ENABLED_TOOLS` listing always wins.

Re-verified by the orchestrator after the fixes: `make test` green at 542 passing assertions, and the invariant checked end-to-end over a real stdio MCP handshake across ten configurations — flag-based enablement, junk values, the allowlist-plus-falsey combination, the original bug, case-insensitive truthy, and channel allowlists. Every case matched expectation and no configuration produced a listed-but-broken tool. `--enabled-tools attachment_get_data` now works too, a pre-existing breakage fixed as a side effect of H1.

Deferred as follow-ups, not lost: renaming the nine dormant `pkg/server` tests so `make test` actually runs them (highest value — it is the gap that let this bug ship); a `markClient()` seam so the mark test can assert on a recorded call instead of a recovered panic; consolidating the three divergent truthy parsers, including the `isChannelAllowedForConfig` allow-set that deliberately omits `yes`; substring versus list-element matching on the raw `SLACK_MCP_ENABLED_TOOLS` value; and `validateToolConfig` running only for `SLACK_MCP_ADD_MESSAGE_TOOL`.

Next: commit-push. This is the flow log's last committed entry — from the PR onward, progress is reported to the user and resumed from remote state.



