# Review — `fix/falsey-env-registers-tool`

**Verdict: request changes.** The reported bug is genuinely fixed and the regression tests are real (verified RED pre-fix / GREEN post-fix). But the new handler-side gate on `ConversationsMarkHandler` breaks a previously working enablement path — `--enabled-tools conversations_mark` — because the gate reads `SLACK_MCP_ENABLED_TOOLS` from the environment while `main.go` lets the CLI flag take precedence over that variable and never exports it back. That is H1 below and it is a one-line fix.

Scope is otherwise disciplined: 40 lines of source across three files, no changes to rate limiting, retry/429 handling, cache read/write paths, or token-capability gating. `isChannelAllowedForConfig` was correctly left untouched.

Reviewed: working-tree diff for `README.md`, `pkg/handler/conversations.go`, `pkg/server/server.go`, `pkg/server/server_test.go`, `pkg/text/text_processor.go`, `pkg/text/text_processor_test.go`, plus the new untracked `pkg/handler/conversations_mark_test.go`. `git diff fork/master...HEAD` is empty — nothing is committed yet.

## What was verified, not assumed

- `go build ./...`, `go vet ./pkg/...`, `gofmt -l pkg/` — all clean.
- All new and adjacent tests pass on the working tree.
- Fail-to-pass reproduced independently: an isolated pre-fix copy of the tree (source reverted, test files kept) fails `TestUnitShouldAddToolFalseyEnv` on exactly the seven falsey/whitespace subtests, and fails all three `TestUnitConversationsMarkHandlerGateBlocksWhenDisabled` subtests by reaching the Slack client every time. The regression tests do target the changed lines.
- Both behaviors in H1 and M1 were reproduced end-to-end against real binaries over a stdio MCP handshake with a demo token, `--no-cache`, and temp cache paths — no network, no writes, no impact on the running service.
- The user's working tree was not modified by this review. All experiments ran on copies under the session scratchpad.

## H1 — `--enabled-tools conversations_mark` now refuses every call (regression introduced by this change)

`main.go:42-44` resolves the enable list as *flag first, env as fallback*:

```go
if enabledToolsFlag == "" {
    enabledToolsFlag = os.Getenv("SLACK_MCP_ENABLED_TOOLS")
}
```

The parsed `enabledTools` slice is passed to `NewMCPServer`, so registration is correct — `shouldAddTool` sees the tool in the allowlist and registers it. But the new gate at `pkg/handler/conversations.go:966-980` re-derives the allowlist by reading the environment directly:

```go
enabledTools := os.Getenv("SLACK_MCP_ENABLED_TOOLS")
```

When the operator used the flag, that variable is empty. With `SLACK_MCP_MARK_TOOL` also unset, the gate takes the "disabled by default" branch and refuses.

Reproduced against the branch build (`build/slack-mcp-server`), env cleared of both variables:

```
$ … --transport stdio --no-cache --enabled-tools conversations_mark
tools/call conversations_mark →
  "by default, the conversations_mark tool is disabled. To enable it, set the
   SLACK_MCP_MARK_TOOL environment variable to true or 1"   (isError: true)
```

Against a pre-fix binary built from the same tree with only the two source hunks reverted, the identical invocation proceeds to the Slack client (it panics on the nil demo client, which is exactly the write path). So this configuration worked before and does not work now.

The same flag-vs-env gap already exists for the four sibling tools — confirmed on the pre-fix binary, `--enabled-tools attachment_get_data` and `--enabled-tools conversations_add_message` both refuse with their own "disabled by default" message. That is pre-existing and is presumably why the pattern was copied. It does not make it correct to extend it to a tool that previously worked.

Cheapest correct fix, which also repairs the four pre-existing cases, in `main.go` right after the flag/env resolution:

```go
os.Setenv("SLACK_MCP_ENABLED_TOOLS", enabledToolsFlag)
```

The structurally better fix is to thread the parsed `enabledTools` slice into `NewConversationsHandler` so the handlers stop re-reading process environment for a value the server already parsed and validated. That is a larger change; either is acceptable, but H1 should not ship unaddressed.

## M1 — "register-then-fail" still reachable for `conversations_mark` and `attachment_get_data`

The bug's stated symptom is a tool advertised to the MCP client that can never be successfully called. The falsey guard removes the `false`/`0`/`no`/`off` spellings of it, but two inputs still produce it. Both reproduced:

```
shouldAddTool(mark, [], MARK_TOOL="banana")                    = true   → handler errors
shouldAddTool(mark, ["conversations_mark"], MARK_TOOL="false") = true   → handler errors
```

For the second, the handler returns `SLACK_MCP_MARK_TOOL must be set to 'true', '1', or 'yes' to enable` and never reaches the Slack client — so the write is correctly blocked, but the tool is still listed.

Note the internal contradiction this creates: `TestUnitShouldAddToolEnabledToolsPrecedenceOverFalseyEnv` asserts that `SLACK_MCP_ENABLED_TOOLS` wins over a falsey env var, while the mark handler asserts the opposite — once `SLACK_MCP_MARK_TOOL` is set to anything at all, `SLACK_MCP_ENABLED_TOOLS` is ignored. Both halves are tested in isolation and the combination is tested nowhere, so neither behavior is actually owned.

The fix log records the `SLACK_MCP_ENABLED_TOOLS`-precedence question as consciously deferred with the user, which I accept. The `banana` case is not covered by that deferral: a junk value registering a permanently broken tool is the reported bug with a different input.

Suggestion: `SLACK_MCP_MARK_TOOL` and `SLACK_MCP_ATTACHMENT_TOOL` are the two vars with no channel-list semantics, so for those two `shouldAddTool` can require `text.IsTruthy(v)` rather than merely `!text.IsFalsey(v)`. The channel-scoped vars must keep the `!IsFalsey` form. That needs a per-call distinction rather than a change to the shared helper.

## M2 — `os.Unsetenv` in the new server tests leaks env state past the test

`pkg/server/server_test.go:521` and `:548` call `os.Unsetenv("SLACK_MCP_MARK_TOOL")` directly. In the `envSet: false` subtest and in the second `Precedence` subtest, `t.Setenv` is never called, so no cleanup is registered and the variable stays unset for everything that runs afterwards in the package. The value is also not restored to whatever the surrounding environment had.

The idiom that keeps the restore is to register the cleanup first and then clear:

```go
t.Setenv("SLACK_MCP_MARK_TOOL", "")
os.Unsetenv("SLACK_MCP_MARK_TOOL")
```

The leading `os.Unsetenv` at the top of every `envSet: true` subtest is also redundant — the `t.Setenv` on the next line overwrites it — and it is what defeats the restore in the unset case.

## M3 — the combinations the change actually alters are untested

Three gaps, all cheap to close:

- The `--enabled-tools` flag path (H1). No test exercises enablement via the flag, which is why the regression got through.
- `SLACK_MCP_ENABLED_TOOLS` listing `conversations_mark` *together with* a falsey `SLACK_MCP_MARK_TOOL` (M1). Whichever precedence is chosen, pin it.
- `shouldAddTool` is only exercised with `SLACK_MCP_MARK_TOOL`. The guard changed behavior for `SLACK_MCP_ADD_MESSAGE_TOOL`, `SLACK_MCP_DRAFT_MESSAGE_TOOL`, `SLACK_MCP_REACTION_TOOL`, and `SLACK_MCP_ATTACHMENT_TOOL` too, and none of them has a falsey case. The channel-list subtests guard against an over-broad fix well, but only for one variable.

## M4 — README documents the new semantics for one of the five affected variables

The diff correctly removes the duplicated, self-contradicting `SLACK_MCP_MARK_TOOL` row and the surviving row is accurate. But the falsey guard changed observable behavior for `SLACK_MCP_ADD_MESSAGE_TOOL`, `SLACK_MCP_DRAFT_MESSAGE_TOOL`, `SLACK_MCP_REACTION_TOOL`, and `SLACK_MCP_ATTACHMENT_TOOL` as well — `=false` no longer registers those tools. Nothing in the table tells an operator that `false`/`0`/`no`/`off` are now recognized as explicit disables, or that any *other* non-empty value still registers the tool. A single sentence covering the shared convention, or one clause per affected row, would close this.

## L1 — the attachment gate silently became case-insensitive and whitespace-tolerant

`parseParamsToolFilesGet` went from exact `toolConfig != "true" && != "1" && != "yes"` to `!text.IsTruthy(toolConfig)`, which trims and lowercases. `SLACK_MCP_ATTACHMENT_TOOL=TRUE` previously registered the tool and failed every call; it now works. This is an improvement and it matches the documented contract, and the fix log calls it out — but it is a behavior change outside the reported bug, so it belongs in the PR body as such rather than as an invisible side effect of the refactor. Error strings are unchanged byte-for-byte, which is good.

## L2 — the truthy vocabulary now diverges across three parsers

`text.IsTruthy` accepts `true`/`1`/`yes` (trimmed, case-insensitive). `isChannelAllowedForConfig` (`conversations.go:2170`) accepts only `""`/`"true"`/`"1"`, exact match. `validateToolConfig` (`main.go:255`) carries a third copy of that same allow-set.

The live consequence: `SLACK_MCP_ADD_MESSAGE_TOOL=yes` passes `shouldAddTool` (not falsey → registered), then `isChannelAllowedForConfig` reads `"yes"` as a one-item channel allowlist and denies every real channel. Same for `SLACK_MCP_ADD_MESSAGE_TOOL=" true "`.

This is pre-existing and the fix log deliberately puts `isChannelAllowedForConfig` off-limits — correctly, since changing its allow-set would move channel-allowlist semantics for three tools. Recording it because introducing a shared helper that only two of the five call sites use makes the divergence look like an oversight rather than a decision. Worth its own change.

## L3 — the mark test's panic-based seam is correct but brittle

`conversations_mark_test.go` proves "the call reached the write path" by booting a demo-mode `ApiProvider` whose `Slack()` wraps a nil client and recovering the resulting nil-pointer panic. It never performs a real Slack write, the reasoning is documented at length in the file, and given that `ConversationsMarkHandler` has no client seam it is a defensible choice.

The exposure is that `TestUnitConversationsMarkHandlerGateAllowsWhenEnabled` asserts a panic *does* happen. If `provider.New` ever returns a working fake for the `demo` token, that test inverts and fails for a reason unrelated to the gate. The blocking test is safer — it also pins the error string, so it cannot pass for the wrong reason.

`ConversationsMarkHandler` calls `ch.apiProvider.Slack().MarkConversationContext` directly at `conversations.go:1025`, while the add-message handler goes through `ch.postMessageClient()`. A `markClient()` seam mirroring that would let the test assert on a recorded call instead of a panic. Out of scope for this fix; worth a follow-up.

## L4 — substring matching on the raw `SLACK_MCP_ENABLED_TOOLS` string

`strings.Contains(enabledTools, "conversations_mark")` matches a substring of the raw env value rather than a list element. It is safe today — `ValidateEnabledTools` rejects anything outside `ValidToolNames`, and no valid tool name contains another as a substring — and it mirrors the four sibling gates exactly, which is the right call for consistency. It would silently misfire if a future tool name were a superstring of an existing one. `strings.Split` on `","` plus `slices.Contains` would remove the hazard, ideally for all five gates at once.

## L5 — `IsFalsey`/`IsTruthy` placement

Environment-value predicates in `pkg/text`, a package otherwise about Slack text and markdown processing. The fix log justifies it: `pkg/text` already held the only falsey-aware parser (`IsUnfurlingEnabled`) and imports no internal packages, so neither `pkg/server` nor `pkg/handler` gains a cycle. Reasonable. A `pkg/config` or similar would read better if one ever appears.

`strconv.ParseBool` was correctly not used — it does not accept `yes`/`no`/`on`/`off`, which the documented contract requires. The two helpers being deliberate non-complements (`banana` is neither truthy nor falsey) is right for values that may be channel lists, and both tests pin it.

## Operational — the launchd-pinned binary is currently a dirty branch build

```
$ ./build/slack-mcp-server --version
slack-mcp-server pv-v1.0.1-10-g10bb30c-dirty
built:  2026-08-10T06:57:33Z
```

`build/slack-mcp-server` is the path the local launchd service pins, and `make build` during this work overwrote it with an unreviewed build of the uncommitted branch. The running process still holds the previous binary in memory, so nothing is broken right now, but the pin must be rebuilt from `fork/master` before this work is closed out. The fix log flags this too; repeating it here so it is not lost between documents.

## Confirmed clean

- **`SLACK_MCP_ADD_MESSAGE_MARK` is unaffected.** The auto-mark path at `conversations.go:359-366` calls `MarkConversationContext` directly rather than through `ConversationsMarkHandler`, so it still works when `conversations_mark` is disabled. Correct — it is a separate feature under its own explicit opt-in. (It does mean an operator who set `SLACK_MCP_MARK_TOOL=false` can still cause marks via `SLACK_MCP_ADD_MESSAGE_MARK=true`; that reads as intended, not as a hole.) Note it still uses the inline `== "1" || == "true" || == "yes"` triple — the one truthy check the refactor did not migrate to `text.IsTruthy`.
- **A falsey value cannot grant channel access.** `isChannelAllowedForConfig` allows only on `""`, `"true"`, `"1"`; `"false"` becomes a one-item allowlist matching no channel ID, so it denies. The three channel-scoped tools were checked for the inverted failure and are safe.
- **Error handling matches the file's conventions.** The new gate returns `(nil, error)` like every sibling gate, logs at `Error` with `zap.String("config", …)` on the falsey branch, and does not leak the env value into the returned message.
- **Gate ordering is right.** It runs before `ch.apiProvider.IsReady()`, so a disabled tool fails fast without touching cache-readiness logic.
- **429/retry, tiered rate limiting, cache freshness and isolation, and token-mode gating are untouched.** The `provider.IsBotToken()` / `provider.IsOAuth()` guards at `server.go:239`, `:368`, `:390`, `:621` are ANDed with `shouldAddTool` and are unaffected by the changed branch.
- **Test naming clears the `make test` filter.** All new tests are `TestUnit*`, so `-run=".*Unit.*"` actually runs them. Worth noting separately that the pre-existing `TestShouldAddTool_Matrix` (`server_test.go:485`) does *not* match that filter and has therefore never run under `make test` — it is the test that should have caught this bug in the first place. Renaming it is out of scope here but is the highest-value follow-up in this file.

## Summary

| Severity | Count |
| --- | --- |
| High | 1 |
| Medium | 4 |
| Low | 5 |
| Operational | 1 |

H1 must be fixed before merge — it is a one-line change in `main.go` plus a test. M1 through M4 are worth resolving in this PR. The Low findings are follow-ups.
