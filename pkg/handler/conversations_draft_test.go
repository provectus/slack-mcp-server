// @layer: unit
// @spec: 004-safe-draft-lifecycle
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"github.com/korotovsky/slack-mcp-server/pkg/provider/edge"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// draftsRecorder is an in-memory draftsAPI that records every call, so the
// tests can prove not just what the handler returned but what it wrote (and,
// critically for the refusal path, that it wrote nothing). DraftsList serves
// listQueue entries in call order, repeating the last one — the first entry
// is the pre-write listing, the second the confirming re-list.
type draftsRecorder struct {
	listQueue [][]edge.Draft
	listErr   error
	createID  string
	createErr error
	updateErr error

	listCalls   int
	createCalls []draftsCreateCall
	updateCalls []draftsUpdateCall
}

type draftsCreateCall struct {
	channelID string
	threadTs  string
	blocks    json.RawMessage
}

type draftsUpdateCall struct {
	draftID       string
	clientMsgID   string
	lastUpdatedTS string
	channelID     string
	threadTs      string
	blocks        json.RawMessage
}

func (m *draftsRecorder) DraftsList(ctx context.Context, limit int) ([]edge.Draft, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	i := m.listCalls
	m.listCalls++
	if len(m.listQueue) == 0 {
		return nil, nil
	}
	if i >= len(m.listQueue) {
		i = len(m.listQueue) - 1
	}
	return m.listQueue[i], nil
}

func (m *draftsRecorder) DraftsCreate(ctx context.Context, channelID, threadTs string, blocks json.RawMessage) (string, error) {
	m.createCalls = append(m.createCalls, draftsCreateCall{channelID: channelID, threadTs: threadTs, blocks: blocks})
	if m.createErr != nil {
		return "", m.createErr
	}
	return m.createID, nil
}

func (m *draftsRecorder) DraftsUpdate(ctx context.Context, draftID, clientMsgID, lastUpdatedTS, channelID, threadTs string, blocks json.RawMessage) error {
	m.updateCalls = append(m.updateCalls, draftsUpdateCall{
		draftID:       draftID,
		clientMsgID:   clientMsgID,
		lastUpdatedTS: lastUpdatedTS,
		channelID:     channelID,
		threadTs:      threadTs,
		blocks:        blocks,
	})
	return m.updateErr
}

// newDraftHandlerFixture builds a ConversationsHandler backed by a real,
// network-free ApiProvider ("demo" mode skips auth and the Slack client;
// SkipCache satisfies IsReady; raw channel IDs pass through resolveChannelID
// unresolved) with the draft tool enabled for all channels and the drafts
// seam pointed at the recorder.
//
// Credential gate: SLACK_MCP_XOXP_TOKEN=demo is an OAuth-shaped token, yet
// the handler's session-token gate passes because ApiProvider.IsOAuth()
// returns false whenever ap.client is not an *MCPSlackClient — and demo mode
// never builds one. The fixture depends on that nil-client behaviour
// deliberately (it is what makes the suite representative of a session-token
// setup without a network-built client); tests that need the gate to fire
// set h.isOAuthTokenFn instead.
func newDraftHandlerFixture(t *testing.T, mock *draftsRecorder) *ConversationsHandler {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("SLACK_MCP_XOXP_TOKEN", "demo")
	t.Setenv("SLACK_MCP_XOXB_TOKEN", "")
	t.Setenv("SLACK_MCP_XOXC_TOKEN", "")
	t.Setenv("SLACK_MCP_XOXD_TOKEN", "")
	t.Setenv("SLACK_MCP_USERS_CACHE", filepath.Join(dir, "users_cache.json"))
	t.Setenv("SLACK_MCP_CHANNELS_CACHE", filepath.Join(dir, "channels_cache.json"))
	t.Setenv("SLACK_MCP_CHANNEL_MEMBERS_CACHE", filepath.Join(dir, "members_cache.json"))
	t.Setenv("SLACK_MCP_DRAFT_MESSAGE_TOOL", "true")

	ap := provider.New("stdio", zap.NewNop())
	ap.SkipCache()

	h := NewConversationsHandler(ap, zap.NewNop())
	h.drafts = mock
	return h
}

func callDraftMessage(t *testing.T, h *ConversationsHandler, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	var req mcp.CallToolRequest
	req.Params.Name = "conversations_draft_message"
	req.Params.Arguments = args
	return h.ConversationsDraftMessageHandler(context.Background(), req)
}

func decodeDraftActionResult(t *testing.T, res *mcp.CallToolResult) draftActionResult {
	t.Helper()
	require.NotNil(t, res)
	require.Len(t, res.Content, 1)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected TextContent")
	var out draftActionResult
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &out), "result must be the JSON draftActionResult, got: %s", tc.Text)
	return out
}

// handWrittenBlocks mimics what drafts.list returns for a draft the user typed
// in the Slack client: rich_text with a server-injected block_id that
// normalisation must strip before the agent compares.
const handWrittenBlocks = `[{"type":"rich_text","block_id":"hIJk","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"hand-written words"}]}]}]`

func handWrittenDraft() edge.Draft {
	return edge.Draft{
		ID:                "Dr0EXISTING1",
		ClientMsgID:       "cmid-existing",
		LastUpdatedTS:     "1752000000.1234567",
		LastUpdatedClient: "Slack SSB Mac (Atom)",
		Destinations:      []edge.DraftDestination{{ChannelID: "C0DEST"}},
		Blocks:            json.RawMessage(handWrittenBlocks),
	}
}

// TestUnitDraftMessageExistingDraftRefusesWithoutOverwrite is the headline
// safety property (functional spec §2.1): a draft already at the destination
// plus no overwrite assertion → a successful existing_draft_found result
// carrying the draft's content, and zero writes of any kind.
func TestUnitDraftMessageExistingDraftRefusesWithoutOverwrite(t *testing.T) {
	mock := &draftsRecorder{listQueue: [][]edge.Draft{{handWrittenDraft()}}}
	h := newDraftHandlerFixture(t, mock)

	res, err := callDraftMessage(t, h, map[string]any{
		"channel_id": "C0DEST",
		"text":       "agent replacement text",
	})
	require.NoError(t, err, "existing_draft_found must be a successful tool result, not a Go error")

	out := decodeDraftActionResult(t, res)
	assert.Equal(t, draftActionExistingFound, out.Action)
	assert.Equal(t, "Dr0EXISTING1", out.DraftID)
	assert.Equal(t, "C0DEST", out.ChannelID)

	wantBlocks, nErr := normalizeDraftBlocks(json.RawMessage(handWrittenBlocks))
	require.NoError(t, nErr)
	assert.Equal(t, string(wantBlocks), string(out.Draft.BlocksJSON),
		"blocks_json must be the normalised existing content — the agent's comparison source")
	assert.Equal(t, "hand-written words", out.Draft.Text)
	assert.Equal(t, "Slack SSB Mac (Atom)", out.Draft.LastUpdatedClient)
	assert.Contains(t, out.Note, "overwrite=true", "note must tell the agent what to do next")
	assert.Nil(t, out.Displaced, "nothing was displaced")

	assert.Empty(t, mock.createCalls, "refusal must write nothing: zero creates")
	assert.Empty(t, mock.updateCalls, "refusal must write nothing: zero updates")
}

// TestUnitDraftMessageOverwriteReplacesAndReportsDisplaced covers the
// authorized-overwrite path: the update carries the concurrency token freshly
// obtained from this invocation's listing, the result is "replaced" with the
// pre-update content in displaced, and draft reflects the confirming re-list.
func TestUnitDraftMessageOverwriteReplacesAndReportsDisplaced(t *testing.T) {
	existing := handWrittenDraft()

	written, err := json.Marshal([]slack.Block{plainRichTextBlock("replacement body")})
	require.NoError(t, err)

	updated := existing
	updated.LastUpdatedTS = "1752000100.7654321"
	updated.LastUpdatedClient = "Chrome"
	updated.Blocks = written

	mock := &draftsRecorder{listQueue: [][]edge.Draft{{existing}, {updated}}}
	h := newDraftHandlerFixture(t, mock)

	res, err := callDraftMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         "replacement body",
		"content_type": "text/plain",
		"overwrite":    true,
	})
	require.NoError(t, err)

	require.Len(t, mock.updateCalls, 1)
	up := mock.updateCalls[0]
	assert.Equal(t, existing.ID, up.draftID)
	assert.Equal(t, existing.ClientMsgID, up.clientMsgID)
	assert.Equal(t, existing.LastUpdatedTS, up.lastUpdatedTS,
		"update must carry the concurrency token freshly obtained in the same invocation")
	assert.Empty(t, mock.createCalls)
	assert.Equal(t, 2, mock.listCalls, "one pre-write listing plus one confirming re-list")

	out := decodeDraftActionResult(t, res)
	assert.Equal(t, draftActionReplaced, out.Action)
	assert.Equal(t, existing.ID, out.DraftID)

	require.NotNil(t, out.Displaced, "replaced must return the pre-update content")
	wantDisplaced, nErr := normalizeDraftBlocks(existing.Blocks)
	require.NoError(t, nErr)
	assert.Equal(t, string(wantDisplaced), string(out.Displaced.BlocksJSON))
	assert.Equal(t, "hand-written words", out.Displaced.Text)

	wantStored, nErr := normalizeDraftBlocks(written)
	require.NoError(t, nErr)
	assert.Equal(t, string(wantStored), string(out.Draft.BlocksJSON),
		"draft must be the as-stored content from the confirming re-list")
}

// TestUnitDraftMessageEmptyDestinationCreates: no draft at the destination →
// create, in both overwrite modes — the assertion permits replacing, it does
// not require something to replace.
func TestUnitDraftMessageEmptyDestinationCreates(t *testing.T) {
	for _, overwrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("overwrite=%v", overwrite), func(t *testing.T) {
			written, err := json.Marshal([]slack.Block{plainRichTextBlock("fresh draft")})
			require.NoError(t, err)

			created := edge.Draft{
				ID:                "Dr0NEW1",
				ClientMsgID:       "cmid-new",
				LastUpdatedTS:     "1752000200.0000001",
				LastUpdatedClient: "Chrome",
				Destinations:      []edge.DraftDestination{{ChannelID: "C0DEST"}},
				Blocks:            written,
			}
			mock := &draftsRecorder{
				createID:  "Dr0NEW1",
				listQueue: [][]edge.Draft{nil, {created}},
			}
			h := newDraftHandlerFixture(t, mock)

			res, err := callDraftMessage(t, h, map[string]any{
				"channel_id":   "C0DEST",
				"text":         "fresh draft",
				"content_type": "text/plain",
				"overwrite":    overwrite,
			})
			require.NoError(t, err)

			require.Len(t, mock.createCalls, 1)
			assert.Equal(t, "C0DEST", mock.createCalls[0].channelID)
			assert.Empty(t, mock.updateCalls, "an empty destination must never route to update")

			out := decodeDraftActionResult(t, res)
			assert.Equal(t, draftActionCreated, out.Action)
			assert.Equal(t, "Dr0NEW1", out.DraftID, "draft_id must come from the confirming re-list")
			assert.Nil(t, out.Displaced)
		})
	}
}

// TestUnitDraftMessageCreateNotesListTruncation: when the pre-write listing
// returns exactly draftsListLimit drafts, the window may have hidden an
// existing draft at the destination — the create still happens (fail-safe:
// a visible duplicate, never an overwrite), but the note must caveat "no
// draft previously existed" instead of asserting it confidently.
func TestUnitDraftMessageCreateNotesListTruncation(t *testing.T) {
	full := make([]edge.Draft, draftsListLimit)
	for i := range full {
		full[i] = edge.Draft{
			ID:            fmt.Sprintf("Dr0OTHER%03d", i),
			LastUpdatedTS: "1752000000.0000001",
			Destinations:  []edge.DraftDestination{{ChannelID: fmt.Sprintf("C0OTHER%03d", i)}},
			Blocks:        json.RawMessage(`[]`),
		}
	}

	written, err := json.Marshal([]slack.Block{plainRichTextBlock("fresh draft")})
	require.NoError(t, err)
	created := edge.Draft{
		ID:            "Dr0NEW1",
		LastUpdatedTS: "1752000200.0000001",
		Destinations:  []edge.DraftDestination{{ChannelID: "C0DEST"}},
		Blocks:        written,
	}
	mock := &draftsRecorder{createID: "Dr0NEW1", listQueue: [][]edge.Draft{full, {created}}}
	h := newDraftHandlerFixture(t, mock)

	res, err := callDraftMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         "fresh draft",
		"content_type": "text/plain",
	})
	require.NoError(t, err)

	require.Len(t, mock.createCalls, 1, "no listed draft matches the destination, so this is a create")
	assert.Empty(t, mock.updateCalls)

	out := decodeDraftActionResult(t, res)
	assert.Equal(t, draftActionCreated, out.Action)
	assert.Contains(t, out.Note, draftNoteCreated, "the created note stays; the caveat is appended to it")
	assert.Contains(t, out.Note, draftNoteListTruncated,
		"a listing that fills the whole window must caveat that an existing draft may have been missed")
}

// TestUnitDraftMessageConfirmReListMissErrors: a write whose confirming
// re-list cannot find the draft must surface an honest error naming the write
// that succeeded — never a fabricated success.
func TestUnitDraftMessageConfirmReListMissErrors(t *testing.T) {
	t.Run("create path", func(t *testing.T) {
		mock := &draftsRecorder{createID: "Dr0NEW1", listQueue: [][]edge.Draft{nil, nil}}
		h := newDraftHandlerFixture(t, mock)

		res, err := callDraftMessage(t, h, map[string]any{
			"channel_id": "C0DEST",
			"text":       "body",
		})
		require.Error(t, err)
		assert.Nil(t, res)
		require.Len(t, mock.createCalls, 1, "the create itself did happen")
		assert.Contains(t, err.Error(), "creation of draft Dr0NEW1")
		assert.Contains(t, err.Error(), "succeeded")
		assert.Contains(t, err.Error(), "re-list could not find")
	})

	t.Run("update path", func(t *testing.T) {
		existing := handWrittenDraft()
		mock := &draftsRecorder{listQueue: [][]edge.Draft{{existing}, nil}}
		h := newDraftHandlerFixture(t, mock)

		res, err := callDraftMessage(t, h, map[string]any{
			"channel_id": "C0DEST",
			"text":       "body",
			"overwrite":  true,
		})
		require.Error(t, err)
		assert.Nil(t, res)
		require.Len(t, mock.updateCalls, 1, "the update itself did happen")
		assert.Contains(t, err.Error(), "update of draft Dr0EXISTING1")
		assert.Contains(t, err.Error(), "succeeded")
		assert.Contains(t, err.Error(), "re-list could not find")
	})
}

// spyMarkdownPipeline sets the handler's markdown-pipeline seams to counting
// wrappers around the real implementations, so a test can prove whether
// markdownToRichTextBlock and draftContentLoss were invoked.
func spyMarkdownPipeline(t *testing.T, h *ConversationsHandler) (converterCalls, lossCalls *int) {
	t.Helper()
	var conv, loss int
	h.markdownToRichTextBlockFn = func(markdown string) (*slack.RichTextBlock, error) {
		conv++
		return markdownToRichTextBlock(markdown)
	}
	h.draftContentLossFn = func(input string, rtb *slack.RichTextBlock) []string {
		loss++
		return draftContentLoss(input, rtb)
	}
	return &conv, &loss
}

// TestUnitDraftMessageJSONContentBypassesMarkdownPipeline (technical spec
// §2.4): content_type application/json carries already-valid Slack blocks, so
// the markdown converter and the loss check must never run — the loss check
// would false-positive on content that was never markdown. The markdown
// control run proves the spies actually observe the pipeline.
func TestUnitDraftMessageJSONContentBypassesMarkdownPipeline(t *testing.T) {
	t.Run("application/json bypasses", func(t *testing.T) {
		normalized, err := normalizeDraftBlocks(json.RawMessage(handWrittenBlocks))
		require.NoError(t, err)

		created := handWrittenDraft()
		created.ID = "Dr0RESTORED1"
		mock := &draftsRecorder{createID: "Dr0RESTORED1", listQueue: [][]edge.Draft{nil, {created}}}
		h := newDraftHandlerFixture(t, mock)
		conv, loss := spyMarkdownPipeline(t, h)

		_, err = callDraftMessage(t, h, map[string]any{
			"channel_id":   "C0DEST",
			"text":         handWrittenBlocks,
			"content_type": "application/json",
		})
		require.NoError(t, err)

		assert.Zero(t, *conv, "markdownToRichTextBlock must never run on the JSON path")
		assert.Zero(t, *loss, "draftContentLoss must never run on the JSON path")
		require.Len(t, mock.createCalls, 1)
		assert.Equal(t, string(normalized), string(mock.createCalls[0].blocks),
			"the stored blocks must be the normalised verbatim input, not a markdown conversion")
	})

	t.Run("markdown control invokes the spies", func(t *testing.T) {
		created := handWrittenDraft()
		mock := &draftsRecorder{createID: created.ID, listQueue: [][]edge.Draft{nil, {created}}}
		h := newDraftHandlerFixture(t, mock)
		conv, loss := spyMarkdownPipeline(t, h)

		_, err := callDraftMessage(t, h, map[string]any{
			"channel_id":   "C0DEST",
			"text":         "plain markdown words",
			"content_type": "text/markdown",
		})
		require.NoError(t, err)
		assert.Equal(t, 1, *conv, "spy must observe the markdown converter, or the bypass assertion proves nothing")
		assert.Equal(t, 1, *loss, "spy must observe the loss check, or the bypass assertion proves nothing")
	})
}

// TestUnitDraftMessageJSONContentRejectsInvalidBlocks: the drafts composer
// accepts only rich_text blocks and silently drops anything else, so anything
// that is not an array of rich_text block objects is rejected explicitly —
// before any Slack traffic.
func TestUnitDraftMessageJSONContentRejectsInvalidBlocks(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		wantErr string
	}{
		{"non-rich_text block type", `[{"type":"section","text":{"type":"mrkdwn","text":"hi"}}]`, `type "section"`},
		{"mixed types", `[{"type":"rich_text","elements":[]},{"type":"header"}]`, `type "header"`},
		{"not an array", `{"type":"rich_text","elements":[]}`, "JSON array"},
		{"element not an object", `["rich_text"]`, "block objects"},
		{"empty array", `[]`, "at least one rich_text block"},
		{"not JSON at all", `**markdown**, not blocks`, "JSON array"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := &draftsRecorder{}
			h := newDraftHandlerFixture(t, mock)

			res, err := callDraftMessage(t, h, map[string]any{
				"channel_id":   "C0DEST",
				"text":         tc.text,
				"content_type": "application/json",
			})
			require.Error(t, err)
			assert.Nil(t, res)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Zero(t, mock.listCalls, "validation must fail before any Slack traffic")
			assert.Empty(t, mock.createCalls)
			assert.Empty(t, mock.updateCalls)
		})
	}
}

// TestUnitDraftMessageJSONRoundTripsByteIdentical (functional spec §2.3): a
// blocks_json previously returned by the tool, fed back via application/json,
// is stored byte-identically — and after Slack's echo re-adds block_id, the
// reported blocks_json still equals the input. This is what makes restore
// lossless.
func TestUnitDraftMessageJSONRoundTripsByteIdentical(t *testing.T) {
	// What a previous call returned: normalised blocks, no block_id.
	previous, err := normalizeDraftBlocks(json.RawMessage(handWrittenBlocks))
	require.NoError(t, err)

	// What drafts.list echoes after the write: same content with a fresh
	// server-injected block_id.
	stored := handWrittenDraft()
	stored.ID = "Dr0RESTORED1"
	mock := &draftsRecorder{createID: "Dr0RESTORED1", listQueue: [][]edge.Draft{nil, {stored}}}
	h := newDraftHandlerFixture(t, mock)

	res, err := callDraftMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         string(previous),
		"content_type": "application/json",
	})
	require.NoError(t, err)

	require.Len(t, mock.createCalls, 1)
	assert.Equal(t, string(previous), string(mock.createCalls[0].blocks),
		"the write must carry the input bytes unchanged — normalisation is idempotent on already-normalised blocks")

	out := decodeDraftActionResult(t, res)
	assert.Equal(t, draftActionCreated, out.Action)
	assert.Equal(t, string(previous), string(out.Draft.BlocksJSON),
		"the as-stored blocks_json must round-trip byte-identical to the restored input")
}

// TestUnitDraftMessageDraftIDNotFound: an unresolvable draft_id is an explicit
// error with nothing written — never a silent fallback to destination lookup
// or a create.
func TestUnitDraftMessageDraftIDNotFound(t *testing.T) {
	mock := &draftsRecorder{listQueue: [][]edge.Draft{{handWrittenDraft()}}}
	h := newDraftHandlerFixture(t, mock)

	res, err := callDraftMessage(t, h, map[string]any{
		"channel_id": "C0DEST",
		"text":       "body",
		"draft_id":   "Dr0MISSING99",
		"overwrite":  true,
	})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.Contains(t, err.Error(), `"Dr0MISSING99"`)
	assert.Contains(t, err.Error(), "nothing was written")
	assert.Empty(t, mock.createCalls)
	assert.Empty(t, mock.updateCalls)
}

// TestUnitDraftMessageDraftIDDestinationMismatch: draft_id is targeting, not
// retargeting — a resolved draft whose destination differs from the requested
// channel/thread is an explicit error, keeping the channel policy check
// covering the draft actually touched.
func TestUnitDraftMessageDraftIDDestinationMismatch(t *testing.T) {
	existing := handWrittenDraft() // targets C0DEST, no thread

	t.Run("different channel", func(t *testing.T) {
		mock := &draftsRecorder{listQueue: [][]edge.Draft{{existing}}}
		h := newDraftHandlerFixture(t, mock)

		res, err := callDraftMessage(t, h, map[string]any{
			"channel_id": "C0OTHER",
			"text":       "body",
			"draft_id":   existing.ID,
			"overwrite":  true,
		})
		require.Error(t, err)
		assert.Nil(t, res)
		assert.Contains(t, err.Error(), "different destination")
		assert.Contains(t, err.Error(), "nothing was written")
		assert.Empty(t, mock.createCalls)
		assert.Empty(t, mock.updateCalls)
	})

	t.Run("different thread", func(t *testing.T) {
		mock := &draftsRecorder{listQueue: [][]edge.Draft{{existing}}}
		h := newDraftHandlerFixture(t, mock)

		res, err := callDraftMessage(t, h, map[string]any{
			"channel_id": "C0DEST",
			"thread_ts":  "1752000000.111111",
			"text":       "body",
			"draft_id":   existing.ID,
			"overwrite":  true,
		})
		require.Error(t, err)
		assert.Nil(t, res)
		assert.Contains(t, err.Error(), "different destination")
		assert.Empty(t, mock.updateCalls)
	})
}

// TestUnitDraftMessageDraftIDMatchingDestinationReplaces: the draft_id happy
// path, with the destination match tolerant of trailing-zero timestamp widths
// (normalizeTS) exactly like the destination lookup.
func TestUnitDraftMessageDraftIDMatchingDestinationReplaces(t *testing.T) {
	existing := handWrittenDraft()
	existing.Destinations = []edge.DraftDestination{{ChannelID: "C0DEST", ThreadTS: "1752000000.123400"}}

	updated := existing
	updated.LastUpdatedTS = "1752000100.7654321"
	written, err := json.Marshal([]slack.Block{plainRichTextBlock("revised body")})
	require.NoError(t, err)
	updated.Blocks = written

	mock := &draftsRecorder{listQueue: [][]edge.Draft{{existing}, {updated}}}
	h := newDraftHandlerFixture(t, mock)

	res, err := callDraftMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"thread_ts":    "1752000000.1234", // trailing-zero width differs from the stored destination
		"text":         "revised body",
		"content_type": "text/plain",
		"draft_id":     existing.ID,
		"overwrite":    true,
	})
	require.NoError(t, err)

	require.Len(t, mock.updateCalls, 1)
	assert.Equal(t, existing.ID, mock.updateCalls[0].draftID)
	assert.Empty(t, mock.createCalls)

	out := decodeDraftActionResult(t, res)
	assert.Equal(t, draftActionReplaced, out.Action)
	assert.Equal(t, existing.ID, out.DraftID)
}

// TestUnitDraftMessageScheduledDraftUntouchable (functional spec §2.1,
// technical spec §2.5 step 5): a scheduled draft is a queued send; destroying
// one is worse than destroying a draft. The guard holds regardless of
// overwrite and regardless of how the draft was resolved.
func TestUnitDraftMessageScheduledDraftUntouchable(t *testing.T) {
	scheduled := handWrittenDraft()
	scheduled.DateScheduled = 1752100000

	t.Run("draft_id path refuses even with overwrite", func(t *testing.T) {
		mock := &draftsRecorder{listQueue: [][]edge.Draft{{scheduled}}}
		h := newDraftHandlerFixture(t, mock)

		res, err := callDraftMessage(t, h, map[string]any{
			"channel_id": "C0DEST",
			"text":       "body",
			"draft_id":   scheduled.ID,
			"overwrite":  true,
		})
		require.Error(t, err)
		assert.Nil(t, res)
		assert.Contains(t, err.Error(), "scheduled")
		assert.Empty(t, mock.createCalls)
		assert.Empty(t, mock.updateCalls, "a known draft_id must not bypass the scheduled-draft skip")
	})

	t.Run("destination path skips scheduled and creates", func(t *testing.T) {
		written, err := json.Marshal([]slack.Block{plainRichTextBlock("fresh body")})
		require.NoError(t, err)
		created := edge.Draft{
			ID:            "Dr0NEW1",
			LastUpdatedTS: "1752000200.0000001",
			Destinations:  []edge.DraftDestination{{ChannelID: "C0DEST"}},
			Blocks:        written,
		}
		mock := &draftsRecorder{createID: "Dr0NEW1", listQueue: [][]edge.Draft{{scheduled}, {scheduled, created}}}
		h := newDraftHandlerFixture(t, mock)

		res, err := callDraftMessage(t, h, map[string]any{
			"channel_id":   "C0DEST",
			"text":         "fresh body",
			"content_type": "text/plain",
			"overwrite":    true,
		})
		require.NoError(t, err)

		require.Len(t, mock.createCalls, 1, "the scheduled draft is invisible to the destination lookup; a new draft is created beside it")
		assert.Empty(t, mock.updateCalls, "the scheduled draft must never be updated")

		out := decodeDraftActionResult(t, res)
		assert.Equal(t, draftActionCreated, out.Action)
		assert.Equal(t, "Dr0NEW1", out.DraftID)
	})
}

// TestUnitDraftMessageConflictFailsSafe (technical spec §2.8): the user edited
// the draft in Slack between our list and our update; Slack rejects the write
// and the draft survives (live-confirmed). The handler must not retry —
// the content it compared against is stale — and instead returns a successful
// "conflict" result carrying the draft's current content, restarting the
// consent protocol.
func TestUnitDraftMessageConflictFailsSafe(t *testing.T) {
	existing := handWrittenDraft()

	edited := existing
	edited.LastUpdatedTS = "1752000300.9999999"
	edited.Blocks = json.RawMessage(`[{"type":"rich_text","block_id":"nEWiD","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"user edited this meanwhile"}]}]}]`)

	mock := &draftsRecorder{
		listQueue: [][]edge.Draft{{existing}, {edited}},
		updateErr: fmt.Errorf("drafts.update: %w", edge.ErrDraftConflict),
	}
	h := newDraftHandlerFixture(t, mock)

	res, err := callDraftMessage(t, h, map[string]any{
		"channel_id": "C0DEST",
		"text":       "agent replacement text",
		"overwrite":  true,
	})
	require.NoError(t, err, "conflict must be a successful tool result, not a Go error")

	require.Len(t, mock.updateCalls, 1, "the write must never be retried after a conflict")
	assert.Empty(t, mock.createCalls)
	assert.Equal(t, 2, mock.listCalls, "one pre-write listing plus one post-conflict re-list")

	out := decodeDraftActionResult(t, res)
	assert.Equal(t, draftActionConflict, out.Action)
	assert.Equal(t, existing.ID, out.DraftID)

	wantCurrent, nErr := normalizeDraftBlocks(edited.Blocks)
	require.NoError(t, nErr)
	assert.Equal(t, string(wantCurrent), string(out.Draft.BlocksJSON),
		"the result must carry the draft's current (edited) content — fresh state for the restarted consent protocol")
	assert.Equal(t, "user edited this meanwhile", out.Draft.Text)
	assert.Nil(t, out.Displaced, "nothing was displaced — the write was rejected")
	assert.Contains(t, out.Note, "overwrite=true", "note must restart the consent protocol")
}

// TestUnitDraftMessageDisabledToolNeverListsDrafts locks the gating order as
// an invariant (spec §2.7): the refusal path returns private draft content,
// so with the tool disabled the policy error must fire before any DraftsList
// — the mock proves no draft was listed, read or revealed.
func TestUnitDraftMessageDisabledToolNeverListsDrafts(t *testing.T) {
	mock := &draftsRecorder{listQueue: [][]edge.Draft{{handWrittenDraft()}}}
	h := newDraftHandlerFixture(t, mock)
	t.Setenv("SLACK_MCP_DRAFT_MESSAGE_TOOL", "")
	t.Setenv("SLACK_MCP_ENABLED_TOOLS", "")

	res, err := callDraftMessage(t, h, map[string]any{
		"channel_id": "C0DEST",
		"text":       "body",
	})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.Contains(t, err.Error(), "disabled")

	assert.Equal(t, 0, mock.listCalls, "policy must gate before any DraftsList — no draft text read")
	assert.Empty(t, mock.createCalls)
	assert.Empty(t, mock.updateCalls)
	assert.NotContains(t, err.Error(), "hand-written words", "the policy error must not reveal draft content")
}

// TestUnitDraftMessageDeniedChannelNeverListsDrafts: same invariant for the
// per-channel allow/deny policy — a denied channel yields the policy error
// with zero drafts read.
func TestUnitDraftMessageDeniedChannelNeverListsDrafts(t *testing.T) {
	mock := &draftsRecorder{listQueue: [][]edge.Draft{{handWrittenDraft()}}}
	h := newDraftHandlerFixture(t, mock)
	t.Setenv("SLACK_MCP_DRAFT_MESSAGE_TOOL", "C0OTHER")

	res, err := callDraftMessage(t, h, map[string]any{
		"channel_id": "C0DEST",
		"text":       "body",
	})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.Contains(t, err.Error(), "not allowed for channel")

	assert.Equal(t, 0, mock.listCalls, "policy must gate before any DraftsList — no draft text read")
	assert.Empty(t, mock.createCalls)
	assert.Empty(t, mock.updateCalls)
	assert.NotContains(t, err.Error(), "hand-written words", "the policy error must not reveal draft content")
}

// TestUnitDraftMessageMultiDestinationDraftRefused: a draft addressed to more
// than one conversation is out of scope (functional spec §3) and refused with
// a Go error before any content payload is built, on both resolution paths.
// Overwriting it would silently drop the other destinations (M1), and the
// channel policy only covered the requested channel, so returning its content
// through an existing_draft_found would leak text addressed to a possibly
// denied conversation (M2) — the error must not carry the draft body.
func TestUnitDraftMessageMultiDestinationDraftRefused(t *testing.T) {
	// Modelled the way Slack returns it: one draft, two destination entries.
	multiDest := handWrittenDraft()
	multiDest.Destinations = []edge.DraftDestination{
		{ChannelID: "C0DEST"},
		{ChannelID: "C0DENIED"},
	}

	cases := []struct {
		name string
		args map[string]any
	}{
		{
			"destination match with overwrite",
			map[string]any{"channel_id": "C0DEST", "text": "body", "overwrite": true},
		},
		{
			"destination match without overwrite",
			map[string]any{"channel_id": "C0DEST", "text": "body"},
		},
		{
			"explicit draft_id with overwrite",
			map[string]any{"channel_id": "C0DEST", "text": "body", "draft_id": multiDest.ID, "overwrite": true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := &draftsRecorder{listQueue: [][]edge.Draft{{multiDest}}}
			h := newDraftHandlerFixture(t, mock)

			res, err := callDraftMessage(t, h, tc.args)
			require.Error(t, err, "multi-destination drafts must refuse with a Go error, never a content-carrying result")
			assert.Nil(t, res)
			assert.Contains(t, err.Error(), "more than one conversation")
			assert.Contains(t, err.Error(), "nothing was written")
			assert.NotContains(t, err.Error(), "hand-written words",
				"the refusal must not reveal content addressed to a possibly denied conversation")

			assert.Empty(t, mock.createCalls, "refusal must write nothing: zero creates")
			assert.Empty(t, mock.updateCalls, "refusal must write nothing: zero updates")
		})
	}
}

// TestUnitDraftMessageConfirmMismatchNoted (functional spec §2.2): the
// confirming re-list proves the draft exists, not that it holds what was
// sent. When the re-listed content differs from the sent content, the result
// still succeeds but the note must report the mismatch instead of asserting
// confirmation — the agent must not memorise possibly stale bytes as its own
// confirmed write.
func TestUnitDraftMessageConfirmMismatchNoted(t *testing.T) {
	t.Run("update path", func(t *testing.T) {
		existing := handWrittenDraft()

		// The confirming re-list returns content that is NOT what was sent —
		// a concurrent edit landed between the update and the re-list.
		diverged := existing
		diverged.LastUpdatedTS = "1752000300.9999999"
		diverged.Blocks = json.RawMessage(`[{"type":"rich_text","block_id":"dIVg","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"someone else's words"}]}]}]`)

		mock := &draftsRecorder{listQueue: [][]edge.Draft{{existing}, {diverged}}}
		h := newDraftHandlerFixture(t, mock)

		res, err := callDraftMessage(t, h, map[string]any{
			"channel_id":   "C0DEST",
			"text":         "replacement body",
			"content_type": "text/plain",
			"overwrite":    true,
		})
		require.NoError(t, err, "a content mismatch on confirmation is still a successful write")

		out := decodeDraftActionResult(t, res)
		assert.Equal(t, draftActionReplaced, out.Action)
		assert.Equal(t, draftNoteReplacedMismatch, out.Note,
			"the note must report the mismatch, not assert confirmation")
		assert.NotContains(t, out.Note, "confirmed by re-listing",
			"the note must not assert the content was confirmed")
	})

	t.Run("create path", func(t *testing.T) {
		diverged := handWrittenDraft()
		diverged.ID = "Dr0NEW1"

		mock := &draftsRecorder{createID: "Dr0NEW1", listQueue: [][]edge.Draft{nil, {diverged}}}
		h := newDraftHandlerFixture(t, mock)

		res, err := callDraftMessage(t, h, map[string]any{
			"channel_id":   "C0DEST",
			"text":         "fresh body",
			"content_type": "text/plain",
		})
		require.NoError(t, err)

		out := decodeDraftActionResult(t, res)
		assert.Equal(t, draftActionCreated, out.Action)
		assert.Equal(t, draftNoteCreatedMismatch, out.Note,
			"the note must report the mismatch, not assert confirmation")
	})

	t.Run("matching content keeps the confirmed note", func(t *testing.T) {
		existing := handWrittenDraft()

		written, err := json.Marshal([]slack.Block{plainRichTextBlock("replacement body")})
		require.NoError(t, err)
		updated := existing
		updated.LastUpdatedTS = "1752000100.7654321"
		updated.Blocks = written

		mock := &draftsRecorder{listQueue: [][]edge.Draft{{existing}, {updated}}}
		h := newDraftHandlerFixture(t, mock)

		res, err := callDraftMessage(t, h, map[string]any{
			"channel_id":   "C0DEST",
			"text":         "replacement body",
			"content_type": "text/plain",
			"overwrite":    true,
		})
		require.NoError(t, err)

		out := decodeDraftActionResult(t, res)
		assert.Equal(t, draftNoteReplaced, out.Note,
			"matching re-listed content must keep the confirmed note")
	})
}

// TestUnitDraftMessageNullBlocksDraftStillReviewable (functional spec §2.1):
// a listed draft whose blocks are absent or null must not dead-end the
// refusal in a bare error — the agent could then neither review the draft nor
// obtain consent to overwrite it. Empty blocks are reported as empty content
// in a normal existing_draft_found.
func TestUnitDraftMessageNullBlocksDraftStillReviewable(t *testing.T) {
	cases := []struct {
		name   string
		blocks json.RawMessage
	}{
		{"absent blocks", nil},
		{"null blocks", json.RawMessage("null")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			empty := handWrittenDraft()
			empty.Blocks = tc.blocks

			mock := &draftsRecorder{listQueue: [][]edge.Draft{{empty}}}
			h := newDraftHandlerFixture(t, mock)

			res, err := callDraftMessage(t, h, map[string]any{
				"channel_id": "C0DEST",
				"text":       "agent replacement text",
			})
			require.NoError(t, err, "an empty draft must still yield a reviewable existing_draft_found")

			out := decodeDraftActionResult(t, res)
			assert.Equal(t, draftActionExistingFound, out.Action)
			assert.Equal(t, empty.ID, out.DraftID)
			assert.Equal(t, "[]", string(out.Draft.BlocksJSON),
				"absent/null blocks are reported as empty-but-present content")
			assert.Empty(t, out.Draft.Text)

			assert.Empty(t, mock.createCalls, "refusal must write nothing: zero creates")
			assert.Empty(t, mock.updateCalls, "refusal must write nothing: zero updates")
		})
	}
}

// TestUnitDraftMessageNonSessionTokenExplicitError covers the
// defense-in-depth credential gate (spec §2.7, functional §2.5): an OAuth
// token — which cannot reach the edge drafts API — gets the explicit
// explanation, not an obscure downstream failure, and no drafts are listed.
func TestUnitDraftMessageNonSessionTokenExplicitError(t *testing.T) {
	mock := &draftsRecorder{listQueue: [][]edge.Draft{{handWrittenDraft()}}}
	h := newDraftHandlerFixture(t, mock)

	h.isOAuthTokenFn = func() bool { return true }

	res, err := callDraftMessage(t, h, map[string]any{
		"channel_id": "C0DEST",
		"text":       "body",
	})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.Contains(t, err.Error(), "drafts require a browser-session token (xoxc/xoxd)")
	assert.Contains(t, err.Error(), "cannot access Slack's drafts API")

	assert.Equal(t, 0, mock.listCalls, "credential gate must fire before any DraftsList")
	assert.Empty(t, mock.createCalls)
	assert.Empty(t, mock.updateCalls)
}
