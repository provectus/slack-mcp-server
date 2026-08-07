package handler

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestAddMessageMarkdownRendersRichText is a regression guard for the
// conversations_add_message send path. It used to convert markdown with the
// external slack-go-util helper, which emitted section blocks that render bold,
// links and lists less faithfully (and could leak literal markdown markers).
// The send path now shares markdownToRichTextBlock with the drafts path, so a
// representative message must become a rich_text block with real inline styles
// and no literal "**"/"[]()" markup surviving in the text.
func TestAddMessageMarkdownRendersRichText(t *testing.T) {
	input := "Deploy **shipped** to prod. See the [runbook](https://example.com/run).\n\n- check dashboards\n- watch errors"

	rtb, err := markdownToRichTextBlock(input)
	if err != nil {
		t.Fatalf("markdownToRichTextBlock returned error: %v", err)
	}
	if rtb.Type != slack.MBTRichText {
		t.Fatalf("expected rich_text block, got %q", rtb.Type)
	}

	flat := flattenRichText(rtb)
	for _, marker := range []string{"**", "](http"} {
		if strings.Contains(flat, marker) {
			t.Errorf("literal markdown marker %q leaked into rendered text: %q", marker, flat)
		}
	}

	var foundBold, foundLink, foundList bool
	for _, el := range rtb.Elements {
		switch e := el.(type) {
		case *slack.RichTextSection:
			for _, se := range e.Elements {
				switch x := se.(type) {
				case *slack.RichTextSectionTextElement:
					if x.Style != nil && x.Style.Bold && strings.Contains(x.Text, "shipped") {
						foundBold = true
					}
				case *slack.RichTextSectionLinkElement:
					if x.URL == "https://example.com/run" {
						foundLink = true
					}
				}
			}
		case *slack.RichTextList:
			foundList = true
		}
	}

	if !foundBold {
		t.Error("expected a bold styled run for 'shipped'")
	}
	if !foundLink {
		t.Error("expected a link element with the runbook URL")
	}
	if !foundList {
		t.Error("expected a rich_text_list for the bullet items")
	}
}

// postMessageRecorder is an in-memory postMessageAPI that records every post,
// so the tests can prove not just what was sent but — for the mention refusal —
// that nothing was sent at all.
type postMessageRecorder struct {
	calls []postMessageCall
}

type postMessageCall struct {
	channel string
	options []slack.MsgOption
}

func (m *postMessageRecorder) PostMessageContext(_ context.Context, channelID string, options ...slack.MsgOption) (string, string, error) {
	m.calls = append(m.calls, postMessageCall{channel: channelID, options: options})
	return channelID, "1752000000.000100", nil
}

// blocksJSON renders the recorded call's message options the way slack-go would
// put them on the wire, so a test can inspect the blocks actually posted.
func (c postMessageCall) blocksJSON(t *testing.T) string {
	t.Helper()
	_, values, err := slack.UnsafeApplyMsgOptions("xoxc-test", c.channel, "https://slack.com/api/", c.options...)
	require.NoError(t, err)
	return values.Get("blocks")
}

// fallbackText renders the recorded call's message options the way slack-go
// would put them on the wire and returns the "text" field — the notification
// fallback Slack builds its alert from.
func (c postMessageCall) fallbackText(t *testing.T) string {
	t.Helper()
	_, values, err := slack.UnsafeApplyMsgOptions("xoxc-test", c.channel, "https://slack.com/api/", c.options...)
	require.NoError(t, err)
	return values.Get("text")
}

// newAddMessageHandlerFixture builds a ConversationsHandler for the add-message
// tool, backed by the same network-free demo-mode ApiProvider the drafts fixture
// uses, with the post seam pointed at the recorder.
func newAddMessageHandlerFixture(t *testing.T, post *postMessageRecorder) *ConversationsHandler {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("SLACK_MCP_XOXP_TOKEN", "demo")
	t.Setenv("SLACK_MCP_XOXB_TOKEN", "")
	t.Setenv("SLACK_MCP_XOXC_TOKEN", "")
	t.Setenv("SLACK_MCP_XOXD_TOKEN", "")
	t.Setenv("SLACK_MCP_USERS_CACHE", filepath.Join(dir, "users_cache.json"))
	t.Setenv("SLACK_MCP_CHANNELS_CACHE", filepath.Join(dir, "channels_cache.json"))
	t.Setenv("SLACK_MCP_CHANNEL_MEMBERS_CACHE", filepath.Join(dir, "members_cache.json"))
	t.Setenv("SLACK_MCP_ADD_MESSAGE_TOOL", "true")
	t.Setenv("SLACK_MCP_ADD_MESSAGE_MARK", "")

	ap := provider.New("stdio", zap.NewNop())
	ap.SkipCache()

	h := NewConversationsHandler(ap, zap.NewNop())
	h.postMessages = post
	return h
}

func callAddMessage(t *testing.T, h *ConversationsHandler, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	var req mcp.CallToolRequest
	req.Params.Name = "conversations_add_message"
	req.Params.Arguments = args
	return h.ConversationsAddMessageHandler(context.Background(), req)
}

// TestUnitAddMessageValidMentionPostsUserElement (functional spec §2.2, last
// criterion): the mention work applies to the direct post path too. A verified
// reference is posted as a real user element — which is what makes Slack render
// a live, notifying mention instead of a raw code.
func TestUnitAddMessageValidMentionPostsUserElement(t *testing.T) {
	post := &postMessageRecorder{}
	h := newAddMessageHandlerFixture(t, post)
	mentions := &stubMentionDirectory{} // no entry => verifies clean
	h.mentions = mentions

	res, err := callAddMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         "ping <@U0123456> about the release",
		"content_type": "text/markdown",
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Equal(t, []string{"<@U0123456>"}, mentions.verifyCalls,
		"the posted message's mentions must be verified before it is sent")

	require.Len(t, post.calls, 1)
	blocks := post.calls[0].blocksJSON(t)
	assert.Contains(t, blocks, `"type":"user"`, "the reference must post as a live user element")
	assert.Contains(t, blocks, `"user_id":"U0123456"`)
}

// TestUnitAddMessageUnresolvableMentionNeverPosts (functional spec §2.4): an
// unverifiable reference stops the post the same way it stops a draft. This is
// a hard return, not the conversion-error fallback to plain text — falling back
// would post the message with the raw code visible, which is the defect the
// gate exists to prevent.
func TestUnitAddMessageUnresolvableMentionNeverPosts(t *testing.T) {
	post := &postMessageRecorder{}
	h := newAddMessageHandlerFixture(t, post)
	h.mentions = &stubMentionDirectory{verify: map[string]error{
		"<#C0BOGUS1>": refNotFound("no such channel"),
	}}

	res, err := callAddMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         "see <#C0BOGUS1> for details",
		"content_type": "text/markdown",
	})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.Contains(t, err.Error(), "<#C0BOGUS1>", "the error must name the offending reference")
	assert.Contains(t, err.Error(), "does not exist in this workspace")
	assert.Contains(t, err.Error(), "nothing was written")

	assert.Empty(t, post.calls, "a refused mention must never reach PostMessageContext")
}

// TestUnitAddMessageBroadcastNeutralisedInFallbackText (functional spec §2.3):
// the converter already renders @here/@channel/@everyone as inert text elements,
// but the posted message also carries a notification fallback in its "text"
// field — and "<!here>" there is Slack's own encoding of a LIVE broadcast, the
// byte-identical form a genuine @here arrives in. Slack builds the notification
// from that field, so leaving the literal in it would leave the "the assistant
// can never raise a workspace-wide alert" guarantee resting on undocumented
// Slack behaviour. The fallback must carry the same inert "@here" the blocks do.
func TestUnitAddMessageBroadcastNeutralisedInFallbackText(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		literal string
		want    string
	}{
		{name: "here", text: "heads up <!here> deploy at noon", literal: "<!here>", want: "@here"},
		{name: "channel", text: "heads up <!channel> deploy at noon", literal: "<!channel>", want: "@channel"},
		{name: "everyone", text: "heads up <!everyone> deploy at noon", literal: "<!everyone>", want: "@everyone"},
		{name: "labelled here", text: "heads up <!here|@here> deploy at noon", literal: "<!here|@here>", want: "@here"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			post := &postMessageRecorder{}
			h := newAddMessageHandlerFixture(t, post)
			h.mentions = &stubMentionDirectory{}

			res, err := callAddMessage(t, h, map[string]any{
				"channel_id":   "C0DEST",
				"text":         tc.text,
				"content_type": "text/markdown",
			})
			require.NoError(t, err)
			require.NotNil(t, res)
			require.Len(t, post.calls, 1)

			fallback := post.calls[0].fallbackText(t)
			assert.NotContains(t, fallback, tc.literal,
				"the broadcast literal must not survive into the notification fallback")
			assert.NotContains(t, fallback, "<!",
				"no broadcast encoding of any form may remain in the fallback text")
			assert.Contains(t, fallback, tc.want,
				"the fallback must read the same inert text the blocks carry")
			assert.Contains(t, fallback, "deploy at noon",
				"the rest of the fallback text must be untouched")

			blocks := post.calls[0].blocksJSON(t)
			assert.NotContains(t, blocks, `"type":"broadcast"`,
				"the converter must never emit a live broadcast element")
			assert.Contains(t, blocks, tc.want,
				"the blocks must still carry the inert broadcast text element")
		})
	}
}

// TestUnitAddMessageFallbackKeepsNonBroadcastReferencesVerbatim: neutralisation
// is scoped to broadcasts alone. A user, channel or user-group reference is
// resolved server-side by Slack when it builds the notification, and that is
// wanted behaviour — rewriting those would silently degrade a legitimate
// mention's notification into plain text.
func TestUnitAddMessageFallbackKeepsNonBroadcastReferencesVerbatim(t *testing.T) {
	post := &postMessageRecorder{}
	h := newAddMessageHandlerFixture(t, post)
	h.mentions = &stubMentionDirectory{}

	res, err := callAddMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         "<!here> <@U0123456> please read <#C0123456> and <!subteam^S0123456>",
		"content_type": "text/markdown",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Len(t, post.calls, 1)

	fallback := post.calls[0].fallbackText(t)
	assert.NotContains(t, fallback, "<!here>", "only the broadcast is neutralised")
	assert.Contains(t, fallback, "@here")
	assert.Contains(t, fallback, "<@U0123456>", "a user reference must stay verbatim for the notification")
	assert.Contains(t, fallback, "<#C0123456>", "a channel reference must stay verbatim for the notification")
	assert.Contains(t, fallback, "<!subteam^S0123456>", "a user-group reference must stay verbatim")
}

// TestUnitAddMessageBroadcastNeutralisedOnConversionErrorPath (functional spec
// §2.3): when markdown conversion fails, the handler falls back to posting the
// source as a text-only message — no blocks at all. That is the WORST place for
// a raw "<!here>" to survive: on the success path the inert blocks at least sit
// alongside it, whereas here the text field is the entire message, and a live
// broadcast encoding in it raises a real workspace-wide alert. Neutralisation
// must therefore cover this path too, and since the text is also the visible
// body, the reader sees the same "@here" the blocks path renders.
func TestUnitAddMessageBroadcastNeutralisedOnConversionErrorPath(t *testing.T) {
	post := &postMessageRecorder{}
	h := newAddMessageHandlerFixture(t, post)
	h.mentions = &stubMentionDirectory{}
	// Force the conversion-error branch: no real markdown input reliably fails,
	// so the seam stands in for a converter that gave up.
	h.markdownToRichTextBlockFn = func(string) (*slack.RichTextBlock, error) {
		return nil, errors.New("synthetic conversion failure")
	}

	res, err := callAddMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         "heads up <!here> ping <@U0123456> deploy at noon",
		"content_type": "text/markdown",
	})
	require.NoError(t, err, "a conversion error must still fall back to a posted message")
	require.NotNil(t, res)
	require.Len(t, post.calls, 1)

	assert.Empty(t, post.calls[0].blocksJSON(t),
		"the conversion-error fallback posts no blocks, so its text is the whole message")

	fallback := post.calls[0].fallbackText(t)
	assert.NotContains(t, fallback, "<!here>",
		"a live broadcast encoding must never reach Slack's text field")
	assert.NotContains(t, fallback, "<!",
		"no broadcast encoding of any form may remain")
	assert.Contains(t, fallback, "@here",
		"the reader sees the same inert text the blocks path renders")
	assert.Contains(t, fallback, "<@U0123456>",
		"non-broadcast references stay verbatim on this path too")
	assert.Contains(t, fallback, "deploy at noon",
		"the rest of the message body must be untouched")
}

// TestUnitAddMessagePlainTextNeutralisesBroadcasts (functional spec §2.3): the
// text/plain branch is an "exempt from mention validation" path, and that
// exemption used to extend to broadcasts. It cannot.
//
// MsgOptionDisableMarkdown does NOT make "<!here>" inert. Confirmed live: a
// text/plain message carrying "<!here>" posted to a self-DM rendered as a live
// amber @here pill and fired a real mention notification, while the blocks-path
// message rendered "@here" as plain text. AC 2.3 is unconditional — "in any
// form" — so the literal is rewritten here too.
func TestUnitAddMessagePlainTextNeutralisesBroadcasts(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		literal string
		want    string
	}{
		{name: "here", text: "cc <!here> deploy at noon", literal: "<!here>", want: "@here"},
		{name: "channel", text: "cc <!channel> deploy at noon", literal: "<!channel>", want: "@channel"},
		{name: "everyone", text: "cc <!everyone> deploy at noon", literal: "<!everyone>", want: "@everyone"},
		{name: "labelled here", text: "cc <!here|@here> deploy at noon", literal: "<!here|@here>", want: "@here"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			post := &postMessageRecorder{}
			h := newAddMessageHandlerFixture(t, post)
			h.mentions = &stubMentionDirectory{}

			res, err := callAddMessage(t, h, map[string]any{
				"channel_id":   "C0DEST",
				"text":         tc.text,
				"content_type": "text/plain",
			})
			require.NoError(t, err)
			require.NotNil(t, res)
			require.Len(t, post.calls, 1)

			assert.Empty(t, post.calls[0].blocksJSON(t),
				"text/plain posts no blocks, so its text is the whole message")

			sent := post.calls[0].fallbackText(t)
			assert.Contains(t, sent, tc.want,
				"the reader must see the same inert text the blocks path renders")
			assert.NotContains(t, sent, tc.literal,
				"a live broadcast encoding must never reach Slack on the text/plain path")
			assert.NotContains(t, sent, "<!",
				"no broadcast encoding of any form may remain")
			assert.Contains(t, sent, "deploy at noon",
				"the rest of the literal text must be untouched")
		})
	}
}

// The scope of that neutralisation is broadcasts alone. text/plain still means
// literal for every other reference class — Slack resolves <@U…>, <#C…> and
// <!subteam^S…> server-side, and rewriting them would silently degrade a
// legitimate mention the caller asked to send verbatim.
func TestUnitAddMessagePlainTextKeepsNonBroadcastReferencesVerbatim(t *testing.T) {
	post := &postMessageRecorder{}
	h := newAddMessageHandlerFixture(t, post)
	mentions := &stubMentionDirectory{}
	h.mentions = mentions

	res, err := callAddMessage(t, h, map[string]any{
		"channel_id":   "C0DEST",
		"text":         "<!here> <@U0123456> please read <#C0123456> and <!subteam^S0123456>",
		"content_type": "text/plain",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Len(t, post.calls, 1)

	sent := post.calls[0].fallbackText(t)
	assert.NotContains(t, sent, "<!here>", "only the broadcast is neutralised")
	assert.Contains(t, sent, "@here")
	assert.Contains(t, sent, "<@U0123456>", "a person reference stays verbatim on the literal path")
	assert.Contains(t, sent, "<#C0123456>", "a channel reference stays verbatim on the literal path")
	assert.Contains(t, sent, "<!subteam^S0123456>", "a user-group reference stays verbatim on the literal path")

	assert.Empty(t, mentions.verifyCalls,
		"text/plain remains exempt from mention validation: literal means literal")
}
