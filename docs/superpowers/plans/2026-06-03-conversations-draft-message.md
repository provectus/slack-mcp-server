# conversations_draft_message Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `conversations_draft_message` MCP tool that creates a native Slack draft (saved to the user's Drafts list, never auto-sent) via Slack's undocumented `drafts.create` edge endpoint.

**Architecture:** Four layers, mirroring the existing `conversations_add_message` flow — (1) an edge API method `DraftsCreate` in `pkg/provider/edge/drafts.go`; (2) provider wiring through the `SlackAPI` interface and `MCPSlackClient`; (3) a handler + param parser in `pkg/handler/conversations.go` reusing the markdown→blocks conversion, channel resolution and channel-allow policy; (4) tool registration in `pkg/server/server.go`, gated behind `!IsBotToken()` and a new `SLACK_MCP_DRAFT_MESSAGE_TOOL` env var. Drafts are session-token-only (`xoxc`/`xoxd`).

**Tech Stack:** Go, `mark3labs/mcp-go`, `rusq/slack` (edge client), `slack-go/slack` + `takara2314/slack-go-util` (markdown→blocks, handler side), `google/uuid`, `gocarina/gocsv`, `stretchr/testify`.

**Spec:** `docs/superpowers/specs/2026-06-03-conversations-draft-message-design.md`

---

## File Structure

| File | Responsibility | Action |
|------|----------------|--------|
| `pkg/provider/edge/drafts.go` | `drafts.create` edge call + destinations helper | Create |
| `pkg/provider/edge/drafts_test.go` | Unit tests for destinations + form serialization | Create |
| `pkg/provider/api.go` | `SlackAPI` interface + `MCPSlackClient` delegation | Modify |
| `pkg/handler/conversations.go` | `draftMessageParams`, parser, handler, allow-check, confirmation CSV | Modify |
| `pkg/handler/conversations_test.go` | Unit test for draft channel-allow wrapper | Modify |
| `pkg/server/server.go` | Tool constant, `ValidToolNames`, registration | Modify |
| `README.md` | Tool docs + env-var quick reference | Modify |
| `docs/03-configuration-and-usage.md` | Tool list, env-var table, write-tool section, example | Modify |

---

## ⚠️ Pre-implementation checkpoint: verify the `drafts.create` payload

`drafts.create` is undocumented. **Before writing Task 1's implementation**, verify the request field names and response shape against a reference implementation (e.g. the `slackdump`/`rusq/slack` edge client, or a captured browser request to `https://<workspace>.slack.com/api/drafts.create`).

The plan below uses the best-known web-client shape:
- Request (form-encoded): `token`, `client_msg_id` (UUID), `blocks` (JSON string), `destinations` (JSON string), `file_ids` (`[]`), `is_from_composer` (false), plus `_x_*` web-client fields.
- Response: `{ "ok": true, "draft_id": "..." }` or `{ "ok": true, "draft": { "id": "..." } }`.

**If the verified shape differs**, adjust the `draftsCreateForm`/`draftsCreateResponse` structs and the corresponding test expectations in Task 1 to match, then proceed. **If the shape cannot be verified at all, STOP and report** — do not ship a guessed endpoint.

Record the outcome (verified as-is / adjusted / blocked) in the Task 1 commit message.

---

## Task 1: Edge `drafts.create` method

**Files:**
- Create: `pkg/provider/edge/drafts.go`
- Test: `pkg/provider/edge/drafts_test.go`

- [ ] **Step 1: Write the failing test for the destinations helper**

Create `pkg/provider/edge/drafts_test.go`:

```go
package edge

import (
	"encoding/json"
	"net/url"
	"testing"
)

func TestBuildDraftDestinations(t *testing.T) {
	t.Run("channel only omits empty thread_ts", func(t *testing.T) {
		got, err := buildDraftDestinations("C123", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := `[{"channel_id":"C123"}]`
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("includes thread_ts when set", func(t *testing.T) {
		got, err := buildDraftDestinations("C123", "1700000000.000100")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := `[{"channel_id":"C123","thread_ts":"1700000000.000100"}]`
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

func TestDraftsCreateFormValues(t *testing.T) {
	form := draftsCreateForm{
		BaseRequest:    BaseRequest{Token: "xoxc-test"},
		ClientMsgID:    "11111111-1111-1111-1111-111111111111",
		Blocks:         `[{"type":"rich_text"}]`,
		Destinations:   `[{"channel_id":"C123"}]`,
		FileIDs:        "[]",
		IsFromComposer: false,
		WebClientFields: webclientReason(""),
	}
	v := values(form, true)
	for _, key := range []string{"token", "client_msg_id", "blocks", "destinations", "file_ids"} {
		if v.Get(key) == "" {
			t.Errorf("expected form key %q to be set, got empty (all: %v)", key, url.Values(v))
		}
	}
	if v.Get("blocks") != `[{"type":"rich_text"}]` {
		t.Errorf("blocks not passed through verbatim: %q", v.Get("blocks"))
	}
	// destinations must be valid JSON
	var dest []map[string]any
	if err := json.Unmarshal([]byte(v.Get("destinations")), &dest); err != nil {
		t.Errorf("destinations not valid JSON: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /Users/aleksandrmakarov/code/slack-mcp-server && go test ./pkg/provider/edge/ -run 'TestBuildDraftDestinations|TestDraftsCreateFormValues' -v`
Expected: FAIL — compile error, `buildDraftDestinations` and `draftsCreateForm` undefined.

- [ ] **Step 3: Write the implementation**

Create `pkg/provider/edge/drafts.go`:

```go
package edge

import (
	"context"
	"encoding/json"
	"runtime/trace"

	"github.com/google/uuid"
)

// drafts.* API (undocumented web-client endpoint)

type draftDestination struct {
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts,omitempty"`
}

// buildDraftDestinations builds the JSON `destinations` payload for a single
// target channel, optionally scoped to a thread.
func buildDraftDestinations(channelID, threadTs string) (string, error) {
	b, err := json.Marshal([]draftDestination{{ChannelID: channelID, ThreadTS: threadTs}})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type draftsCreateForm struct {
	BaseRequest
	ClientMsgID    string `json:"client_msg_id"`
	Blocks         string `json:"blocks"`
	Destinations   string `json:"destinations"`
	FileIDs        string `json:"file_ids"`
	IsFromComposer bool   `json:"is_from_composer"`
	WebClientFields
}

type draftsCreateResponse struct {
	baseResponse
	DraftID string `json:"draft_id"`
	Draft   struct {
		ID string `json:"id"`
	} `json:"draft"`
}

func (r draftsCreateResponse) draftID() string {
	if r.DraftID != "" {
		return r.DraftID
	}
	return r.Draft.ID
}

// DraftsCreate creates a native Slack draft in channelID (optionally threaded
// under threadTs) with the given pre-rendered block kit JSON. Returns the new
// draft's identifier. Requires a session token (xoxc/xoxd); bot tokens cannot
// reach the edge API.
func (cl *Client) DraftsCreate(ctx context.Context, channelID, threadTs string, blocks json.RawMessage) (string, error) {
	ctx, task := trace.NewTask(ctx, "DraftsCreate")
	defer task.End()
	trace.Logf(ctx, "params", "channelID=%v threadTs=%v", channelID, threadTs)

	dest, err := buildDraftDestinations(channelID, threadTs)
	if err != nil {
		return "", err
	}

	form := draftsCreateForm{
		BaseRequest:     BaseRequest{Token: cl.token},
		ClientMsgID:     uuid.NewString(),
		Blocks:          string(blocks),
		Destinations:    dest,
		FileIDs:         "[]",
		IsFromComposer:  false,
		WebClientFields: webclientReason(""),
	}

	resp, err := cl.PostForm(ctx, "drafts.create", values(form, true))
	if err != nil {
		return "", err
	}

	var r draftsCreateResponse
	if err := cl.ParseResponse(&r, resp); err != nil {
		return "", err
	}
	if err := r.validate("drafts.create"); err != nil {
		return "", err
	}
	return r.draftID(), nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd /Users/aleksandrmakarov/code/slack-mcp-server && go test ./pkg/provider/edge/ -run 'TestBuildDraftDestinations|TestDraftsCreateFormValues' -v`
Expected: PASS (both tests, all subtests).

- [ ] **Step 5: Build the package**

Run: `cd /Users/aleksandrmakarov/code/slack-mcp-server && go build ./pkg/provider/edge/`
Expected: no output (success).

- [ ] **Step 6: Commit**

```bash
cd /Users/aleksandrmakarov/code/slack-mcp-server
git add pkg/provider/edge/drafts.go pkg/provider/edge/drafts_test.go
git commit -m "feat(edge): add drafts.create API method for native Slack drafts"
```
Include the endpoint-verification outcome (verified/adjusted) in the commit body.

---

## Task 2: Provider wiring

**Files:**
- Modify: `pkg/provider/api.go` (interface `SlackAPI` ~line 163; `MCPSlackClient` methods ~line 392)

- [ ] **Step 1: Add `DraftsCreate` to the `SlackAPI` interface**

In `pkg/provider/api.go`, in the `// Edge API methods` block of `SlackAPI` (currently `ClientUserBoot` and `UsersSearch`), add:

```go
	// Edge API methods
	ClientUserBoot(ctx context.Context) (*edge.ClientUserBootResponse, error)
	UsersSearch(ctx context.Context, query string, count int) ([]slack.User, error)
	DraftsCreate(ctx context.Context, channelID, threadTs string, blocks json.RawMessage) (string, error)
```

Ensure `encoding/json` is imported in `pkg/provider/api.go` (add it to the import block if missing).

- [ ] **Step 2: Implement the delegation on `MCPSlackClient`**

After the existing `UsersSearch` method (~line 392-394), add:

```go
func (c *MCPSlackClient) DraftsCreate(ctx context.Context, channelID, threadTs string, blocks json.RawMessage) (string, error) {
	return c.edgeClient.DraftsCreate(ctx, channelID, threadTs, blocks)
}
```

- [ ] **Step 3: Build to verify the interface is satisfied**

Run: `cd /Users/aleksandrmakarov/code/slack-mcp-server && go build ./...`
Expected: no output (success). A failure here means `MCPSlackClient` doesn't satisfy `SlackAPI` — recheck the signature matches exactly.

- [ ] **Step 4: Commit**

```bash
cd /Users/aleksandrmakarov/code/slack-mcp-server
git add pkg/provider/api.go
git commit -m "feat(provider): wire DraftsCreate through SlackAPI"
```

---

## Task 3: Handler, parser, and channel-allow guard

**Files:**
- Modify: `pkg/handler/conversations.go` (params struct ~line 92; new handler near `ConversationsAddMessageHandler` ~line 193; new parser near `parseParamsToolAddMessage` ~line 936; allow helper near `isChannelAllowed` ~line 691)
- Test: `pkg/handler/conversations_test.go` (add a unit test near `TestUnitIsChannelAllowedForConfig` ~line 594)

- [ ] **Step 1: Write the failing test for the draft channel-allow wrapper**

In `pkg/handler/conversations_test.go`, add:

```go
func TestUnitIsDraftChannelAllowed(t *testing.T) {
	t.Run("allowed when env unset-as-true", func(t *testing.T) {
		t.Setenv("SLACK_MCP_DRAFT_MESSAGE_TOOL", "true")
		if !isDraftChannelAllowed("C123") {
			t.Fatal("expected C123 to be allowed with =true")
		}
	})
	t.Run("restricted to listed channel", func(t *testing.T) {
		t.Setenv("SLACK_MCP_DRAFT_MESSAGE_TOOL", "C123")
		if !isDraftChannelAllowed("C123") {
			t.Fatal("expected listed channel C123 to be allowed")
		}
		if isDraftChannelAllowed("C999") {
			t.Fatal("expected unlisted channel C999 to be denied")
		}
	})
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /Users/aleksandrmakarov/code/slack-mcp-server && go test ./pkg/handler/ -run TestUnitIsDraftChannelAllowed -v`
Expected: FAIL — compile error, `isDraftChannelAllowed` undefined.

- [ ] **Step 3: Add the allow-check helper**

In `pkg/handler/conversations.go`, directly after `isChannelAllowed` (~line 691-693), add:

```go
func isDraftChannelAllowed(channel string) bool {
	return isChannelAllowedForConfig(channel, os.Getenv("SLACK_MCP_DRAFT_MESSAGE_TOOL"))
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd /Users/aleksandrmakarov/code/slack-mcp-server && go test ./pkg/handler/ -run TestUnitIsDraftChannelAllowed -v`
Expected: PASS.

- [ ] **Step 5: Add the params struct**

In `pkg/handler/conversations.go`, after the `addMessageParams` struct (~line 92-97), add:

```go
type draftMessageParams struct {
	channel     string
	threadTs    string
	text        string
	contentType string
}
```

- [ ] **Step 6: Add the param parser**

In `pkg/handler/conversations.go`, after `parseParamsToolAddMessage` (~line 996), add:

```go
func (ch *ConversationsHandler) parseParamsToolDraftMessage(ctx context.Context, request mcp.CallToolRequest) (*draftMessageParams, error) {
	toolConfig := os.Getenv("SLACK_MCP_DRAFT_MESSAGE_TOOL")
	enabledTools := os.Getenv("SLACK_MCP_ENABLED_TOOLS")

	if toolConfig == "" {
		if !strings.Contains(enabledTools, "conversations_draft_message") {
			ch.logger.Error("Draft-message tool disabled by default")
			return nil, errors.New(
				"by default, the conversations_draft_message tool is disabled. " +
					"To enable it, set the SLACK_MCP_DRAFT_MESSAGE_TOOL environment variable to true, 1, or a comma separated list of channels " +
					"to limit where the MCP can create drafts, e.g. 'SLACK_MCP_DRAFT_MESSAGE_TOOL=C1234567890,D0987654321', 'SLACK_MCP_DRAFT_MESSAGE_TOOL=!C1234567890' " +
					"to enable all except one or 'SLACK_MCP_DRAFT_MESSAGE_TOOL=true' for all channels and DMs",
			)
		}
		toolConfig = "true"
	}

	channel := request.GetString("channel_id", "")
	if channel == "" {
		ch.logger.Error("channel_id missing in draft-message params")
		return nil, errors.New("channel_id must be a string")
	}
	channel, err := ch.resolveChannelID(ctx, channel)
	if err != nil {
		ch.logger.Error("Channel not found", zap.String("channel", channel), zap.Error(err))
		return nil, err
	}
	if !isDraftChannelAllowed(channel) {
		ch.logger.Warn("Draft-message tool not allowed for channel", zap.String("channel", channel), zap.String("policy", toolConfig))
		return nil, fmt.Errorf("conversations_draft_message tool is not allowed for channel %q, applied policy: %s", channel, toolConfig)
	}

	threadTs := request.GetString("thread_ts", "")
	if threadTs != "" && !strings.Contains(threadTs, ".") {
		ch.logger.Error("Invalid thread_ts format", zap.String("thread_ts", threadTs))
		return nil, errors.New("thread_ts must be a valid timestamp in format 1234567890.123456")
	}

	msgText := request.GetString("text", "")
	if msgText == "" {
		ch.logger.Error("Message text missing")
		return nil, errors.New("text must be a string")
	}

	contentType := request.GetString("content_type", "text/markdown")
	if contentType != "text/plain" && contentType != "text/markdown" {
		ch.logger.Error("Invalid content_type", zap.String("content_type", contentType))
		return nil, errors.New("content_type must be either 'text/plain' or 'text/markdown'")
	}

	return &draftMessageParams{
		channel:     channel,
		threadTs:    threadTs,
		text:        msgText,
		contentType: contentType,
	}, nil
}
```

- [ ] **Step 7: Add the handler**

In `pkg/handler/conversations.go`, after `ConversationsAddMessageHandler` (~line 276), add:

```go
// draftResult is the CSV confirmation row returned after creating a draft.
type draftResult struct {
	DraftID  string `csv:"draft_id"`
	Channel  string `csv:"channel_id"`
	ThreadTS string `csv:"thread_ts"`
}

// ConversationsDraftMessageHandler creates a native Slack draft and returns a CSV confirmation.
func (ch *ConversationsHandler) ConversationsDraftMessageHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ConversationsDraftMessageHandler called", zap.Any("params", request.Params))

	if ready, err := ch.apiProvider.IsReady(); !ready {
		ch.logger.Error("API provider not ready", zap.Error(err))
		return nil, err
	}

	params, err := ch.parseParamsToolDraftMessage(ctx, request)
	if err != nil {
		ch.logger.Error("Failed to parse draft-message params", zap.Error(err))
		return nil, err
	}

	var blocks []slack.Block
	switch params.contentType {
	case "text/plain":
		blocks = []slack.Block{
			slack.NewSectionBlock(slack.NewTextBlockObject(slack.PlainTextType, params.text, false, false), nil, nil),
		}
	case "text/markdown":
		converted, convErr := slackGoUtil.ConvertMarkdownTextToBlocks(params.text)
		if convErr != nil {
			ch.logger.Warn("Markdown parsing error, falling back to plain text", zap.Error(convErr))
			blocks = []slack.Block{
				slack.NewSectionBlock(slack.NewTextBlockObject(slack.PlainTextType, params.text, false, false), nil, nil),
			}
		} else {
			blocks = converted
		}
	default:
		return nil, errors.New("content_type must be either 'text/plain' or 'text/markdown'")
	}

	blocksJSON, err := json.Marshal(blocks)
	if err != nil {
		ch.logger.Error("Failed to marshal blocks", zap.Error(err))
		return nil, err
	}

	ch.logger.Debug("Creating Slack draft",
		zap.String("channel", params.channel),
		zap.String("thread_ts", params.threadTs),
		zap.String("content_type", params.contentType),
	)
	draftID, err := ch.apiProvider.Slack().DraftsCreate(ctx, params.channel, params.threadTs, blocksJSON)
	if err != nil {
		ch.logger.Error("Slack DraftsCreate failed", zap.Error(err))
		return nil, err
	}

	csvBytes, err := gocsv.MarshalBytes(&[]draftResult{{
		DraftID:  draftID,
		Channel:  params.channel,
		ThreadTS: params.threadTs,
	}})
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(csvBytes)), nil
}
```

- [ ] **Step 8: Build and run the handler package tests**

Run: `cd /Users/aleksandrmakarov/code/slack-mcp-server && go build ./... && go test ./pkg/handler/ -run TestUnit -v`
Expected: build succeeds; all `TestUnit*` tests PASS (including `TestUnitIsDraftChannelAllowed`).

- [ ] **Step 9: Commit**

```bash
cd /Users/aleksandrmakarov/code/slack-mcp-server
git add pkg/handler/conversations.go pkg/handler/conversations_test.go
git commit -m "feat(handler): add conversations_draft_message handler and parser"
```

---

## Task 4: Tool registration

**Files:**
- Modify: `pkg/server/server.go` (const block ~line 27-49; registration near the add-message block ~line 152-199)

- [ ] **Step 1: Add the tool constant and register it as valid**

In `pkg/server/server.go`, add to the `const` block (after `ToolConversationsMark`):

```go
	ToolConversationsMark           = "conversations_mark"
	ToolConversationsDraftMessage   = "conversations_draft_message"
	ToolChannelsList                = "channels_list"
```

And add to `ValidToolNames` (after `ToolConversationsMark`):

```go
	ToolConversationsMark,
	ToolConversationsDraftMessage,
	ToolChannelsList,
```

- [ ] **Step 2: Register the tool**

In `pkg/server/server.go`, after the `ToolConversationsMark` registration block (~line 199, before the `ToolReactionsAdd` block), add:

```go
	// Drafts use the undocumented edge API, which needs a session token (xoxc/xoxd).
	// Bot tokens cannot reach it, so only register for non-bot tokens.
	if !provider.IsBotToken() && shouldAddTool(ToolConversationsDraftMessage, enabledTools, "SLACK_MCP_DRAFT_MESSAGE_TOOL") {
		s.AddTool(mcp.NewTool(ToolConversationsDraftMessage,
			mcp.WithDescription("Create a native Slack draft message in a channel, DM, or thread. The draft is saved to the user's Drafts and is NOT sent automatically. Requires a session token (xoxc/xoxd); not available with bot tokens."),
			mcp.WithTitleAnnotation("Draft Message"),
			mcp.WithString("channel_id",
				mcp.Required(),
				mcp.Description("ID of the channel in format Cxxxxxxxxxx or its name starting with #... or @... aka #general or @username_dm."),
			),
			mcp.WithString("thread_ts",
				mcp.Description("Unique identifier of a thread's parent message in format 1234567890.123456. Optional; if provided the draft is a threaded reply, otherwise it targets the channel itself."),
			),
			mcp.WithString("text",
				mcp.Description("Message text in specified content_type format. Example: 'Hello, world!' for text/plain or '# Hello, world!' for text/markdown."),
			),
			mcp.WithString("content_type",
				mcp.DefaultString("text/markdown"),
				mcp.Description("Content type of the message. Default is 'text/markdown'. Allowed values: 'text/markdown', 'text/plain'."),
			),
		), conversationsHandler.ConversationsDraftMessageHandler)
	}
```

- [ ] **Step 3: Build to verify**

Run: `cd /Users/aleksandrmakarov/code/slack-mcp-server && go build ./...`
Expected: no output (success).

- [ ] **Step 4: Run the full test suite (non-integration)**

Run: `cd /Users/aleksandrmakarov/code/slack-mcp-server && go test ./... -run 'TestUnit|TestBuildDraft|TestDraftsCreate' -v`
Expected: PASS across `pkg/handler`, `pkg/provider/edge`. (The network/OpenAI integration tests are not selected by these run filters.)

- [ ] **Step 5: Commit**

```bash
cd /Users/aleksandrmakarov/code/slack-mcp-server
git add pkg/server/server.go
git commit -m "feat(server): register conversations_draft_message tool"
```

---

## Task 5: Documentation

**Files:**
- Modify: `README.md`
- Modify: `docs/03-configuration-and-usage.md`

- [ ] **Step 1: Update `README.md` — Features bullet**

After the existing "Safe Message Posting" feature bullet (~line 17), add:

```markdown
- **Native Drafts**: The `conversations_draft_message` tool creates Slack drafts (saved to your Drafts, never auto-sent). Disabled by default; enable via `SLACK_MCP_DRAFT_MESSAGE_TOOL`. Requires a session token (`xoxc`/`xoxd`).
```

- [ ] **Step 2: Update `README.md` — Tools section**

After the `conversations_mark` tool subsection (§8, ~line 107-110), add a new subsection:

```markdown
### 9. conversations_draft_message:

Creates a native Slack **draft** in a channel, DM, or thread. The draft appears in the user's Slack "Drafts" list and is **never sent automatically** — the user reviews and sends it from Slack.

| Argument       | Type   | Required | Description                                                                 |
|----------------|--------|----------|-----------------------------------------------------------------------------|
| `channel_id`   | string | Yes      | `Cxxxxxxxxxx`, `#channel`, or `@username_dm`.                               |
| `thread_ts`    | string | No       | Thread parent timestamp `1234567890.123456`. If set, draft is a reply.      |
| `text`         | string | Yes      | Message text in `content_type` format.                                      |
| `content_type` | string | No       | `text/markdown` (default) or `text/plain`.                                  |

> **Note:** Drafting is disabled by default. Enable it via `SLACK_MCP_DRAFT_MESSAGE_TOOL` (`true`, `1`, a comma-separated channel allowlist, or `!Cxxxx` negation), or by listing `conversations_draft_message` in `SLACK_MCP_ENABLED_TOOLS`. This tool uses Slack's edge API and requires a **session token (`xoxc`/`xoxd`)** — it is not registered for bot tokens. `@username` in `channel_id` resolves to that user's DM; `@username` inside `text` is not converted to a mention.
```

- [ ] **Step 3: Update `README.md` — Environment Variables quick reference**

In the Environment Variables (Quick Reference) table, after the `SLACK_MCP_ADD_MESSAGE_TOOL` row (~line 186), add:

```markdown
| `SLACK_MCP_DRAFT_MESSAGE_TOOL`    | No        | `nil`                     | Enable native draft creation via `conversations_draft_message` by setting it to `true` for all channels, a comma-separated list of channel IDs to whitelist, or `!` before a channel ID to allow all except those. If empty, the tool is only registered when explicitly listed in `SLACK_MCP_ENABLED_TOOLS`. Requires a session token (`xoxc`/`xoxd`); ignored for bot tokens. |
```

- [ ] **Step 4: Update `docs/03-configuration-and-usage.md` — tool lists**

In the `--enabled-tools` row (~line 264) and the `SLACK_MCP_ENABLED_TOOLS` row (~line 288), append `conversations_draft_message` to each "Available tools:" enumeration.

- [ ] **Step 5: Update `docs/03-configuration-and-usage.md` — env var table**

In the Environment Variables table, after the `SLACK_MCP_ADD_MESSAGE_UNFURLING` row (~line 284), add:

```markdown
| `SLACK_MCP_DRAFT_MESSAGE_TOOL`    | No        | `nil`                     | Enable native draft creation via `conversations_draft_message`: `true` for all channels, a comma-separated channel-ID whitelist, or `!Cxxxx` negation. Requires a session token (`xoxc`/`xoxd`); not available with bot tokens. Drafts are saved to the user's Drafts and never auto-sent. |
```

- [ ] **Step 6: Update `docs/03-configuration-and-usage.md` — write-tools section**

In the "Write tools (...) are not registered by default" paragraph (~line 298), add `conversations_draft_message` to the listed write tools, and add a sentence:

```markdown
`conversations_draft_message` additionally requires a session token (`xoxc`/`xoxd`) — it is not registered for bot tokens because it relies on Slack's edge API.
```

- [ ] **Step 7: Commit**

```bash
cd /Users/aleksandrmakarov/code/slack-mcp-server
git add README.md docs/03-configuration-and-usage.md
git commit -m "docs: document conversations_draft_message tool and SLACK_MCP_DRAFT_MESSAGE_TOOL"
```

---

## Task 6: Final verification

**Files:** none (verification only)

- [ ] **Step 1: Full build**

Run: `cd /Users/aleksandrmakarov/code/slack-mcp-server && go build ./...`
Expected: no output.

- [ ] **Step 2: Vet**

Run: `cd /Users/aleksandrmakarov/code/slack-mcp-server && go vet ./...`
Expected: no findings.

- [ ] **Step 3: Unit tests**

Run: `cd /Users/aleksandrmakarov/code/slack-mcp-server && go test ./pkg/... -run 'TestUnit|TestBuildDraft|TestDraftsCreate'`
Expected: `ok` for `pkg/handler` and `pkg/provider/edge`.

- [ ] **Step 4: Manual smoke test (requires a real xoxc/xoxd session token)**

Build the binary and run it with the tool enabled, then call the tool from an MCP client (or the MCP inspector):

```bash
cd /Users/aleksandrmakarov/code/slack-mcp-server
SLACK_MCP_XOXC_TOKEN=xoxc-... SLACK_MCP_XOXD_TOKEN=xoxd-... \
SLACK_MCP_DRAFT_MESSAGE_TOOL=true \
go run ./cmd/slack-mcp-server --transport stdio
```

Call `conversations_draft_message` with `{ "channel_id": "#general", "text": "# Draft test\nHello" }`. Expected: a CSV result with a non-empty `draft_id`, and the draft visible in Slack's **Drafts** list. Verify a `text/plain` draft and a `thread_ts` reply draft as well.

> If `drafts.create` returns an API error (e.g. `invalid_blocks`, `unknown_method`), revisit the pre-implementation checkpoint — the payload shape needs correction.

- [ ] **Step 5: Finalize the branch**

Use the `superpowers:finishing-a-development-branch` skill to decide merge/PR/cleanup.
```
