// @layer: unit
// @spec: 004-safe-draft-lifecycle
//
// Acceptance-level tests for the safe draft lifecycle feature as a whole
// (functional-spec.md §2.1-§2.5), driven end-to-end through the same mock
// seam as pkg/handler/conversations_draft_test.go (draftsRecorder,
// newDraftHandlerFixture, callDraftMessage). Each test is a multi-call flow —
// what makes these acceptance rather than unit tests is that they chain
// several ConversationsDraftMessageHandler invocations the way a real agent
// conversation would, and assert on the flow as a whole rather than one call
// in isolation. The per-slice unit tests already cover individual branches;
// these do not repeat that coverage.
package handler

import (
	"encoding/json"
	"testing"

	"github.com/korotovsky/slack-mcp-server/pkg/provider/edge"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// injectBlockID returns raw with "block_id" set to id on every top-level
// block, simulating what Slack's drafts.list actually echoes: a
// server-generated id that can differ between two listings of logically
// identical content (live finding 8). Used to prove that normalizeDraftBlocks
// — not raw byte comparison — is what makes the consent protocol's
// byte-for-byte comparison work.
func injectBlockID(t *testing.T, raw json.RawMessage, id string) json.RawMessage {
	t.Helper()
	var blocks []map[string]any
	require.NoError(t, json.Unmarshal(raw, &blocks))
	for _, b := range blocks {
		b["block_id"] = id
	}
	out, err := json.Marshal(blocks)
	require.NoError(t, err)
	return out
}

// TestUnitDraftLifecycleFullConsentRoundTrip is the protocol's happy path
// (functional spec §2.1, §2.3): an agent writes a draft, reads back its own
// blocks_json, calls again at the same destination, gets existing_draft_found,
// compares the two byte-for-byte, finds them identical despite Slack echoing
// a different injected block_id on each listing (live finding 8), and only
// then re-calls with overwrite=true. If this comparison does not genuinely
// succeed after a real write+read cycle, the entire consent design is broken
// and every single call would demand the user's attention.
//
// @spec: 004-safe-draft-lifecycle
// @regression
func TestUnitDraftLifecycleFullConsentRoundTrip(t *testing.T) {
	written, err := json.Marshal([]slack.Block{plainRichTextBlock("full consent round trip")})
	require.NoError(t, err)

	// Two listings of the identical logical content, differing only in the
	// block_id Slack injects on each list call.
	createdEcho := injectBlockID(t, written, "BID_FIRST_LIST")
	rereadEcho := injectBlockID(t, written, "BID_SECOND_LIST")

	created := edge.Draft{
		ID:            "D_ROUNDTRIP",
		ClientMsgID:   "cmid-roundtrip",
		LastUpdatedTS: "1752000000.1111111",
		Destinations:  []edge.DraftDestination{{ChannelID: "C0DEST"}},
		Blocks:        createdEcho,
	}
	reread := created
	reread.Blocks = rereadEcho

	replacedWritten, err := json.Marshal([]slack.Block{plainRichTextBlock("full consent round trip v2")})
	require.NoError(t, err)
	replaced := created
	replaced.LastUpdatedTS = "1752000100.2222222"
	replaced.Blocks = injectBlockID(t, replacedWritten, "BID_THIRD_LIST")

	mock := &draftsRecorder{
		createID: "D_ROUNDTRIP",
		listQueue: [][]edge.Draft{
			nil,        // [0] pre-write listing for the create: nothing there yet
			{created},  // [1] confirming re-list after create
			{reread},   // [2] agent's second call: read back before acting
			{reread},   // [3] pre-write listing for the authorized overwrite
			{replaced}, // [4] confirming re-list after the overwrite
		},
	}
	h := newDraftHandlerFixture(t, mock)

	// Step 1: agent writes the draft.
	res1, err := callDraftMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         "full consent round trip",
		"content_type": "text/plain",
	})
	require.NoError(t, err)
	out1 := decodeDraftActionResult(t, res1)
	require.Equal(t, draftActionCreated, out1.Action)

	// Step 2: agent calls again at the same destination — the compare step.
	// The text supplied here must never be written, since a draft is found.
	res2, err := callDraftMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         "irrelevant text — this call must write nothing",
		"content_type": "text/plain",
	})
	require.NoError(t, err)
	out2 := decodeDraftActionResult(t, res2)
	require.Equal(t, draftActionExistingFound, out2.Action)

	// The crux of the protocol: byte-for-byte identical despite Slack having
	// echoed a different block_id on each listing.
	assert.Equal(t, string(out1.Draft.BlocksJSON), string(out2.Draft.BlocksJSON),
		"the agent's own untouched draft must compare byte-identical across two listings, or every call would spuriously demand user consent")

	// Step 3: comparison succeeded, so the agent re-calls with overwrite=true
	// with no user interaction required.
	res3, err := callDraftMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         "full consent round trip v2",
		"content_type": "text/plain",
		"overwrite":    true,
	})
	require.NoError(t, err)
	out3 := decodeDraftActionResult(t, res3)
	assert.Equal(t, draftActionReplaced, out3.Action)
	assert.Equal(t, "D_ROUNDTRIP", out3.DraftID)
	require.NotNil(t, out3.Displaced)
	assert.Equal(t, string(out1.Draft.BlocksJSON), string(out3.Displaced.BlocksJSON),
		"the displaced content must be the agent's own original draft, byte-identical to what it stored")

	assert.Len(t, mock.createCalls, 1, "exactly one draft created across the whole round trip")
	assert.Len(t, mock.updateCalls, 1, "exactly one update — the authorized overwrite")
}

// TestUnitDraftLifecycleHumanEditDetected (functional spec §2.1) is the
// defect that lost the real user's writing: the assistant creates a draft,
// the user edits it by hand in the Slack client, and the assistant asks again
// at the same destination. What comes back must be the human's current edit,
// not the content the assistant itself wrote moments earlier — the mismatch
// is what forces consent before anything is overwritten.
//
// @spec: 004-safe-draft-lifecycle
// @regression
func TestUnitDraftLifecycleHumanEditDetected(t *testing.T) {
	written, err := json.Marshal([]slack.Block{plainRichTextBlock("agent original draft")})
	require.NoError(t, err)
	created := edge.Draft{
		ID:            "D_HUMANEDIT",
		ClientMsgID:   "cmid-humanedit",
		LastUpdatedTS: "1752000000.1111111",
		Destinations:  []edge.DraftDestination{{ChannelID: "C0DEST"}},
		Blocks:        written,
	}

	// A human edit in the Slack desktop client preserves id, client_msg_id and
	// destination but changes content and last_updated_ts (live finding 5).
	humanEdited := created
	humanEdited.LastUpdatedTS = "1752000050.5555555"
	humanEdited.Blocks = json.RawMessage(`[{"type":"rich_text","block_id":"hUM4N","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"human added more text"}]}]}]`)

	mock := &draftsRecorder{
		createID:  "D_HUMANEDIT",
		listQueue: [][]edge.Draft{nil, {created}, {humanEdited}},
	}
	h := newDraftHandlerFixture(t, mock)

	res1, err := callDraftMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         "agent original draft",
		"content_type": "text/plain",
	})
	require.NoError(t, err)
	out1 := decodeDraftActionResult(t, res1)
	require.Equal(t, draftActionCreated, out1.Action)
	assert.Equal(t, "agent original draft", out1.Draft.Text)

	res2, err := callDraftMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         "irrelevant text — this call must write nothing",
		"content_type": "text/plain",
	})
	require.NoError(t, err)
	out2 := decodeDraftActionResult(t, res2)
	require.Equal(t, draftActionExistingFound, out2.Action)

	// The reported content must be the human's current edit, not the
	// assistant's stale creation content.
	assert.Equal(t, "human added more text", out2.Draft.Text)
	assert.NotEqual(t, string(out1.Draft.BlocksJSON), string(out2.Draft.BlocksJSON),
		"a human edit must produce a content mismatch — this is what forces the agent to ask before overwriting")

	assert.Empty(t, mock.updateCalls, "no write must happen without an explicit overwrite=true")
	assert.Len(t, mock.createCalls, 1)
}

// TestUnitDraftLifecycleRestoreFidelity (functional spec §2.3, §2.4) covers
// every formatting kind the markdown converter emits — heading, bold, italic,
// inline code, fenced code, ordered/unordered/nested lists, quotes and links
// — plus element types the converter never emits at all: a user mention, a
// channel mention and an emoji, which only Slack's own client can produce.
// That is what makes a genuinely HUMAN-authored draft restorable. The
// displaced content from a "replaced" result is fed straight back via
// content_type: application/json and must reproduce the original
// byte-for-byte.
//
// @spec: 004-safe-draft-lifecycle
// @regression
func TestUnitDraftLifecycleRestoreFidelity(t *testing.T) {
	const comprehensiveMarkdown = "# Heading\n\n" +
		"**bold** and *italic* and `inline code` text.\n\n" +
		"- unordered item one\n" +
		"- unordered item two\n" +
		"  - nested item\n\n" +
		"1. ordered item one\n" +
		"2. ordered item two\n\n" +
		"> a blockquote line\n\n" +
		"```\ncode fence line\n```\n\n" +
		"[a link](https://example.com/docs)\n"

	rtb, err := markdownToRichTextBlock(comprehensiveMarkdown)
	require.NoError(t, err)

	// Foreign elements the markdown converter never emits — only Slack's own
	// composer produces these, which is exactly what a human-authored draft
	// looks like.
	rtb.Elements = append(rtb.Elements, &slack.RichTextSection{
		Type: slack.RTESection,
		Elements: []slack.RichTextSectionElement{
			&slack.RichTextSectionUserElement{Type: slack.RTSEUser, UserID: "U0MENTION"},
			&slack.RichTextSectionChannelElement{Type: slack.RTSEChannel, ChannelID: "C0MENTION"},
			&slack.RichTextSectionEmojiElement{Type: slack.RTSEEmoji, Name: "tada"},
		},
	})

	original, err := json.Marshal([]slack.Block{rtb})
	require.NoError(t, err)

	existing := edge.Draft{
		ID:            "D_RESTORE",
		ClientMsgID:   "cmid-restore",
		LastUpdatedTS: "1752000000.1111111",
		Destinations:  []edge.DraftDestination{{ChannelID: "C0DEST"}},
		Blocks:        injectBlockID(t, original, "BID_ORIG"),
	}

	agentWritten, err := json.Marshal([]slack.Block{plainRichTextBlock("agent replacement, authorized")})
	require.NoError(t, err)
	replaced := existing
	replaced.LastUpdatedTS = "1752000100.2222222"
	replaced.Blocks = injectBlockID(t, agentWritten, "BID_REPL")

	mock := &draftsRecorder{
		listQueue: [][]edge.Draft{
			{existing}, // [0] read
			{existing}, // [1] pre-write for the authorized overwrite
			{replaced}, // [2] confirm after overwrite
			{replaced}, // [3] pre-write for the restore
			nil,        // [4] filled in below, once the exact restore bytes are known
		},
	}
	h := newDraftHandlerFixture(t, mock)

	// Step 1: agent reads the human's draft before touching it.
	res1, err := callDraftMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         "irrelevant text — this call must write nothing",
		"content_type": "text/plain",
	})
	require.NoError(t, err)
	out1 := decodeDraftActionResult(t, res1)
	require.Equal(t, draftActionExistingFound, out1.Action)

	// Step 2: user authorizes; the assistant overwrites and gets back the
	// displaced content.
	res2, err := callDraftMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         "agent replacement, authorized",
		"content_type": "text/plain",
		"overwrite":    true,
	})
	require.NoError(t, err)
	out2 := decodeDraftActionResult(t, res2)
	require.Equal(t, draftActionReplaced, out2.Action)
	require.NotNil(t, out2.Displaced)

	wantOriginal, nErr := normalizeDraftBlocks(original)
	require.NoError(t, nErr)
	assert.Equal(t, string(wantOriginal), string(out2.Displaced.BlocksJSON),
		"displaced content must be the original human-authored blocks, byte-preserved apart from block_id/key order")

	// Slack's echo after the restore write re-injects its own block_id —
	// simulate that now that the exact restore bytes are known.
	mock.listQueue[4] = []edge.Draft{{
		ID:            existing.ID,
		ClientMsgID:   existing.ClientMsgID,
		LastUpdatedTS: "1752000200.3333333",
		Destinations:  existing.Destinations,
		Blocks:        injectBlockID(t, out2.Displaced.BlocksJSON, "BID_RESTORED"),
	}}

	// Step 3: restore — the displaced blocks_json fed straight back.
	res3, err := callDraftMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         string(out2.Displaced.BlocksJSON),
		"content_type": "application/json",
		"overwrite":    true,
	})
	require.NoError(t, err)
	out3 := decodeDraftActionResult(t, res3)
	assert.Equal(t, draftActionReplaced, out3.Action)
	assert.Equal(t, existing.ID, out3.DraftID, "restore updates the same draft — none left over")

	assert.Equal(t, string(out2.Displaced.BlocksJSON), string(out3.Draft.BlocksJSON),
		"the draft in Slack after restore must be byte-identical to the original — bold, italic, code, lists, quotes, links, headings and the mention/emoji elements our converter never emits")

	require.Len(t, mock.updateCalls, 2, "two updates: the authorized overwrite and the restore")
	assert.Empty(t, mock.createCalls, "exactly one draft throughout — restore must never create a duplicate")
}

// TestUnitDraftLifecycleNRevisionsOneDraft (functional spec §2.2) is the
// defect that produced four drafts for one message: repeated authorized
// revisions of a single message must converge on exactly one draft in Slack,
// and every reported action must match what actually happened.
//
// @spec: 004-safe-draft-lifecycle
// @regression
func TestUnitDraftLifecycleNRevisionsOneDraft(t *testing.T) {
	v1, err := json.Marshal([]slack.Block{plainRichTextBlock("revision 1")})
	require.NoError(t, err)
	draftV1 := edge.Draft{
		ID:            "D_NREV",
		ClientMsgID:   "cmid-nrev",
		LastUpdatedTS: "1752000000.0000001",
		Destinations:  []edge.DraftDestination{{ChannelID: "C0DEST"}},
		Blocks:        v1,
	}

	mkNext := func(prev edge.Draft, text, ts string) edge.Draft {
		b, err := json.Marshal([]slack.Block{plainRichTextBlock(text)})
		require.NoError(t, err)
		next := prev
		next.LastUpdatedTS = ts
		next.Blocks = b
		return next
	}
	draftV2 := mkNext(draftV1, "revision 2", "1752000100.0000002")
	draftV3 := mkNext(draftV2, "revision 3", "1752000200.0000003")
	draftV4 := mkNext(draftV3, "revision 4", "1752000300.0000004")

	mock := &draftsRecorder{
		createID: "D_NREV",
		listQueue: [][]edge.Draft{
			nil, {draftV1}, // create
			{draftV1}, {draftV2}, // revision 2
			{draftV2}, {draftV3}, // revision 3
			{draftV3}, {draftV4}, // revision 4
		},
	}
	h := newDraftHandlerFixture(t, mock)

	res1, err := callDraftMessage(t, h, map[string]any{"channel_id": "C0DEST", "text": "revision 1", "content_type": "text/plain"})
	require.NoError(t, err)
	out1 := decodeDraftActionResult(t, res1)
	require.Equal(t, draftActionCreated, out1.Action)

	for _, txt := range []string{"revision 2", "revision 3", "revision 4"} {
		res, err := callDraftMessage(t, h, map[string]any{
			"channel_id":   "C0DEST",
			"text":         txt,
			"content_type": "text/plain",
			"overwrite":    true,
		})
		require.NoError(t, err)
		out := decodeDraftActionResult(t, res)
		require.Equal(t, draftActionReplaced, out.Action, "every revision after the first must report replaced, never created")
		assert.Equal(t, out1.DraftID, out.DraftID, "every revision must land on the same draft id")
	}

	assert.Len(t, mock.createCalls, 1, "exactly one draft created for the whole sequence")
	assert.Len(t, mock.updateCalls, 3, "three revisions, three updates, zero duplicates")
}

// TestUnitDraftLifecycleScheduledDraftSurvivesEverything (functional spec
// §2.1) is the strongest guarantee in the spec: a scheduled draft — a queued
// send — must never be touched, via the destination path with
// overwrite=true, or via an explicit draft_id with overwrite=true.
//
// @spec: 004-safe-draft-lifecycle
// @regression
func TestUnitDraftLifecycleScheduledDraftSurvivesEverything(t *testing.T) {
	scheduled := handWrittenDraft()
	scheduled.ID = "D_SCHEDULED"
	scheduled.DateScheduled = 1752999999

	freshWritten, err := json.Marshal([]slack.Block{plainRichTextBlock("attempt via destination")})
	require.NoError(t, err)
	sideBySide := edge.Draft{
		ID:            "D_SIDE",
		LastUpdatedTS: "1752000200.0000001",
		Destinations:  []edge.DraftDestination{{ChannelID: "C0DEST"}},
		Blocks:        freshWritten,
	}

	mock := &draftsRecorder{
		createID: "D_SIDE",
		listQueue: [][]edge.Draft{
			{scheduled},             // destination-path pre-write: scheduled is invisible to the lookup
			{scheduled, sideBySide}, // confirming re-list after the harmless create
			{scheduled, sideBySide}, // draft_id-path lookup
		},
	}
	h := newDraftHandlerFixture(t, mock)

	// Vector 1: destination path, overwrite=true. The scheduled draft is
	// invisible to findDraftForDestination, so this creates a harmless
	// duplicate beside it — it must never route to update.
	res1, err := callDraftMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         "attempt via destination",
		"content_type": "text/plain",
		"overwrite":    true,
	})
	require.NoError(t, err)
	out1 := decodeDraftActionResult(t, res1)
	assert.Equal(t, draftActionCreated, out1.Action)
	assert.Empty(t, mock.updateCalls, "the scheduled draft must never be updated via the destination path")

	// Vector 2: explicit draft_id targeting the scheduled draft, overwrite=true.
	res2, err := callDraftMessage(t, h, map[string]any{
		"channel_id": "C0DEST",
		"text":       "attempt via draft_id",
		"draft_id":   scheduled.ID,
		"overwrite":  true,
	})
	require.Error(t, err)
	assert.Nil(t, res2)
	assert.Contains(t, err.Error(), "scheduled")

	assert.Empty(t, mock.updateCalls, "the scheduled draft must never be updated, via any path, under any assertion")
	assert.Len(t, mock.createCalls, 1, "only the one harmless duplicate from vector 1 — the scheduled draft itself was never touched")
}

// TestUnitDraftLifecycleNoLeakWhenDisabledOrDenied (functional spec §2.5): a
// disabled tool and a denied channel must both refuse before any draft is
// read — the refusal path in this feature carries private draft content, so
// a gating regression here would leak it. Both gates are exercised against
// the same recorder to prove neither reads a single draft, cumulatively.
//
// @spec: 004-safe-draft-lifecycle
// @regression
func TestUnitDraftLifecycleNoLeakWhenDisabledOrDenied(t *testing.T) {
	mock := &draftsRecorder{listQueue: [][]edge.Draft{{handWrittenDraft()}}}
	h := newDraftHandlerFixture(t, mock)

	t.Run("tool disabled entirely", func(t *testing.T) {
		t.Setenv("SLACK_MCP_DRAFT_MESSAGE_TOOL", "")
		t.Setenv("SLACK_MCP_ENABLED_TOOLS", "")
		res, err := callDraftMessage(t, h, map[string]any{"channel_id": "C0DEST", "text": "body"})
		require.Error(t, err)
		assert.Nil(t, res)
		assert.NotContains(t, err.Error(), "hand-written words")
	})

	t.Run("channel denied", func(t *testing.T) {
		t.Setenv("SLACK_MCP_DRAFT_MESSAGE_TOOL", "C0OTHER")
		res, err := callDraftMessage(t, h, map[string]any{"channel_id": "C0DEST", "text": "body"})
		require.Error(t, err)
		assert.Nil(t, res)
		assert.NotContains(t, err.Error(), "hand-written words")
	})

	assert.Equal(t, 0, mock.listCalls, "neither gate may read a single draft — the draft in the queue was never touched")
	assert.Empty(t, mock.createCalls)
	assert.Empty(t, mock.updateCalls)
}

// TestUnitDraftLifecycleThreadedRevisionStaysThreaded (functional spec §2.4)
// is the exact symptom that started this feature: a draft created as a reply
// in a thread must stay in that thread on revision, never migrating to the
// channel's main message box. It also covers the sibling criterion — a
// channel-level request must not treat an existing threaded draft as the one
// to replace, and must create its own channel-level draft alongside it,
// leaving the threaded draft untouched. (The pure-function half of that
// sibling criterion — findDraftForDestination ignoring a threaded draft for a
// channel-level lookup — is already covered by
// TestUnitFindDraftForDestination/matches_channel-level_draft in
// conversations_test.go; this test proves the same guarantee end-to-end
// through the handler.)
//
// @spec: 004-safe-draft-lifecycle
// @regression
func TestUnitDraftLifecycleThreadedRevisionStaysThreaded(t *testing.T) {
	const threadTs = "1752000000.100001"

	v1, err := json.Marshal([]slack.Block{plainRichTextBlock("threaded draft v1")})
	require.NoError(t, err)
	createdThreaded := edge.Draft{
		ID:            "D_THREAD",
		ClientMsgID:   "cmid-thread",
		LastUpdatedTS: "1752000000.1111111",
		Destinations:  []edge.DraftDestination{{ChannelID: "C0DEST", ThreadTS: threadTs}},
		Blocks:        v1,
	}

	v2, err := json.Marshal([]slack.Block{plainRichTextBlock("threaded draft v2")})
	require.NoError(t, err)
	updatedThreaded := createdThreaded
	updatedThreaded.LastUpdatedTS = "1752000100.2222222"
	updatedThreaded.Blocks = v2

	v3, err := json.Marshal([]slack.Block{plainRichTextBlock("channel-level draft")})
	require.NoError(t, err)
	channelLevelCreated := edge.Draft{
		ID:            "D_CHANLEVEL",
		ClientMsgID:   "cmid-chanlevel",
		LastUpdatedTS: "1752000200.3333333",
		Destinations:  []edge.DraftDestination{{ChannelID: "C0DEST"}},
		Blocks:        v3,
	}

	mock := &draftsRecorder{
		createID: "D_THREAD",
		listQueue: [][]edge.Draft{
			nil,                                    // [0] pre-write for the threaded create
			{createdThreaded},                      // [1] confirm after create
			{createdThreaded},                      // [2] pre-write for the threaded revision
			{updatedThreaded},                      // [3] confirm after the threaded revision
			{updatedThreaded},                      // [4] pre-write for the channel-level request
			{updatedThreaded, channelLevelCreated}, // [5] confirm after the channel-level create
		},
	}
	h := newDraftHandlerFixture(t, mock)

	// Step 1: create a draft as a reply to a specific message.
	res1, err := callDraftMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"thread_ts":    threadTs,
		"text":         "threaded draft v1",
		"content_type": "text/plain",
	})
	require.NoError(t, err)
	out1 := decodeDraftActionResult(t, res1)
	require.Equal(t, draftActionCreated, out1.Action)
	assert.Equal(t, threadTs, out1.ThreadTS)

	// Step 2: revise it, authorized. The revision must stay attached to the
	// same thread.
	res2, err := callDraftMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"thread_ts":    threadTs,
		"text":         "threaded draft v2",
		"content_type": "text/plain",
		"overwrite":    true,
	})
	require.NoError(t, err)
	out2 := decodeDraftActionResult(t, res2)
	require.Equal(t, draftActionReplaced, out2.Action)
	assert.Equal(t, threadTs, out2.ThreadTS, "the reported result must still identify the thread")

	require.Len(t, mock.updateCalls, 1)
	up := mock.updateCalls[0]
	assert.Equal(t, "C0DEST", up.channelID)
	assert.Equal(t, threadTs, up.threadTs,
		"the revision must be sent back to Slack with the same thread_ts — an empty or missing thread_ts here is the draft silently migrating to the channel's main message box, the exact symptom that started this feature")

	// Step 3 (sibling criterion): a channel-level request (no thread_ts) at
	// the same channel must not treat the threaded draft as the one to
	// replace — it must create its own channel-level draft and leave the
	// threaded draft alone.
	mock.createID = "D_CHANLEVEL"
	res3, err := callDraftMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         "channel-level draft",
		"content_type": "text/plain",
	})
	require.NoError(t, err)
	out3 := decodeDraftActionResult(t, res3)
	assert.Equal(t, draftActionCreated, out3.Action, "a channel-level request must never match a threaded draft")
	assert.Equal(t, "D_CHANLEVEL", out3.DraftID)

	require.Len(t, mock.createCalls, 2, "one threaded create, one channel-level create")
	assert.Equal(t, "", mock.createCalls[1].threadTs, "the second create is channel-level")
	assert.Len(t, mock.updateCalls, 1, "the threaded draft was never touched by the channel-level request")
}
