# Design: `conversations_draft_message` tool

**Date:** 2026-06-03
**Status:** Approved (pending user spec review)
**Branch:** `feat/refresh-cache-channels-list`

## Summary

Add a new MCP tool, `conversations_draft_message`, that creates a **native Slack
draft** — a message saved to the user's Slack "Drafts" list, editable in the Slack
UI, that is **never sent automatically**. It is the safe counterpart to
`conversations_add_message` (which posts immediately).

Native drafts are not exposed by Slack's public Web API. They are created through
Slack's undocumented `drafts.create` edge endpoint, which requires a browser/session
token (`xoxc`/`xoxd`). It does **not** work with bot tokens (`xoxb`).

## Goals

- Create a native Slack draft in a channel, DM, or thread.
- Mirror `conversations_add_message`'s input surface for consistency.
- Reuse existing patterns: edge form/PostForm/ParseResponse, markdown→blocks
  conversion, channel resolution, channel-allow policy, CSV output.

## Non-goals (YAGNI)

- Multi-destination drafts (draft to several channels at once).
- Editing, listing, or deleting existing drafts.
- Scheduled messages (`chat.scheduleMessage`) — explicitly rejected during
  brainstorming; that auto-sends and is a different feature.
- File attachments in drafts.

## Input surface

Mirrors `conversations_add_message`:

| Param          | Required | Notes |
|----------------|----------|-------|
| `channel_id`   | yes      | `Cxxxxxxxxxx`, `#channel`, or `@user_dm`. Resolved via `resolveChannelID`. |
| `thread_ts`    | no       | If set, draft is a threaded reply. Validated to contain `.`. |
| `text`         | yes      | Message body in `content_type` format. |
| `content_type` | no       | `text/markdown` (default) or `text/plain`. |

## Architecture — 4 layers

### 1. Edge API call — new file `pkg/provider/edge/drafts.go`

Follows the existing edge pattern (see `conversations.go`
`ConversationsGenericInfo`/`ConversationsView`):

```go
func (cl *Client) DraftsCreate(
    ctx context.Context,
    channelID, threadTs string,
    blocks json.RawMessage,
) (draftID string, err error)
```

- `draftsCreateForm{ BaseRequest; ...; WebClientFields }` built and submitted via
  `cl.PostForm(ctx, "drafts.create", values(form, true))`.
- Response parsed via `cl.ParseResponse(&r, resp)` into a struct embedding
  `baseResponse` plus the draft identifier.

**Payload fields** (Slack web client `drafts.create`):

- `blocks` — JSON-encoded rich-text block array (the converted message).
- `client_msg_id` — a generated UUID.
- `destinations` — JSON array, e.g. `[{"channel_id":"C…","thread_ts":""}]`
  (single destination; `thread_ts` empty unless replying in a thread).
- `file_ids` — empty array.
- `is_from_composer` — `false`.
- standard `WebClientFields` (`_x_reason`, etc.) via `webclientReason(...)`.

### 2. Provider wiring — `pkg/provider/api.go`

- Add to the `SlackAPI` interface, in the "Edge API methods" block:
  ```go
  DraftsCreate(ctx context.Context, channelID, threadTs string, blocks json.RawMessage) (string, error)
  ```
- Implement on `MCPSlackClient`, delegating to `c.edgeClient.DraftsCreate(...)`
  (same shape as the existing `UsersSearch` delegation).

### 3. Handler — `pkg/handler/conversations.go`

`ConversationsDraftMessageHandler` mirrors `ConversationsAddMessageHandler`:

1. `ch.apiProvider.IsReady()` guard.
2. `parseParamsToolDraftMessage(ctx, request)`.
3. Build blocks: for `text/markdown`, use
   `slackGoUtil.ConvertMarkdownTextToBlocks(text)`; on parse error, fall back to a
   plain-text block (same fallback as add-message). For `text/plain`, a plain-text
   block. Marshal blocks to `json.RawMessage`.
4. `ch.apiProvider.Slack().DraftsCreate(ctx, channel, threadTs, blocks)`.
5. Return a small confirmation as CSV (the output format the other tools use):
   columns `draft_id, channel_id, thread_ts`. Drafts are not in channel history, so
   there is nothing to re-fetch.

`parseParamsToolDraftMessage` mirrors `parseParamsToolAddMessage`:

- Enable guard via `SLACK_MCP_DRAFT_MESSAGE_TOOL` (see Enable guard below).
- `channel_id` required → `resolveChannelID`.
- Channel-allow policy via `isChannelAllowedForConfig(channel, os.Getenv("SLACK_MCP_DRAFT_MESSAGE_TOOL"))`.
- `thread_ts` optional, must contain `.` if present.
- `text` required.
- `content_type` ∈ {`text/markdown`, `text/plain`}.

### 4. Tool registration — `pkg/server/server.go`

- Const `ToolConversationsDraftMessage = "conversations_draft_message"`, added to
  `ValidToolNames`.
- Registered only when **both**:
  - `!provider.IsBotToken()` — edge needs a session token (same guard the search
    tool uses); and
  - `shouldAddTool(ToolConversationsDraftMessage, enabledTools, "SLACK_MCP_DRAFT_MESSAGE_TOOL")`.
- Tool definition mirrors `conversations_add_message`'s params (`channel_id`,
  `thread_ts`, `text`, `content_type`).
- Annotations: `WithTitleAnnotation("Draft Message")`. **Not** marked destructive —
  drafting sends nothing.

## Enable guard

New dedicated env var **`SLACK_MCP_DRAFT_MESSAGE_TOOL`**, using the same
channel-allowlist syntax as `SLACK_MCP_ADD_MESSAGE_TOOL`
(`true` | `1` | comma-separated channel IDs | `!Cxxxx` negation). This lets users
enable safe drafting **without** enabling auto-posting. Disabled by default, with a
helpful error message (mirroring the add-message disabled message) when called while
unset.

## Token-type behavior

- `xoxc`/`xoxd` (session): tool registered and functional.
- `xoxb`/`xoxp` (bot/OAuth): tool **not registered** (edge client unavailable), same
  as the search tool. No partial/broken behavior.

## Testing

- `parseParamsToolDraftMessage` unit tests: channel resolution, `thread_ts`
  validation, `content_type` validation, enable-guard/allowlist behavior.
- Handler test using a mock `SlackAPI` that implements `DraftsCreate`, asserting the
  blocks payload and confirmation output.

## Key risk

`drafts.create` is **undocumented**. The exact request field names and response
shape will be verified during implementation against known edge-client
implementations (e.g. slackdump) before finalizing the form struct and tests. If the
shape cannot be confidently verified, implementation stops and the risk is flagged
rather than shipping a guess.
