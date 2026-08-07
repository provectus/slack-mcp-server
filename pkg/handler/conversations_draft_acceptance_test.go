// @layer: unit
// @spec: 004-safe-draft-lifecycle
// @spec: 005-draft-formatting-fidelity
//
// Acceptance-level tests for two features, kept in one file because both are
// verified through the same handler seams and a reader comparing them wants
// them side by side:
//
//   - 004-safe-draft-lifecycle (first half of this file)
//   - 005-draft-formatting-fidelity (second half, from
//     TestUnitDraftFidelityBulletMarkersBecomeNativeLists onwards)
//
// Both are driven end-to-end through the same mock seam as
// pkg/handler/conversations_draft_test.go (newDraftHandlerFixture,
// callDraftMessage). What makes these acceptance rather than unit tests is
// that they exercise a whole user-visible outcome — a multi-call agent flow,
// or the exact bytes a tool call puts on the wire to Slack — rather than one
// function in isolation. The per-slice unit tests already cover individual
// branches; these do not repeat that coverage.
//
// Every test function name must begin with "TestUnit": `make test` runs
// `go test -run=".*Unit.*"`, so anything else silently never runs.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"github.com/korotovsky/slack-mcp-server/pkg/provider/edge"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
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

// ---------------------------------------------------------------------------
// 005-draft-formatting-fidelity
//
// Acceptance tests for the draft & message formatting fidelity feature as a
// whole (context/spec/005-draft-formatting-fidelity/functional-spec.md
// §2.1-§2.5). Each test states the criteria it covers.
//
// The layer under test is deliberately "what Slack actually receives": every
// assertion is made against the blocks handed to drafts.create / drafts.update
// or to chat.postMessage, decoded back from JSON — the same bytes that decide
// what the user sees when they open the draft. Assertions on the converter's
// in-memory return value alone would not prove the handler shipped it.
// ---------------------------------------------------------------------------

// slackDraftStore is an in-memory stand-in for Slack's drafts service: it
// remembers what was written and serves it back on the next list, echoing a
// server-generated block_id the way Slack does. draftsRecorder (the per-slice
// fake) replays a scripted list queue, which is the right tool for asserting
// call ordering but forces a test to know the created blocks before the call
// that creates them. These tests write real content and read it back, so they
// need a store rather than a script.
type slackDraftStore struct {
	t      *testing.T
	drafts []edge.Draft
	seq    int

	listCalls   int
	createCalls []draftsCreateCall
	updateCalls []draftsUpdateCall
}

func (s *slackDraftStore) DraftsList(_ context.Context, _ int) ([]edge.Draft, error) {
	s.listCalls++
	return append([]edge.Draft(nil), s.drafts...), nil
}

func (s *slackDraftStore) DraftsCreate(_ context.Context, channelID, threadTs string, blocks json.RawMessage) (string, error) {
	s.createCalls = append(s.createCalls, draftsCreateCall{channelID: channelID, threadTs: threadTs, blocks: blocks})
	s.seq++
	id := fmt.Sprintf("D_ACC%d", s.seq)
	s.drafts = append(s.drafts, edge.Draft{
		ID:            id,
		ClientMsgID:   "cmid-" + id,
		LastUpdatedTS: s.stamp(),
		Destinations:  []edge.DraftDestination{{ChannelID: channelID, ThreadTS: threadTs}},
		Blocks:        injectBlockID(s.t, blocks, "BID_"+id),
	})
	return id, nil
}

func (s *slackDraftStore) DraftsUpdate(_ context.Context, draftID, clientMsgID, lastUpdatedTS, channelID, threadTs string, blocks json.RawMessage) error {
	s.updateCalls = append(s.updateCalls, draftsUpdateCall{
		draftID:       draftID,
		clientMsgID:   clientMsgID,
		lastUpdatedTS: lastUpdatedTS,
		channelID:     channelID,
		threadTs:      threadTs,
		blocks:        blocks,
	})
	for i := range s.drafts {
		if s.drafts[i].ID != draftID {
			continue
		}
		s.seq++
		s.drafts[i].Blocks = injectBlockID(s.t, blocks, fmt.Sprintf("BID_%s_%d", draftID, s.seq))
		s.drafts[i].LastUpdatedTS = s.stamp()
		return nil
	}
	return fmt.Errorf("slackDraftStore: no draft %q to update", draftID)
}

func (s *slackDraftStore) stamp() string {
	return fmt.Sprintf("175200%04d.000000%d", s.seq, s.seq)
}

// lastCreatedBlocks returns the exact bytes the handler sent to drafts.create.
func (s *slackDraftStore) lastCreatedBlocks(t *testing.T) json.RawMessage {
	t.Helper()
	require.NotEmpty(t, s.createCalls, "expected the draft to have been created")
	return s.createCalls[len(s.createCalls)-1].blocks
}

// newDraftFidelityFixture wires the draft handler to an in-memory drafts store
// and an explicit mention directory. The directory is always injected: with a
// nil seam the handler builds a real one over the demo-mode provider, which has
// no Slack client, so every reference would refuse as "could not be verified"
// and no formatting assertion would ever be reached.
func newDraftFidelityFixture(t *testing.T, store *slackDraftStore, mentions mentionResolver) *ConversationsHandler {
	t.Helper()
	h := newDraftHandlerFixture(t, &draftsRecorder{})
	store.t = t
	h.drafts = store
	h.mentions = mentions
	return h
}

// singleRichTextBlock decodes a blocks payload the way Slack would and returns
// the one rich_text block it must contain. The drafts composer accepts nothing
// else, so a payload that is not exactly one rich_text block is already wrong.
func singleRichTextBlock(t *testing.T, raw json.RawMessage) *slack.RichTextBlock {
	t.Helper()
	var blocks slack.Blocks
	require.NoError(t, json.Unmarshal(raw, &blocks), "blocks payload must be decodable Slack JSON")
	require.Len(t, blocks.BlockSet, 1, "the draft body must be a single rich_text block")
	rtb, ok := blocks.BlockSet[0].(*slack.RichTextBlock)
	require.True(t, ok, "expected a rich_text block, got %T", blocks.BlockSet[0])
	return rtb
}

// (allSectionElements — flattening every leaf element of a rich_text block
// across sections, list items, quotes and preformatted blocks — already exists
// in draft_richtext_test.go and is reused here.)

// TestUnitDraftFidelityBulletMarkersBecomeNativeLists covers functional spec
// §2.1 criteria 1, 2, 3, 4 and 5 in one table: every marker the assistant
// commonly types produces a native Slack list of the right style, the marker
// character never leaks into the item text, and — because every input puts the
// introducing line immediately above the list with no blank line — the intro
// section carries no stray "\n\n" run.
//
// The 25-of-49 finding in the spec's rationale is what makes the "•" row the
// most important test in this file: it is the form the assistant actually uses.
//
// @spec: 005-draft-formatting-fidelity
// @regression
func TestUnitDraftFidelityBulletMarkersBecomeNativeLists(t *testing.T) {
	cases := []struct {
		name  string
		input string
		style slack.RichTextListElementType
	}{
		// §2.1 criterion 1 — the common case the defect report is about.
		{"bullet •", "Sprint plan:\n• ship the fix\n• tell the team\n", slack.RTEListBullet},
		// §2.1 criterion 2 — the other characters the assistant reaches for.
		{"white bullet ◦", "Sprint plan:\n◦ ship the fix\n◦ tell the team\n", slack.RTEListBullet},
		{"triangular bullet ‣", "Sprint plan:\n‣ ship the fix\n‣ tell the team\n", slack.RTEListBullet},
		{"hyphen bullet ⁃", "Sprint plan:\n⁃ ship the fix\n⁃ tell the team\n", slack.RTEListBullet},
		{"middle dot ·", "Sprint plan:\n· ship the fix\n· tell the team\n", slack.RTEListBullet},
		// §2.1 criterion 5 — the forms that already worked must keep working.
		{"dash -", "Sprint plan:\n- ship the fix\n- tell the team\n", slack.RTEListBullet},
		{"asterisk *", "Sprint plan:\n* ship the fix\n* tell the team\n", slack.RTEListBullet},
		{"plus +", "Sprint plan:\n+ ship the fix\n+ tell the team\n", slack.RTEListBullet},
		{"ordered 1.", "Sprint plan:\n1. ship the fix\n2. tell the team\n", slack.RTEListOrdered},
		// §2.1 criterion 3 — the paren form must match the dot form exactly.
		{"ordered 1)", "Sprint plan:\n1) ship the fix\n2) tell the team\n", slack.RTEListOrdered},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &slackDraftStore{}
			h := newDraftFidelityFixture(t, store, &stubMentionDirectory{})

			res, err := callDraftMessage(t, h, map[string]any{
				"channel_id": "C0DEST",
				"text":       tc.input,
			})
			require.NoError(t, err)
			require.Equal(t, draftActionCreated, decodeDraftActionResult(t, res).Action)

			rtb := singleRichTextBlock(t, store.lastCreatedBlocks(t))
			require.Len(t, rtb.Elements, 2,
				"expected [section, rich_text_list]; a run of plain lines each starting with the marker is the defect")

			intro, ok := rtb.Elements[0].(*slack.RichTextSection)
			require.True(t, ok, "element 0 must be the introducing section, got %T", rtb.Elements[0])
			assert.Equal(t, "Sprint plan:", sectionText(intro))
			for _, e := range intro.Elements {
				if te, ok := e.(*slack.RichTextSectionTextElement); ok {
					assert.NotContains(t, te.Text, "\n\n",
						"§2.1: the list must start directly beneath the intro line, with no empty line inserted")
				}
			}

			list, ok := rtb.Elements[1].(*slack.RichTextList)
			require.True(t, ok, "element 1 must be a native rich_text_list, got %T", rtb.Elements[1])
			assert.Equal(t, tc.style, list.Style)
			assert.Equal(t, 0, list.Indent)
			require.Len(t, list.Elements, 2)
			for i, want := range []string{"ship the fix", "tell the team"} {
				item, ok := list.Elements[i].(*slack.RichTextSection)
				require.True(t, ok)
				assert.Equal(t, want, sectionText(item),
					"the marker character must not survive inside the item text")
			}
		})
	}
}

// TestUnitDraftFidelityLoneBulletLineLosesNoWords covers functional spec §2.1
// criterion 6: a line that merely begins with a bullet character, and is not
// part of a list, must still be preserved and readable. The line-scan rewrite
// treats it as a one-item list, which is fine — the criterion is about content
// survival, and the draft tool's own loss check would refuse the write if a
// single word had gone missing.
//
// @spec: 005-draft-formatting-fidelity
// @regression
func TestUnitDraftFidelityLoneBulletLineLosesNoWords(t *testing.T) {
	const input = "· heads up, this one line is prose and not a list"

	store := &slackDraftStore{}
	h := newDraftFidelityFixture(t, store, &stubMentionDirectory{})

	res, err := callDraftMessage(t, h, map[string]any{
		"channel_id": "C0DEST",
		"text":       input,
	})
	require.NoError(t, err, "a lone bullet-prefixed line must not refuse the write")
	out := decodeDraftActionResult(t, res)
	require.Equal(t, draftActionCreated, out.Action)

	rtb := singleRichTextBlock(t, store.lastCreatedBlocks(t))
	assert.Contains(t, flattenRichText(rtb), "heads up, this one line is prose and not a list",
		"§2.1: the text must be preserved and readable; no content is lost")
	assert.Empty(t, draftContentLoss(input, rtb),
		"the conversion loss check must see nothing missing")
}

// TestUnitDraftFidelityListItemStylingSurvives covers functional spec §2.1
// criterion 7: bold, italic, inline code and a link inside a list item survive
// the bullet rewrite. The rewrite happens before the markdown parse, so a
// regression there would strip the item's inline markup rather than its marker.
//
// @spec: 005-draft-formatting-fidelity
// @regression
func TestUnitDraftFidelityListItemStylingSurvives(t *testing.T) {
	store := &slackDraftStore{}
	h := newDraftFidelityFixture(t, store, &stubMentionDirectory{})

	res, err := callDraftMessage(t, h, map[string]any{
		"channel_id": "C0DEST",
		"text":       "Checklist:\n• **bold** and *italic* and `code` and [runbook](https://example.com/run)\n",
	})
	require.NoError(t, err)
	require.Equal(t, draftActionCreated, decodeDraftActionResult(t, res).Action)

	rtb := singleRichTextBlock(t, store.lastCreatedBlocks(t))
	list := firstRichTextList(rtb)
	require.NotNil(t, list, "expected a native list, got %#v", rtb.Elements)
	require.Len(t, list.Elements, 1)
	item, ok := list.Elements[0].(*slack.RichTextSection)
	require.True(t, ok)

	var bold, italic, code, link bool
	for _, e := range item.Elements {
		switch el := e.(type) {
		case *slack.RichTextSectionTextElement:
			if el.Style == nil {
				continue
			}
			bold = bold || (el.Style.Bold && el.Text == "bold")
			italic = italic || (el.Style.Italic && el.Text == "italic")
			code = code || (el.Style.Code && el.Text == "code")
		case *slack.RichTextSectionLinkElement:
			link = link || el.URL == "https://example.com/run"
		}
	}
	assert.True(t, bold, "bold styling must survive inside the list item")
	assert.True(t, italic, "italic styling must survive inside the list item")
	assert.True(t, code, "inline code must survive inside the list item")
	assert.True(t, link, "a link must survive inside the list item")
	assert.NotContains(t, sectionText(item), "**", "literal markdown markers must not leak into the item")
}

// TestUnitDraftFidelityReferencesBecomeLiveMentions covers functional spec §2.2
// criteria 1, 2, 3 and 4: a person, channel and user-group reference mixed into
// ordinary sentence text each become the live element Slack renders as a
// clickable name, and every surrounding word, comma and space survives exactly.
// The element sequence is asserted in full, so a regression that ate a space or
// swallowed the trailing full stop fails here.
//
// @spec: 005-draft-formatting-fidelity
// @regression
func TestUnitDraftFidelityReferencesBecomeLiveMentions(t *testing.T) {
	store := &slackDraftStore{}
	mentions := &stubMentionDirectory{}
	h := newDraftFidelityFixture(t, store, mentions)

	res, err := callDraftMessage(t, h, map[string]any{
		"channel_id": "C0DEST",
		"text":       "Ask <@U0AAAAA1> in <#C0AAAAA1>, cc <!subteam^S0AAAAA1>.",
	})
	require.NoError(t, err)
	require.Equal(t, draftActionCreated, decodeDraftActionResult(t, res).Action)

	rtb := singleRichTextBlock(t, store.lastCreatedBlocks(t))
	require.Len(t, rtb.Elements, 1)
	section, ok := rtb.Elements[0].(*slack.RichTextSection)
	require.True(t, ok)
	require.Len(t, section.Elements, 7,
		"expected text/user/text/channel/text/usergroup/text, got %#v", section.Elements)

	text := func(i int) string {
		te, ok := section.Elements[i].(*slack.RichTextSectionTextElement)
		require.True(t, ok, "element %d must be text, got %T", i, section.Elements[i])
		return te.Text
	}
	assert.Equal(t, "Ask ", text(0))
	user, ok := section.Elements[1].(*slack.RichTextSectionUserElement)
	require.True(t, ok, "a person reference must become a live user element, got %T", section.Elements[1])
	assert.Equal(t, "U0AAAAA1", user.UserID)
	assert.Equal(t, " in ", text(2))
	channel, ok := section.Elements[3].(*slack.RichTextSectionChannelElement)
	require.True(t, ok, "a channel reference must become a live channel element, got %T", section.Elements[3])
	assert.Equal(t, "C0AAAAA1", channel.ChannelID)
	assert.Equal(t, ", cc ", text(4))
	group, ok := section.Elements[5].(*slack.RichTextSectionUserGroupElement)
	require.True(t, ok, "a user-group reference must become a live usergroup element, got %T", section.Elements[5])
	assert.Equal(t, "S0AAAAA1", group.UsergroupID)
	assert.Equal(t, ".", text(6))

	assert.NotContains(t, string(store.lastCreatedBlocks(t)), `<@U0AAAAA1>`,
		"no raw angle-bracket code may survive into the draft")
	assert.Equal(t, []string{"<@U0AAAAA1>", "<#C0AAAAA1>", "<!subteam^S0AAAAA1>"}, mentions.verifyCalls,
		"every reference written must be verified before the draft is written")
}

// TestUnitDraftFidelityMentionsResolveInsideStructures covers functional spec
// §2.2 criterion 5: a mention resolves wherever a draft can carry one — inside
// a heading, a quote and a list item — not only in a bare paragraph. Each
// container is parsed by a different goldmark node path, so one working proves
// nothing about the others.
//
// @spec: 005-draft-formatting-fidelity
// @regression
func TestUnitDraftFidelityMentionsResolveInsideStructures(t *testing.T) {
	store := &slackDraftStore{}
	h := newDraftFidelityFixture(t, store, &stubMentionDirectory{})

	res, err := callDraftMessage(t, h, map[string]any{
		"channel_id": "C0DEST",
		"text":       "# Release owner <@U0AAAAA1>\n\n> posted in <#C0AAAAA1>\n\n• paged <!subteam^S0AAAAA1>\n",
	})
	require.NoError(t, err)
	require.Equal(t, draftActionCreated, decodeDraftActionResult(t, res).Action)

	rtb := singleRichTextBlock(t, store.lastCreatedBlocks(t))
	require.Len(t, rtb.Elements, 3, "expected [heading section, quote, list], got %#v", rtb.Elements)

	heading, ok := rtb.Elements[0].(*slack.RichTextSection)
	require.True(t, ok)
	assert.True(t, hasUserElement(heading.Elements, "U0AAAAA1"), "a mention in a heading must resolve")

	quote, ok := rtb.Elements[1].(*slack.RichTextQuote)
	require.True(t, ok, "element 1 must be a quote, got %T", rtb.Elements[1])
	assert.True(t, hasChannelElement(quote.Elements, "C0AAAAA1"), "a mention in a quote must resolve")

	list, ok := rtb.Elements[2].(*slack.RichTextList)
	require.True(t, ok, "element 2 must be a list, got %T", rtb.Elements[2])
	require.Len(t, list.Elements, 1)
	item, ok := list.Elements[0].(*slack.RichTextSection)
	require.True(t, ok)
	assert.True(t, hasUserGroupElement(item.Elements, "S0AAAAA1"), "a mention in a list item must resolve")
}

func hasUserElement(elems []slack.RichTextSectionElement, id string) bool {
	for _, e := range elems {
		if el, ok := e.(*slack.RichTextSectionUserElement); ok && el.UserID == id {
			return true
		}
	}
	return false
}

func hasChannelElement(elems []slack.RichTextSectionElement, id string) bool {
	for _, e := range elems {
		if el, ok := e.(*slack.RichTextSectionChannelElement); ok && el.ChannelID == id {
			return true
		}
	}
	return false
}

func hasUserGroupElement(elems []slack.RichTextSectionElement, id string) bool {
	for _, e := range elems {
		if el, ok := e.(*slack.RichTextSectionUserGroupElement); ok && el.UsergroupID == id {
			return true
		}
	}
	return false
}

// TestUnitPostedMessageFidelityMatchesDraftFidelity covers functional spec §2.2
// criterion 6: posting directly rather than drafting gets the same treatment.
// One message carries both halves of the feature — a bullet list and all three
// reference classes — and the posted blocks must show a native list and three
// live mention elements, exactly as the drafted version does.
//
// @spec: 005-draft-formatting-fidelity
// @regression
func TestUnitPostedMessageFidelityMatchesDraftFidelity(t *testing.T) {
	post := &postMessageRecorder{}
	h := newAddMessageHandlerFixture(t, post)
	mentions := &stubMentionDirectory{}
	h.mentions = mentions

	res, err := callAddMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         "Release notes, cc <!subteam^S0AAAAA1>:\n• ask <@U0AAAAA1>\n• watch <#C0AAAAA1>\n",
		"content_type": "text/markdown",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Len(t, post.calls, 1)

	rtb := singleRichTextBlock(t, json.RawMessage(post.calls[0].blocksJSON(t)))
	require.NotNil(t, firstRichTextList(rtb),
		"a posted message's bullet lines must become a native list too")

	elems := allSectionElements(rtb)
	assert.True(t, hasUserElement(elems, "U0AAAAA1"), "the posted message must carry a live user element")
	assert.True(t, hasChannelElement(elems, "C0AAAAA1"), "the posted message must carry a live channel element")
	assert.True(t, hasUserGroupElement(elems, "S0AAAAA1"), "the posted message must carry a live usergroup element")

	assert.ElementsMatch(t, []string{"<!subteam^S0AAAAA1>", "<@U0AAAAA1>", "<#C0AAAAA1>"}, mentions.verifyCalls,
		"the post path must verify the same references the draft path does")
}

// TestUnitDraftFidelityBroadcastsStayInertText covers functional spec §2.3,
// both criteria: @here / @channel / @everyone written in any form reach the
// draft as ordinary text — never a rich_text broadcast element, which is the
// only thing that would make Slack notify the channel on send — and the text
// itself stays intact and legible, neither mangled nor partly swallowed.
//
// @spec: 005-draft-formatting-fidelity
// @regression
func TestUnitDraftFidelityBroadcastsStayInertText(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		wantAll string
	}{
		{"here", "heads up <!here> deploy at noon", "heads up @here deploy at noon"},
		{"channel", "heads up <!channel> deploy at noon", "heads up @channel deploy at noon"},
		{"everyone", "heads up <!everyone> deploy at noon", "heads up @everyone deploy at noon"},
		{"labelled here", "heads up <!here|@here> deploy at noon", "heads up @here deploy at noon"},
		{"typed as prose", "heads up @here deploy at noon", "heads up @here deploy at noon"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &slackDraftStore{}
			mentions := &stubMentionDirectory{}
			h := newDraftFidelityFixture(t, store, mentions)

			res, err := callDraftMessage(t, h, map[string]any{
				"channel_id": "C0DEST",
				"text":       tc.text,
			})
			require.NoError(t, err)
			out := decodeDraftActionResult(t, res)
			require.Equal(t, draftActionCreated, out.Action)

			raw := store.lastCreatedBlocks(t)
			assert.NotContains(t, string(raw), `"type":"broadcast"`,
				"§2.3: a live broadcast element must never be written on the user's behalf")

			rtb := singleRichTextBlock(t, raw)
			for _, e := range allSectionElements(rtb) {
				_, isBroadcast := e.(*slack.RichTextSectionBroadcastElement)
				assert.False(t, isBroadcast, "no element may be a broadcast")
			}
			assert.Equal(t, tc.wantAll, flattenRichText(rtb),
				"§2.3: the alert text must stay intact and legible — never mangled, split or partly swallowed")
			assert.Equal(t, tc.wantAll, out.Draft.Text,
				"the readback must show the same intact, inert text")
			assert.Empty(t, mentions.verifyCalls,
				"a broadcast resolves to nothing and must cost no workspace lookup")
		})
	}
}

// TestUnitDraftFidelityUnverifiableReferenceRefusesAndSparesExistingDraft
// covers functional spec §2.4 criteria 1, 2, 3 and 4 across all three reference
// classes and both failure modes. In every case a hand-written draft is already
// sitting at the destination and the caller has authorized overwriting it, so
// the assertion that it survives byte-for-byte is meaningful: the refusal must
// land above drafts.list, before anything can be read or damaged.
//
// @spec: 005-draft-formatting-fidelity
// @regression
func TestUnitDraftFidelityUnverifiableReferenceRefusesAndSparesExistingDraft(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		literal   string
		verifyErr error
		wantPhr   string
		denyPhr   string
	}{
		{
			name:      "unknown person",
			text:      "ping <@U0BOGUS1> about the release",
			literal:   "<@U0BOGUS1>",
			verifyErr: refNotFound("no such user"),
			wantPhr:   "does not exist in this workspace",
		},
		{
			name:      "unknown channel",
			text:      "see <#C0BOGUS1> for details",
			literal:   "<#C0BOGUS1>",
			verifyErr: refNotFound("no such channel"),
			wantPhr:   "does not exist in this workspace",
		},
		{
			name:      "unknown user group",
			text:      "cc <!subteam^S0BOGUS1> please",
			literal:   "<!subteam^S0BOGUS1>",
			verifyErr: refNotFound("no such user group"),
			wantPhr:   "does not exist in this workspace",
		},
		{
			// §2.4 criterion 3: the workspace could not be consulted. The
			// assistant must be told this is "could not check", never "does not
			// exist" — otherwise it rewrites a reference that is perfectly valid.
			name:      "user group could not be verified",
			text:      "cc <!subteam^S0123ABC> please",
			literal:   "<!subteam^S0123ABC>",
			verifyErr: refUnverifiable("usergroups.list failed: missing_scope — the token is missing the usergroups:read scope"),
			wantPhr:   `"could not check", not "does not exist"`,
			denyPhr:   "does not exist in this workspace",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			existing := handWrittenDraft()
			store := &slackDraftStore{drafts: []edge.Draft{existing}}
			h := newDraftFidelityFixture(t, store, &stubMentionDirectory{
				verify: map[string]error{tc.literal: tc.verifyErr},
			})

			res, err := callDraftMessage(t, h, map[string]any{
				"channel_id": "C0DEST",
				"text":       tc.text,
				"overwrite":  true,
			})
			require.Error(t, err, "an unverifiable reference must stop the write")
			assert.Nil(t, res)
			assert.Contains(t, err.Error(), tc.literal, "the error must name the offending reference")
			assert.Contains(t, err.Error(), tc.wantPhr)
			assert.Contains(t, err.Error(), "nothing was written")
			if tc.denyPhr != "" {
				assert.NotContains(t, err.Error(), tc.denyPhr,
					"could-not-verify must never read as a claim of absence")
			}

			assert.Zero(t, store.listCalls,
				"§2.4: the refusal must land before any drafts call — an existing draft is never even read")
			assert.Empty(t, store.createCalls)
			assert.Empty(t, store.updateCalls)
			require.Len(t, store.drafts, 1)
			assert.Equal(t, handWrittenBlocks, string(store.drafts[0].Blocks),
				"§2.4: the draft already at the destination must survive the refusal byte-for-byte")
			assert.Equal(t, existing.LastUpdatedTS, store.drafts[0].LastUpdatedTS,
				"an untouched draft's concurrency token must not move")
		})
	}
}

// TestUnitDraftFidelityMalformedDMCodeInPersonReferenceIsRefused is the
// originally reported defect, as an acceptance test: the assistant pasted the
// DM-channel code users_search returns into a person reference and the draft
// was written with "<@D0BJE66FPU5>" visible in it. Functional spec §2.4
// criterion 1 says a person reference whose code matches no real person must
// stop the write — and a D… code can match no person at all.
//
// Both write tools are covered in one test because the defect is one defect:
// §2.4 criterion 5 requires the direct post to stop the same way. The mention
// directory is given no failing entries, so a refusal that needed a lookup
// would not happen — the wrongness is visible in the grammar alone.
//
// @spec: 005-draft-formatting-fidelity
// @regression
func TestUnitDraftFidelityMalformedDMCodeInPersonReferenceIsRefused(t *testing.T) {
	const text = "bad ref check <@D0BJE66FPU5>"
	const wantWhy = "D0BJE66FPU5 is not a user ID (a D… code identifies a conversation)"

	t.Run("draft", func(t *testing.T) {
		store := &slackDraftStore{drafts: []edge.Draft{handWrittenDraft()}}
		mentions := &stubMentionDirectory{}
		h := newDraftFidelityFixture(t, store, mentions)

		res, err := callDraftMessage(t, h, map[string]any{
			"channel_id": "C0DEST",
			"text":       text,
			"overwrite":  true,
		})
		require.Error(t, err)
		assert.Nil(t, res)
		assert.Contains(t, err.Error(), "<@D0BJE66FPU5>")
		assert.Contains(t, err.Error(), wantWhy)
		assert.Contains(t, err.Error(), "nothing was written")
		assert.Empty(t, mentions.verifyCalls, "a malformed code is refused on the grammar alone")

		assert.Zero(t, store.listCalls)
		assert.Empty(t, store.createCalls)
		assert.Empty(t, store.updateCalls)
		assert.Equal(t, handWrittenBlocks, string(store.drafts[0].Blocks),
			"the draft already at the destination must survive untouched")
	})

	t.Run("posted message", func(t *testing.T) {
		post := &postMessageRecorder{}
		h := newAddMessageHandlerFixture(t, post)
		mentions := &stubMentionDirectory{}
		h.mentions = mentions

		res, err := callAddMessage(t, h, map[string]any{
			"channel_id":   "C0DEST",
			"text":         text,
			"content_type": "text/markdown",
		})
		require.Error(t, err, "§2.4: an unverifiable reference stops a direct post the same way")
		assert.Nil(t, res)
		assert.Contains(t, err.Error(), "<@D0BJE66FPU5>")
		assert.Contains(t, err.Error(), wantWhy)
		assert.Empty(t, post.calls,
			"the message must never reach Slack with a raw code in it")
	})
}

// TestUnitPostedMessageFidelityUnverifiableReferenceNeverPosts covers
// functional spec §2.4 criterion 5 for a well-formed reference that the
// workspace says nothing about: the post is refused, not degraded. Falling back
// to plain text here would publish the raw code to a channel — worse than a
// draft, because there is nothing left to review before it is seen.
//
// @spec: 005-draft-formatting-fidelity
// @regression
func TestUnitPostedMessageFidelityUnverifiableReferenceNeverPosts(t *testing.T) {
	post := &postMessageRecorder{}
	h := newAddMessageHandlerFixture(t, post)
	h.mentions = &stubMentionDirectory{verify: map[string]error{
		"<!subteam^S0BOGUS1>": refUnverifiable("usergroups.list failed: missing_scope — the token is missing the usergroups:read scope"),
	}}

	res, err := callAddMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         "cc <!subteam^S0BOGUS1> — deploying now",
		"content_type": "text/markdown",
	})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.Contains(t, err.Error(), "<!subteam^S0BOGUS1>")
	assert.Contains(t, err.Error(), `"could not check", not "does not exist"`)
	assert.Contains(t, err.Error(), "usergroups:read", "the could-not-verify form must name the fixable cause")
	assert.Empty(t, post.calls, "nothing may be posted when a reference could not be verified")
}

// TestUnitPostedMessageFidelityRawBlocksAreExempt covers the exemption the
// feature promises alongside §2.4: caller-supplied raw blocks are a verbatim
// path, never converted and never mention-validated — the same reason the draft
// tool exempts application/json. Validating them would make a message carrying
// a since-deactivated colleague's mention unpostable.
//
// @spec: 005-draft-formatting-fidelity
// @regression
func TestUnitPostedMessageFidelityRawBlocksAreExempt(t *testing.T) {
	post := &postMessageRecorder{}
	h := newAddMessageHandlerFixture(t, post)
	// Every reference would be refused if this path validated at all.
	mentions := &stubMentionDirectory{verify: map[string]error{
		"<@U0GONE99>": refNotFound("no such user"),
	}}
	h.mentions = mentions

	res, err := callAddMessage(t, h, map[string]any{
		"channel_id": "C0DEST",
		"text":       "fallback",
		"blocks":     mentionBearingBlocks,
	})
	require.NoError(t, err, "raw blocks must not be gated by mention validation")
	require.NotNil(t, res)
	require.Len(t, post.calls, 1)

	assert.Empty(t, mentions.verifyCalls, "raw blocks must never verify mentions")
	assert.Contains(t, post.calls[0].blocksJSON(t), `"user_id":"U0GONE99"`,
		"the caller's blocks must pass through unchanged")
}

// TestUnitDraftFidelityNoReferencesConsultsNothing covers functional spec §2.4
// criterion 6: a message with no references changes nothing about today's
// behaviour. No verification, no naming, no workspace traffic on the mention
// path — while the list formatting still happens, since the two halves of the
// feature must be independent.
//
// @spec: 005-draft-formatting-fidelity
// @regression
func TestUnitDraftFidelityNoReferencesConsultsNothing(t *testing.T) {
	store := &slackDraftStore{}
	mentions := &stubMentionDirectory{}
	h := newDraftFidelityFixture(t, store, mentions)

	res, err := callDraftMessage(t, h, map[string]any{
		"channel_id": "C0DEST",
		"text":       "Sprint plan:\n• ship the fix\n• tell the team\n",
	})
	require.NoError(t, err)
	require.Equal(t, draftActionCreated, decodeDraftActionResult(t, res).Action)

	assert.Empty(t, mentions.verifyCalls, "a message with no references must verify nothing")
	assert.Empty(t, mentions.nameCalls, "and must resolve no names on readback either")
	require.Len(t, store.createCalls, 1)
	assert.NotNil(t, firstRichTextList(singleRichTextBlock(t, store.lastCreatedBlocks(t))),
		"list formatting must still happen with no references present")
}

// handWrittenMentionDraft is a draft the user typed in Slack: it carries all
// three mention element types plus a broadcast element the server's own
// converter can never produce. It is the fixture for functional spec §2.5 —
// the assistant reading back a draft it did not write.
func handWrittenMentionDraft() edge.Draft {
	d := handWrittenDraft()
	d.ID = "Dr0MENTIONS1"
	d.Blocks = json.RawMessage(`[{"type":"rich_text","block_id":"hUM4N","elements":[{"type":"rich_text_section","elements":[` +
		`{"type":"text","text":"thanks "},{"type":"user","user_id":"U0DANA01"},` +
		`{"type":"text","text":" — posting in "},{"type":"channel","channel_id":"C0GEN001"},` +
		`{"type":"text","text":", cc "},{"type":"usergroup","usergroup_id":"S0ENG001"},` +
		`{"type":"text","text":" "},{"type":"broadcast","range":"here"}]}]}]`)
	return d
}

// TestUnitDraftFidelityReadbackNamesMentionsAndRestoresOriginal covers
// functional spec §2.5 criteria 1, 2, 3 and 5 as one flow, because that is how
// they are used: the assistant finds a hand-written draft, reads it back and
// sees WHO is tagged (not codes), overwrites it with the user's consent, and
// can still restore the original byte-for-byte from the displaced content.
//
// The last assertion is the important one: the readback is a display aid, and
// naming mentions in it must not have leaked into the authoritative blocks.
//
// @spec: 005-draft-formatting-fidelity
// @regression
func TestUnitDraftFidelityReadbackNamesMentionsAndRestoresOriginal(t *testing.T) {
	existing := handWrittenMentionDraft()
	originalNormalized, err := normalizeDraftBlocks(existing.Blocks)
	require.NoError(t, err)

	store := &slackDraftStore{drafts: []edge.Draft{existing}}
	mentions := &stubMentionDirectory{names: map[string]string{
		"<@U0DANA01>":         "@dana",
		"<#C0GEN001>":         "#general",
		"<!subteam^S0ENG001>": "@eng",
	}}
	h := newDraftFidelityFixture(t, store, mentions)

	// Step 1 (§2.5 criteria 1, 2, 3): read the human's draft back before
	// deciding whether to overwrite it.
	res1, err := callDraftMessage(t, h, map[string]any{
		"channel_id": "C0DEST",
		"text":       "irrelevant — this call must write nothing",
	})
	require.NoError(t, err)
	out1 := decodeDraftActionResult(t, res1)
	require.Equal(t, draftActionExistingFound, out1.Action)

	assert.Equal(t, "thanks @dana — posting in #general, cc @eng @here", out1.Draft.Text,
		"§2.5: the assistant must be able to see who it is about to overwrite tagging")
	for _, code := range []string{"U0DANA01", "C0GEN001", "S0ENG001"} {
		assert.NotContains(t, out1.Draft.Text, code, "the readback must not recite raw codes")
	}
	assert.Equal(t, string(originalNormalized), string(out1.Draft.BlocksJSON),
		"§2.5: the authoritative blocks must be the exact original, untouched by naming")

	// Step 2: the user consents; the overwrite hands back the displaced content.
	res2, err := callDraftMessage(t, h, map[string]any{
		"channel_id": "C0DEST",
		"text":       "Replacement:\n• first\n• second\n",
		"overwrite":  true,
	})
	require.NoError(t, err)
	out2 := decodeDraftActionResult(t, res2)
	require.Equal(t, draftActionReplaced, out2.Action)
	require.NotNil(t, out2.Displaced)
	assert.Equal(t, string(originalNormalized), string(out2.Displaced.BlocksJSON),
		"the displaced content must be the original blocks, byte-preserved")

	// Step 3 (§2.5 criterion 5): the original is restorable unchanged.
	res3, err := callDraftMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         string(out2.Displaced.BlocksJSON),
		"content_type": "application/json",
		"overwrite":    true,
	})
	require.NoError(t, err)
	out3 := decodeDraftActionResult(t, res3)
	require.Equal(t, draftActionReplaced, out3.Action)
	assert.Equal(t, string(originalNormalized), string(out3.Draft.BlocksJSON),
		"§2.5: the hand-written draft must come back exactly as it was — mentions as elements, not as names")

	require.Len(t, store.drafts, 1, "the whole flow must converge on the one draft")
	restored, err := normalizeDraftBlocks(store.drafts[0].Blocks)
	require.NoError(t, err)
	assert.Equal(t, string(originalNormalized), string(restored),
		"what is actually sitting in Slack after the restore must be the original")
}

// TestUnitDraftFidelityReadbackNeverOmitsUnnamedMention covers functional spec
// §2.5 criterion 4: when a name cannot be determined — a deactivated user, a
// workspace that could not be consulted — the reference is still shown in a
// form that makes clear something is there. Silently dropping it would hide a
// wrong tag from both the assistant and the user, which is the failure this
// criterion exists to forbid.
//
// @spec: 005-draft-formatting-fidelity
// @regression
func TestUnitDraftFidelityReadbackNeverOmitsUnnamedMention(t *testing.T) {
	store := &slackDraftStore{drafts: []edge.Draft{handWrittenMentionDraft()}}
	// Empty name table: every lookup misses.
	mentions := &stubMentionDirectory{}
	h := newDraftFidelityFixture(t, store, mentions)

	res, err := callDraftMessage(t, h, map[string]any{
		"channel_id": "C0DEST",
		"text":       "irrelevant — this call must write nothing",
	})
	require.NoError(t, err)
	out := decodeDraftActionResult(t, res)
	require.Equal(t, draftActionExistingFound, out.Action)

	assert.Equal(t, "thanks <@U0DANA01> — posting in <#C0GEN001>, cc <!subteam^S0ENG001> @here", out.Draft.Text,
		"§2.5: an unnameable reference must still be visible, never omitted")
	assert.Equal(t, []string{"<@U0DANA01>", "<#C0GEN001>", "<!subteam^S0ENG001>"}, mentions.nameCalls,
		"a broadcast has no directory entry and must cost no lookup")
	assert.Empty(t, store.updateCalls, "reading a draft back writes nothing")
}

// newDemoApiProvider builds the same network-free demo-mode provider the
// handler fixtures use, for tests that need a real mentionDirectory rather than
// the stub seam.
func newDemoApiProvider(t *testing.T) *provider.ApiProvider {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SLACK_MCP_XOXP_TOKEN", "demo")
	t.Setenv("SLACK_MCP_XOXB_TOKEN", "")
	t.Setenv("SLACK_MCP_XOXC_TOKEN", "")
	t.Setenv("SLACK_MCP_XOXD_TOKEN", "")
	t.Setenv("SLACK_MCP_USERS_CACHE", filepath.Join(dir, "users_cache.json"))
	t.Setenv("SLACK_MCP_CHANNELS_CACHE", filepath.Join(dir, "channels_cache.json"))
	t.Setenv("SLACK_MCP_CHANNEL_MEMBERS_CACHE", filepath.Join(dir, "members_cache.json"))

	ap := provider.New("stdio", zap.NewNop())
	ap.SkipCache()
	return ap
}

// TestUnitMentionDirectoryVerificationOutcomeClasses exercises the REAL
// verification mapping in mentionDirectory, not the stub the handler tests
// inject. Functional spec §2.4 criterion 3 rests entirely on this mapping: an
// answer of "does not exist" tells the assistant to rewrite the reference,
// while "could not be verified" tells it to retry — and getting the two the
// wrong way round makes the assistant destroy correct content.
//
// The user-group class is driven through the directory's own memoised-fetch
// state, which is what lets a real Slack error code reach the mapping without a
// network call. The user and channel classes cannot be driven the same way from
// this package — their live lookups go through provider.ApiProvider, whose
// Slack client is unexported and cannot be faked here — so they are covered for
// the no-client case only, where the guarantee that matters is the same: never
// report absence when nothing was consulted.
//
// @spec: 005-draft-formatting-fidelity
// @regression
func TestUnitMentionDirectoryVerificationOutcomeClasses(t *testing.T) {
	ap := newDemoApiProvider(t)
	ctx := context.Background()

	newDir := func() *mentionDirectory { return newMentionDirectory(ap, zap.NewNop()) }

	t.Run("user group present in the fetched list verifies", func(t *testing.T) {
		d := newDir()
		d.groupsFetched = true
		d.groups = map[string]string{"S0ENG001": "@eng"}

		assert.NoError(t, d.Verify(ctx, mentionRef{Kind: mentionUserGroup, ID: "S0ENG001"}))
		assert.Equal(t, "@eng", d.Name(ctx, mentionRef{Kind: mentionUserGroup, ID: "S0ENG001"}))
	})

	t.Run("user group absent from a fetched list does not exist", func(t *testing.T) {
		d := newDir()
		d.groupsFetched = true
		d.groups = map[string]string{"S0ENG001": "@eng"}

		err := d.Verify(ctx, mentionRef{Kind: mentionUserGroup, ID: "S0GONE01"})
		require.Error(t, err)
		assert.ErrorIs(t, err, errRefNotFound,
			"a successfully fetched list that lacks the ID is the one proof of absence")
		assert.NotErrorIs(t, err, errRefUnverifiable)
		assert.Contains(t, refDetail(err), "no such user group")

		// Negative results are memoised: flipping the underlying list must not
		// change the answer within one tool call.
		d.groups["S0GONE01"] = "@late"
		assert.ErrorIs(t, d.Verify(ctx, mentionRef{Kind: mentionUserGroup, ID: "S0GONE01"}), errRefNotFound,
			"the outcome must be memoised for the call's duration")
	})

	t.Run("failed user-group fetch is could-not-verify, per error code", func(t *testing.T) {
		cases := []struct {
			code   string
			detail string
		}{
			{"missing_scope", "usergroups:read"},
			{"paid_only", "unavailable on this workspace's plan"},
			{"not_allowed_token_type", "this token type cannot list user groups"},
			{"ratelimited", "ratelimited"},
		}
		for _, tc := range cases {
			t.Run(tc.code, func(t *testing.T) {
				d := newDir()
				d.groupsFetched = true
				d.groupsErr = slack.SlackErrorResponse{Err: tc.code}

				err := d.Verify(ctx, mentionRef{Kind: mentionUserGroup, ID: "S0ENG001"})
				require.Error(t, err)
				assert.ErrorIs(t, err, errRefUnverifiable,
					"a list that could not be fetched is never proof that the group is absent")
				assert.NotErrorIs(t, err, errRefNotFound)
				assert.Contains(t, refDetail(err), tc.detail)
			})
		}
	})

	t.Run("no Slack client is could-not-verify for every class", func(t *testing.T) {
		for _, ref := range []mentionRef{
			{Kind: mentionUser, ID: "U0AAAAA1"},
			{Kind: mentionChannel, ID: "C0AAAAA1"},
			{Kind: mentionUserGroup, ID: "S0AAAAA1"},
		} {
			d := newDir()
			err := d.Verify(ctx, ref)
			require.Error(t, err, "%s must not silently verify with no client", ref.canonicalLiteral())
			assert.ErrorIs(t, err, errRefUnverifiable,
				"%s: nothing was consulted, so absence was never established", ref.canonicalLiteral())
			assert.NotErrorIs(t, err, errRefNotFound)
			assert.Empty(t, d.Name(ctx, ref),
				"an unnameable reference must degrade to \"\" so the readback falls back to the literal")
		}
	})

	t.Run("a broadcast is never verified and never looked up", func(t *testing.T) {
		d := newDir()
		assert.NoError(t, d.Verify(ctx, mentionRef{Kind: mentionBroadcast, Range: "here"}))
		assert.Empty(t, d.Name(ctx, mentionRef{Kind: mentionBroadcast, Range: "here"}))
		assert.False(t, d.groupsFetched, "resolving a broadcast must cost no workspace call")
	})
}
