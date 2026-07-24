# Fix log — Bug #14: Channel cache silently persists partial lists after mid-pagination failures

- **BUG_ID:** 14 (GitHub issue provectus/slack-mcp-server#14)
- **Branch:** `fix/channel-cache-partial-persist` (from `fork/master`)

## Symptom (normalized)

`channels_list` progressively loses channels after network blips (1805 → 1207 → 740 → 509 → empty). Channel-cache refresh persists a partial channel list when pagination fails mid-way, and that truncated list is written to disk and survives restarts. Later-ordered channels vanish first because `conversations.list` returns a stable order.

## Stages

### fetch-bug — done
Fetched issue #14 via `gh`. Open, no comments, no closing PR. Affected area: `pkg/provider/api.go` (channel cache refresh + pagination). BUG_ID=14.

### resume-detection — done
No prior flow log. Issue OPEN, no merged/closing PR references it. Not already fixed → proceed.

### workspace — done
Tree clean (only untracked `.DS_Store`). `git fetch fork master`; branch `fix/channel-cache-partial-persist` created from `fork/master`.

### diagnose — done (go-mcp-backend, spot-checked by orchestrator)
Root cause confirmed verbatim in `pkg/provider/api.go`:
- `getChannelsMultiType` (1610-1668): on page error logs + `break` (1636-1639), returns partial `chans` with no error. Also `return nil` on rate-limiter failure (1626-1628) — same swallow.
- `GetChannels` (1670-1723): unconditional `channelsSnapshot.Store(newSnapshot)` (1701) built from possibly-partial `chans`; returns `[]Channel` only.
- `fetchAndStoreChannels` (1328-1359): `GetChannels` at 1332 already Stored; len==0 guard (1334) runs after, so in-memory already wiped; nonempty-partial falls through to `atomicWriteFile` (1346) → disk poisoned, survives restart via `refreshChannelsInternal` disk-load (1203).

Surfaces: primary standard path AFFECTED (above). `refreshChannelsInternal` disk-load faithfully reloads poisoned disk (downstream victim). All snapshot readers (channels.go:196 etc.) are clean victims. `GetChannelsType` (1606) inherits signature change, currently dead (no callers). Enterprise edge-supplement loop (523-538) is a *second* silent-partial renderer (breaks on stdErr, returns nil err) — **deliberately out of scope**: it's a best-effort supplement after edge already returned the primary list; propagating its error would convert graceful degradation into a hard failure. `fetchAndStoreChannelMembers` (1511-1532) is the correct `return nil, err` pattern to mirror.

Key handler constraint: `ChannelsHandler` (channels.go:135-141) turns any non-rate-limit `ForceRefreshChannels` error into a hard `channels_list` failure (bails before serving cached data). → Fix must mirror the existing len==0 "keep existing cache, return nil" pattern (1335-1337) when a snapshot survives, not return a hard error.

### classify — done
**No owning spec.** Specs 001-channel-members (channel *members* roster), 002/003/004 unrelated. Channel-list cache refresh + pagination is legacy/cross-cutting, unspecified. Conformance-style fix: fix code + regression test, **no spec amendment** (amend-spec stage skipped).

### fix — done (go-mcp-backend, diff spot-checked)
`pkg/provider/api.go`: `getChannelsMultiType` & `GetChannels` & `GetChannelsType` → `([]Channel, error)`. Pagination error and rate-limiter error now propagate. `GetChannels` returns `nil, err` before any Store on error. `fetchAndStoreChannels`: on error, keep existing snapshot + skip disk write — `return nil` if `channelsReady`, else `fmt.Errorf(... no existing cache ...)`. `GetChannelsType` re-confirmed dead + not on any consumed interface. Enterprise loop 523-538 untouched. `go build ./...` clean.

### regression-test — done (testing-expert; fail→pass independently verified by orchestrator)
New file `pkg/provider/api_channels_partial_test.go`, `TestFetchAndStoreChannelsPartialPagination` with subtests:
- `second_page_error_keeps_existing_snapshot_and_does_not_write_partial_cache`
- `first_page_error_on_cold_start_returns_error_and_leaves_snapshot_unset`
Orchestrator RED check: stashed `pkg/provider/api.go`, both subtests FAIL (partial 2-ch snapshot overwrote good 3-ch; disk written; cold-start error not surfaced). Restored fix → PASS. Evidence recorded.

### verify-criteria — done
Data/control-flow fix, no tool-response/output-path edits, no owning spec → sanctioned evidence = demonstrated failing→passing regression test + unit check of the changed cache data. No full server run needed. Satisfied.

### amend-spec — skipped
No owning spec (conformance). Nothing to amend.

### local-review — done
Static gate: `make test` green, gofmt clean. AI review (code-reviewer subagent) → **APPROVE**, 0 critical / 0 important, 2 sub-threshold informational observations. Review file: `context/review-14.md`. Keep/drop gate (AskUserQuestion): user **accepted both**.
- #1 applied: `getChannelsMultiType` page-error `return chans, err` → `return nil, err`.
- #2 applied: removed dead `GetChannelsType` (grep re-confirmed zero refs incl. tests).
Applied by a fresh agent reading the review file + diff; `make test` re-run green, gofmt clean.

### commit-push — this entry is the log's last committed state
Staging: `pkg/provider/api.go`, `pkg/provider/api_channels_partial_test.go`, `context/fix-log-14.md`, `context/review-14.md`. Excluding `.DS_Store`. Commit `fix(channels): stop persisting partial channel lists on pagination failure` with `Closes #14`. Push `git push -u fork fix/channel-cache-partial-persist`. After the PR opens, remaining stages (remote gates, merge, close) report to the user from remote state — no further writes to this tracked log.
