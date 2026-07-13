# Code Review: 001 — List Channel Members

**Scope reviewed:** working-tree diff against `fork/master` plus the five untracked test files.
Files: `pkg/provider/api.go`, `pkg/handler/channels.go`, `pkg/handler/conversations.go`,
`pkg/server/server.go`, `cmd/slack-mcp-server/main.go`, and the provider/handler tests.

**Verdict:** Approve with changes. The feature builds, `go vet` is clean, and the full
provider/handler/server test suites pass. The primary acceptance criteria (complete roster,
ID/#name parity, name resolution from the users snapshot, include-by-ID fallback, bot/deactivated
filters, empty channel → empty list, per-workspace isolation, restart survival, per-entry TTL
serve-stale, forced-refresh throttle) are implemented and covered by tests. Two Important issues
below concern the rate-limit and concurrency mitigations that the technical spec explicitly promised
but the implementation only partially delivers; both bite exactly the large-channel / concurrent-first-caller
cases the spec's risk analysis called out.

---

## Important (80-89)

### 1. Pagination is neither per-page rate-limited nor per-page retried; a mid-pagination 429 restarts the whole fetch — confidence 82

**Where:** `pkg/provider/api.go` — `fetchAndStoreChannelMembers` (the `limiter.CallWithRetry`
wrapping) together with `MCPSlackClient.GetUsersInConversationContext` (the internal pagination loop).

`fetchAndStoreChannelMembers` wraps the *entire* multi-page fetch in a single
`limiter.CallWithRetry(ctx, ap.rateLimiter, 2, retryAfter, fn)`, where `fn` calls
`ap.client.GetUsersInConversationContext(...)`. That client method (standard path) then loops over
all cursor pages internally, calling `c.slackClient.GetUsersInConversationContext` directly — with
no rate-limiter `Wait` and no retry between pages. Consequences:

- **Under-throttled:** `CallWithRetry` consumes exactly one limiter token before running `fn`, so
  only the first page of a fetch is rate-limited. A 50k-member channel fetches ~50 pages back-to-back
  with no throttling — the precise "rate-limit exposure on large channels" risk the technical spec
  (§3) said it would mitigate by routing "every page through the shared rate limiter."
- **Retry restarts pagination:** the inner loop returns a 429 immediately (no per-page retry), so the
  error propagates to `CallWithRetry`, which re-invokes `fn` from page 1 (cursor reset), re-fetching
  every already-collected page. With `maxRetries = 2`, a large channel that 429s on a late page a few
  times can fail the whole gather rather than resuming.

The technical spec (§2.2 / §2.6 / §3) states the intent explicitly: "route every page through the
shared rate limiter wrapped in the retry layer (429 → `RetryAfter`)." The current wiring wraps the
wrong granularity.

Note the existing tests do not catch this: `TestChannelMembers429Retry` injects the mock *as*
`ap.client`, so the single-shot mock replaces the paginating `MCPSlackClient` entirely — the real
"429 during multi-page pagination" interaction is never exercised. The httptest multi-page test
(`TestChannelMembersColdFetchMultiPage`) never injects a 429.

**Suggested fix:** move the limiter+retry inside the pagination loop (throttle/retry each
`conversations.members` page individually and continue from the current cursor on a retryable error),
e.g. have `MCPSlackClient.GetUsersInConversationContext` take the limiter, or paginate in
`fetchAndStoreChannelMembers` itself wrapping each page call in `CallWithRetry`. Add a test that
returns a `RateLimitedError` on an intermediate page and asserts the roster still completes without
re-fetching earlier pages.

### 2. First-run gather is unguarded, so the concurrent-duplicate-fetch mitigation and the "still preparing" not-ready path are unreachable in production — confidence 80

**Where:** `pkg/provider/api.go` — `GetChannelMembers` (absent-entry branch) and
`spawnBackgroundMembersRefresh`.

On a cache miss with no in-flight gather, `GetChannelMembers` calls `fetchAndStoreChannelMembers`
**synchronously without registering the channel in `refreshingMembers`**. The only writer of
`refreshingMembers` is `spawnBackgroundMembersRefresh`, which is only called from the *expired-entry*
(serve-stale) branch — i.e. only when an entry is already present. Two consequences that diverge from
the technical spec:

- **Concurrent duplicate gathers are not prevented.** Two simultaneous first-time requests for the
  same channel both observe `refreshingMembers.Load == false` and both launch a full synchronous
  Slack fetch. The technical spec (§3, "Concurrent duplicate gathers") promises "the second caller
  gets `ErrChannelMembersNotReady` rather than launching a duplicate fetch" via the `LoadOrStore`
  guard — that guard is never applied to the first-run path.
- **The `ErrChannelMembersNotReady` / "still preparing, please retry shortly" path is dead code in
  production.** It fires only when an entry is *absent* AND the channel is in `refreshingMembers`, a
  combination that can never arise in production (the map is only ever populated for
  present-but-expired entries). The functional-spec AC "Given a channel whose roster has never been
  gathered and a background gather is in progress … receives a short 'still preparing, please retry
  shortly' message" is therefore not genuinely satisfiable — a first-time caller always blocks on a
  synchronous fetch and gets the full roster instead.

The tests that "cover" this (`TestChannelsMembersHandlerNotReadySentinel`,
`TestChannelMembersConcurrentGuard`) rely on `MarkMembersRefreshInFlightForTest` /
`refreshingMembers.Store(...)` to inject a state the real code paths cannot produce, so they validate
the sentinel translation but not a reachable end-to-end scenario.

**Suggested fix:** register the channel in `refreshingMembers` via `LoadOrStore` at the top of the
first-run path in `GetChannelMembers` (deferring the delete), so a concurrent first-time caller
receives `ErrChannelMembersNotReady` — matching the spec's stated mitigation and making the AC path
reachable. Add a genuinely concurrent test (two goroutines, no manual `Store`) asserting exactly one
Slack fetch and one `ErrChannelMembersNotReady`.

---

## Lower-confidence observations (below the 80 reporting threshold; informational)

- **Cache write failure is swallowed (confidence ~55).** `fetchAndStoreChannelMembers` logs but does
  not propagate `atomicWriteFile` / marshal errors, returning `ids, nil`. The in-memory snapshot is
  still updated so the current request succeeds; only restart survival is lost for that entry. This
  mirrors the serve-stale posture of the existing caches and is likely intentional.
- **`edgeFailed` is written without synchronization** in `MCPSlackClient.GetUsersInConversationContext`
  (confidence ~30). This is a pre-existing pattern copied verbatim from `GetConversationsContext`, as
  the technical spec directed, so it is not introduced by this change.
- **Handler `IsReady` failure returns a Go error rather than a text result** (confidence ~20). This
  matches every other handler in the codebase and is the established convention.

---

## Verified as correct

- Tool registration: `channels_members` added to `ValidToolNames`, registered unconditionally via
  `shouldAddTool(..., "")` (read-only, no write-gating), consistent with `channels_list`; server_test
  updated. Correct per token-mode gating (`conversations.members` works across xoxp/xoxb/xoxc+xoxd).
- `resolveChannelID` cleanly extracted to a shared free function; `ConversationsHandler` delegates to
  it with identical behavior (raw IDs pass through; `#`/`@` refs go through `ChannelsInv` with a single
  `ForceRefreshChannels` retry; clear not-found message). Handler returns the not-found text as a clean
  tool result, not a protocol error.
- Copy-on-write snapshot discipline: `fetchAndStoreChannelMembers` builds a fresh map and
  `atomic.Pointer.Store`s it under `fetchMembersMu`; readers never mutate the immutable snapshot.
- Per-entry TTL (`time.Since(roster.FetchedAt) > cacheTTL`), `TTL <= 0` disables expiry, serve-stale
  spawns a background refresh — matches the users/channels model adapted to per-channel granularity.
- Forced-refresh throttle is TOCTOU-safe (single `membersMu` scope for check-and-set), keyed per
  channel, returns `ErrRefreshRateLimited`; handler tolerates it (logs + serves cached).
- Empty roster is stored as a valid entry (not treated as "keep old cache"), satisfying the empty/
  hidden-channel AC; handler include-by-ID fallback and bot/deactivated filters read from
  `ProvideUsersMap()` as specified; CSV columns are exactly `UserID,DisplayName,RealName`.
- Boot load (`RefreshChannelMembers`) is Slack-call-free and tolerant of a missing/corrupt file;
  wired into the same boot path as users/channels for restart survival; per-workspace file path via
  `getCachePathWithTeamID` / `SLACK_MCP_CHANNEL_MEMBERS_CACHE`.
