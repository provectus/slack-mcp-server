# Technical Specification: List Channel Members

- **Functional Specification:** [functional-spec.md](./functional-spec.md)
- **Status:** Draft
- **Author(s):** AWOS implement-feature flow (drafted with the `go-mcp-backend` specialist)

---

## 1. High-Level Technical Approach

Add a new read-only MCP tool, `channels_members`, that returns the complete member roster of a single channel (identified by ID or `#name`/`@name`), each row carrying the member's user ID, display name, and real name. Names are resolved against the existing users snapshot (`ApiProvider.ProvideUsersMap()`), so they stay consistent with every other tool. Optional `exclude_bots` / `exclude_deactivated` flags filter the roster using `slack.User.IsBot` and `slack.User.Deleted` from that same snapshot.

The roster is backed by a **third cache tier** that mirrors the existing `UsersCache` and `ChannelsCache` machinery in `pkg/provider/api.go`: an `atomic.Pointer[...]` immutable snapshot, a per-workspace (TeamID-namespaced) JSON file written via `atomicWriteFile`, TTL-based staleness, background-refresh guarded by an in-flight set, forced refresh throttled by `minRefreshInterval` returning `ErrRefreshRateLimited`, and a "not ready yet" sentinel message.

The one structural departure — the central design decision — is that rosters are **per-channel**, not a single global snapshot. We keep a **single persisted map** `channelID -> {memberIDs, fetchedAt}` inside one workspace-scoped file, with **per-entry TTL** (each roster's own `FetchedAt`) rather than the file-mtime TTL that users/channels use. This preserves per-workspace isolation and single-atomic-write semantics while letting each channel's roster age and refresh independently.

The underlying Slack call is `conversations.members`. The standard slack-go client already exposes `GetUsersInConversationContext`, and the edge client already implements the same signature (`pkg/provider/edge/slacker.go`), but **neither is yet in the `SlackAPI` interface** — it must be added, with an `MCPSlackClient` method dispatching edge-vs-standard following the existing `GetConversationsContext` fallback pattern.

---

## 2. Proposed Solution & Implementation Plan (The "How")

### 2.1 New tool: registration, params, gating (`pkg/server/server.go`)

- New constant `ToolChannelsMembers = "channels_members"`, appended to `ValidToolNames`.
- Registered via `shouldAddTool(ToolChannelsMembers, enabledTools, "")` — read-only, no write-gating env var. `conversations.members` works with `xoxp`, `xoxb`, and `xoxc/xoxd` (standard API for OAuth/bot; edge fallback for Enterprise + session), so it registers **unconditionally** like `channels_list`.
- Tool definition mirrors `channels_list`: `mcp.NewTool(ToolChannelsMembers, WithReadOnlyHintAnnotation(true), ...)` with params:
  - `channel_id` (string, required) — `Cxxxxxxxxxx` or `#name` / `@username_dm`.
  - `exclude_bots` (bool, default false).
  - `exclude_deactivated` (bool, default false).
  - `refresh_cache` (bool, default false).
- Handler `ChannelsMembersHandler` lives on the existing `ChannelsHandler` in `pkg/handler/channels.go`, reusing its `apiProvider`/`logger`.

### 2.2 Slack method + interface change (`pkg/provider/api.go`)

Add to the `SlackAPI` interface (near the conversations methods):

```go
GetUsersInConversationContext(ctx context.Context, params *slack.GetUsersInConversationParameters) ([]string, string, error)
```

Implement on `MCPSlackClient` with the same edge-first-then-standard dispatch as `GetConversationsContext`: for `isEnterprise && !isOAuth` (and `!edgeFailed`) call `c.edgeClient.GetUsersInConversationContext`, falling back to the standard client on error; otherwise call the standard client directly. The standard-client path must **paginate** (loop on the returned `nextCursor`, appending IDs) since `conversations.members` returns cursor-paginated pages — the roster must be complete. (The edge implementation already returns the full list via `UsersList`.)

### 2.3 New cache type (per-channel roster in one file)

```go
type ChannelMemberRoster struct {
    MemberIDs []string  `json:"member_ids"`
    FetchedAt time.Time `json:"fetched_at"`
}

type ChannelMembersCache struct {
    Channels map[string]ChannelMemberRoster `json:"channels"` // keyed by channel ID
}
```

**Cache-shape justification (single file vs per-channel files):** one workspace-scoped file holding the whole `channelID -> roster` map is chosen over per-channel files because it (a) keeps the exact per-workspace isolation model already in place (`getCachePathWithTeamID`), (b) needs only one `atomicWriteFile` temp-file+rename per update — per-channel files would multiply temp/rename churn and orphan files for deleted channels, and (c) loads on boot in one read. The trade-off — a whole-map rewrite on each channel's refresh — is acceptable because rosters are ID-only and small, and writes are serialized under a fetch mutex.

**Per-channel TTL:** the members file holds many rosters of differing ages, so staleness is computed **per entry** as `time.Since(roster.FetchedAt) > ap.cacheTTL` using the same `getCacheTTL()` value (`SLACK_MCP_CACHE_TTL`).

### 2.4 `ApiProvider` fields (mirror users/channels block)

| Field | Type | Purpose |
|---|---|---|
| `membersSnapshot` | `atomic.Pointer[ChannelMembersCache]` | Lock-free immutable snapshot. |
| `membersCachePath` | `string` | Per-workspace file path. |
| `refreshingMembers` | `sync.Map` (channelID → struct{}) | Per-channel in-flight gather guard (replaces the single `atomic.Bool`). |
| `lastForcedMembersRefresh` | `map[string]time.Time` | Per-channel forced-refresh throttle. |
| `membersMu` | `sync.RWMutex` | Protects `lastForcedMembersRefresh` + snapshot swaps. |
| `fetchMembersMu` | `sync.Mutex` | Serializes disk writes. |

Per-channel granularity is why `refreshingUsers`/`refreshingChannels` (single `atomic.Bool`) becomes a keyed set here and `lastForcedUsersRefresh` (single `time.Time`) becomes a per-channel map.

Init in both `newWithXOXP` and `newWithXOXC`: resolve `membersCachePath` from `SLACK_MCP_CHANNEL_MEMBERS_CACHE` or `getCachePathWithTeamID(teamID, "channel_members_cache.json")`, and store an empty `&ChannelMembersCache{Channels: map[string]ChannelMemberRoster{}}` snapshot.

### 2.5 Sentinel + not-ready

```go
const channelMembersNotReadyMsg = "channel members cache is not ready yet, sync process is still running... please wait"
var ErrChannelMembersNotReady = errors.New(channelMembersNotReadyMsg)
```

### 2.6 Provider methods

- **Load-on-boot:** `RefreshChannelMembers(ctx)` (no channel arg) reads `membersCachePath`, `json.Unmarshal` into `ChannelMembersCache`, stores the snapshot. Called from the same boot path as `RefreshUsers`/`RefreshChannels` — gives restart survival with no Slack call.
- **Read path:** `GetChannelMembers(ctx, channelID) ([]string, error)`:
  1. Load snapshot; entry present and **not** per-entry-expired → return `roster.MemberIDs`.
  2. Entry present but expired → return current IDs and spawn a background refresh for that channel (serve-stale, like `refreshUsersInternal`).
  3. Entry absent: if a gather for this `channelID` is already in `refreshingMembers` → return `ErrChannelMembersNotReady`; otherwise perform a **synchronous** `fetchAndStoreChannelMembers` (first-run returns the complete roster, mirroring users/channels).
- **Background gather:** `spawnBackgroundMembersRefresh(channelID)` uses `refreshingMembers.LoadOrStore(channelID, …)` as the per-channel CAS guard, `defer`-deletes the key, and calls `fetchAndStoreChannelMembers`.
- **Fetch/store:** `fetchAndStoreChannelMembers(ctx, channelID)` — paginate `GetUsersInConversationContext`, build the roster with `FetchedAt: time.Now()`, copy-on-write a new `ChannelMembersCache` map (never mutate the immutable snapshot), `atomicWriteFile` the whole map under `fetchMembersMu`. An empty result is a **valid** roster (spec: empty channel → empty list, not an error), so unlike users/channels we do not treat zero results as "keep old cache".
- **Forced refresh:** `ForceRefreshChannelMembers(ctx, channelID)` throttled via `lastForcedMembersRefresh[channelID]` + `minRefreshInterval`, returning `ErrRefreshRateLimited` within the window (same TOCTOU-safe lock pattern as `ForceRefreshUsers`).

### 2.7 Handler flow (`ChannelsMembersHandler`)

1. `IsReady()` gate — needs users + channels ready for name resolution and channel-name lookup.
2. Resolve the channel reference to an ID via a helper equivalent to `resolveChannelID` (currently on `ConversationsHandler`, `conversations.go:1664` — extract/share it): `#`/`@`-prefixed refs go through `ChannelsInv` with a single `ForceRefreshChannels` retry; a miss returns a clear "channel not found" message.
3. If `refresh_cache` → `ForceRefreshChannelMembers(ctx, id)`, tolerating `ErrRefreshRateLimited` (log + serve cached), matching `ChannelsHandler`.
4. `GetChannelMembers(ctx, id)`; on `ErrChannelMembersNotReady` return `mcp.NewToolResultText(channelMembersNotReadyMsg)` (clean text, not a protocol error).
5. Resolve names + filter: for each member ID look up `ProvideUsersMap().Users[id]`; skip when `exclude_bots && u.IsBot` or `exclude_deactivated && u.Deleted`. A member missing from the users snapshot is **still included** by ID with blank names (spec AC); no mass `PatchUser` calls (bounds API cost).
6. Marshal the **complete** roster (no pagination helpers) to CSV via `gocsv.MarshalBytes` and return `mcp.NewToolResultText`.

CSV row struct in `pkg/handler/channels.go`:

```go
type ChannelMember struct {
    UserID      string `csv:"UserID"`
    DisplayName string `csv:"DisplayName"` // u.Profile.DisplayName
    RealName    string `csv:"RealName"`    // u.RealName
}
```

### 2.8 New / changed files

- `pkg/provider/api.go` — new types, `ApiProvider` fields, sentinel/error, provider methods, `SlackAPI` interface addition, `MCPSlackClient.GetUsersInConversationContext`, snapshot init + boot load in `newWithXOXP`/`newWithXOXC`.
- `pkg/handler/channels.go` — `ChannelMember` struct, `ChannelsMembersHandler`, shared channel-resolution.
- `pkg/server/server.go` — `ToolChannelsMembers` const, `ValidToolNames` entry, registration block.
- `SLACK_MCP_CHANNEL_MEMBERS_CACHE` documented alongside `SLACK_MCP_USERS_CACHE`/`SLACK_MCP_CHANNELS_CACHE` in README / `.env` docs.

---

## 3. Impact and Risk Analysis

### System Dependencies

- **`SlackAPI` interface change** ripples to every implementer and to test mocks/fakes — the highest-touch change; must be reflected in mock types used by provider/handler tests.
- **Users snapshot** — name resolution and bot/deactivated filtering depend on `ProvideUsersMap()`; a cold/partial users cache degrades name quality (handled by include-by-ID fallback) but never blocks the roster.
- **Channels snapshot** — `#name`/`@name` resolution depends on `ChannelsInv`; ID input bypasses this.
- **Edge vs standard dispatch** — reuses the established `edgeFailed` fallback; no new auth surface.

### Potential Risks & Mitigations

- **Rate-limit exposure on large channels.** `conversations.members` is cursor-paginated and rate-limited; a 50k-member channel is many sequential pages returning HTTP 429s. *Mitigation:* route every page through the shared rate limiter wrapped in the retry layer (429 → `RetryAfter`), request max page size to minimize round-trips; per-channel TTL means the expensive gather happens once per TTL, not per request; `minRefreshInterval` throttles `refresh_cache` abuse.
- **Cache-file growth for many channels.** One entry (ID list + timestamp) per queried channel; a workspace where every channel is queried yields a large single JSON file rewritten on each refresh. *Mitigation:* rosters store IDs only (no profiles); entries created lazily on demand; whole-map atomic rewrite bounded by ID-only payloads. (Eviction of long-untouched entries is possible but out of scope.)
- **Workspace isolation.** Same guarantee as existing caches — file is `TeamID`-prefixed via `getCachePathWithTeamID`; channel IDs live under a workspace-scoped file, so no cross-workspace collision.
- **Stale rosters.** Membership changes are not seen until per-entry TTL expiry or explicit `refresh_cache`. *Mitigation:* per-channel `FetchedAt` TTL triggers transparent background refresh on next read (serve-stale); `refresh_cache` forces an immediate re-fetch.
- **Concurrent duplicate gathers.** Two simultaneous first-time requests for the same channel. *Mitigation:* `refreshingMembers` per-channel `LoadOrStore` guard — the second caller gets `ErrChannelMembersNotReady` rather than launching a duplicate fetch.
- **Copy-on-write correctness.** Snapshot must never be mutated in place (readers hold it lock-free). *Mitigation:* build a fresh `ChannelMembersCache` map on every update and `atomic.Pointer.Store`, as `PatchUser`/`mergeChannelCounts` do.

---

## 4. Testing Strategy

Mirror the existing testify-based handler/provider tests (`stretchr/testify`), mocking the `SlackAPI` interface (extended with `GetUsersInConversationContext`) and driving the provider/handler directly.

**Mocks / fixtures:**
- `SlackAPI` mock returns scripted `GetUsersInConversationContext` pages (incl. a multi-page cursor sequence) and errors (incl. a `slack.RateLimitedError`).
- Pre-seed `usersSnapshot` with normal users, an `IsBot: true` user, a `Deleted: true` user, and an ID present in the roster but **absent** from the users map.
- Pre-seed `channelsSnapshot`/`ChannelsInv` for `#name` resolution.
- Use a temp `membersCachePath` (`t.TempDir`) to assert atomic write + reload.

**Provider unit tests (`pkg/provider`):**
- Cold fetch stores a complete roster (multi-page pagination assembled) and writes the file atomically.
- Read hit within TTL serves from snapshot with **no** Slack call (assert mock call count 0).
- Per-entry TTL expiry serves stale IDs and spawns a background refresh (older entry replaced, `FetchedAt` advanced).
- Restart survival: provider pointing at a pre-written file, `RefreshChannelMembers` boot load, then read serves from disk with no Slack call.
- Empty channel returns an empty roster (not an error), cached as a valid entry.
- `ForceRefreshChannelMembers` within `minRefreshInterval` returns `ErrRefreshRateLimited`; after the window, re-fetches.
- Concurrent first-time gathers: one fetch runs, the other returns `ErrChannelMembersNotReady`.
- 429 handling: mock returns `RateLimitedError` then success; assert retry and roster completes.
- Workspace isolation: two TeamIDs produce two distinct files; entries don't cross.

**Handler unit tests (`pkg/handler`):**
- ID input vs `#name` input yield identical rosters (resolution parity).
- Unknown channel reference → clear "not found" result, no partial list.
- Default (no filters) includes bot + deactivated + normal + unknown-by-ID members; CSV has `UserID,DisplayName,RealName` columns; unknown-map member appears with blank names.
- `exclude_bots` omits only the bot; `exclude_deactivated` omits only the deleted user; both set → only active humans.
- Not-ready path: roster absent and gather in flight → handler returns the `channelMembersNotReadyMsg` text result.
- `refresh_cache=true` triggers `ForceRefreshChannelMembers`; rate-limited refresh logs and serves cached data rather than erroring.
- Complete-roster assertion: a >1000-member scripted channel returns every member (no truncation).
