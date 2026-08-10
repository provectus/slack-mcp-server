package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gocarina/gocsv"
	"github.com/korotovsky/slack-mcp-server/pkg/limiter"
	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"github.com/korotovsky/slack-mcp-server/pkg/provider/edge"
	"github.com/korotovsky/slack-mcp-server/pkg/server/auth"
	"github.com/korotovsky/slack-mcp-server/pkg/text"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

const (
	defaultConversationsNumericLimit    = 50
	defaultConversationsExpressionLimit = "1d"
	maxFileSizeBytes                    = 5 * 1024 * 1024 // 5MB limit
)

var validFilterKeys = map[string]struct{}{
	"is":     {},
	"in":     {},
	"from":   {},
	"with":   {},
	"before": {},
	"after":  {},
	"on":     {},
	"during": {},
	"has":    {},
	"hasmy":  {},
}

type Message struct {
	MsgID         string `json:"msgID"`
	UserID        string `json:"userID"`
	UserName      string `json:"userUser"`
	RealName      string `json:"realName"`
	Channel       string `json:"channelID"`
	ThreadTs      string `json:"ThreadTs"`
	Text          string `json:"text"`
	Time          string `json:"time"`
	Permalink     string `json:"permalink,omitempty"`
	Reactions     string `json:"reactions,omitempty"`
	BotName       string `json:"botName,omitempty"`
	FileCount     int    `json:"fileCount,omitempty"`
	AttachmentIDs string `json:"attachmentIDs,omitempty"`
	HasMedia      bool   `json:"hasMedia,omitempty"`
	Cursor        string `json:"cursor"`
}

type User struct {
	UserID   string `json:"userID"`
	UserName string `json:"userName"`
	RealName string `json:"realName"`
}

type UserSearchResult struct {
	UserID      string `csv:"UserID"`
	UserName    string `csv:"UserName"`
	RealName    string `csv:"RealName"`
	DisplayName string `csv:"DisplayName"`
	Email       string `csv:"Email"`
	Title       string `csv:"Title"`
	DMChannelID string `csv:"DMChannelID"`
}

type conversationParams struct {
	channel  string
	limit    int
	oldest   string
	latest   string
	cursor   string
	activity bool
}

type searchParams struct {
	query string
	limit int
	page  int
}

type addMessageParams struct {
	channel     string
	threadTs    string
	text        string
	contentType string
	blocks      []slack.Block
}

type draftMessageParams struct {
	channel     string
	threadTs    string
	text        string
	contentType string
	overwrite   bool
	draftID     string
}

type addReactionParams struct {
	channel   string
	timestamp string
	emoji     string
}

type filesGetParams struct {
	fileID string
}

type conversationsMarkParams struct {
	Channels []struct {
		ChannelID string `json:"channel_id"`
		Timestamp string `json:"timestamp"`
	} `json:"channels"`
}

type usersSearchParams struct {
	query string
	limit int
}

type unreadsParams struct {
	includeMessages       bool
	channelTypes          string
	maxChannels           int
	maxMessagesPerChannel int
	mentionsOnly          bool
	includeMuted          bool
	mutedChannels         map[string]bool // populated at runtime from Slack prefs
	mutedUnavailable      bool            // true when muted channels could not be fetched (e.g. xoxp token)
}

type ConversationsHandler struct {
	apiProvider *provider.ApiProvider
	logger      *zap.Logger
	// drafts overrides the drafts API used by the draft-message handler; nil
	// means use apiProvider.Slack(). Set only by tests — the narrow seam that
	// lets handler tests record draft calls without a network-backed client.
	drafts draftsAPI
	// markdownToRichTextBlockFn and draftContentLossFn override the markdown
	// pipeline of the draft-message and add-message handlers; nil means use the
	// real markdownToRichTextBlock / draftContentLoss. Set only by tests, so
	// they can prove the application/json content type bypasses conversion and
	// the loss check entirely (its input is already-valid Slack blocks, not
	// markdown), and reach the add-message conversion-error fallback, which no
	// real markdown input reliably triggers.
	markdownToRichTextBlockFn func(markdown string) (*slack.RichTextBlock, error)
	draftContentLossFn        func(input string, rtb *slack.RichTextBlock) []string
	// isOAuthTokenFn overrides the provider's token-type check in the
	// draft-message handler; nil means use apiProvider.IsOAuth(). Set only by
	// tests, to simulate a non-session (OAuth) token without a network-built
	// client.
	isOAuthTokenFn func() bool
	// mentions overrides the mention directory the handlers verify and name
	// mentions against; nil means build one from apiProvider. Set only by
	// tests, so validation and readback assertions run against a fixed table
	// instead of a live workspace. ApiProvider.client is unexported, so this
	// handler-level field — not a provider seam — is where a test observes
	// mention lookups.
	mentions mentionResolver
	// postMessages overrides the client conversations_add_message posts
	// through; nil means use apiProvider.Slack(). Set only by tests, mirroring
	// the drafts seam: it is what lets a test assert that a refused mention
	// posted nothing at all.
	postMessages postMessageAPI
}

func NewConversationsHandler(apiProvider *provider.ApiProvider, logger *zap.Logger) *ConversationsHandler {
	return &ConversationsHandler{
		apiProvider: apiProvider,
		logger:      logger,
	}
}

// UsersResource streams a CSV of all users
func (ch *ConversationsHandler) UsersResource(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	ch.logger.Debug("UsersResource called", zap.Any("params", request.Params))

	// authentication
	if authenticated, err := auth.IsAuthenticated(ctx, ch.apiProvider.ServerTransport(), ch.logger); !authenticated {
		ch.logger.Error("Authentication failed for users resource", zap.Error(err))
		return nil, err
	}

	// provider readiness
	if ready, err := ch.apiProvider.IsReady(); !ready {
		ch.logger.Error("API provider not ready", zap.Error(err))
		return nil, err
	}

	// Slack auth test
	ar, err := ch.apiProvider.Slack().AuthTest()
	if err != nil {
		ch.logger.Error("Slack AuthTest failed", zap.Error(err))
		return nil, err
	}

	ws, err := text.Workspace(ar.URL)
	if err != nil {
		ch.logger.Error("Failed to parse workspace from URL",
			zap.String("url", ar.URL),
			zap.Error(err),
		)
		return nil, fmt.Errorf("failed to parse workspace from URL: %v", err)
	}

	// collect users
	usersMaps := ch.apiProvider.ProvideUsersMap()
	users := usersMaps.Users
	usersList := make([]User, 0, len(users))
	for _, user := range users {
		usersList = append(usersList, User{
			UserID:   user.ID,
			UserName: user.Name,
			RealName: user.RealName,
		})
	}

	// marshal CSV
	csvBytes, err := gocsv.MarshalBytes(&usersList)
	if err != nil {
		ch.logger.Error("Failed to marshal users to CSV", zap.Error(err))
		return nil, err
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      "slack://" + ws + "/users",
			MIMEType: "text/csv",
			Text:     string(csvBytes),
		},
	}, nil
}

// ConversationsAddMessageHandler posts a message and returns a confirmation
func (ch *ConversationsHandler) ConversationsAddMessageHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ConversationsAddMessageHandler called", zap.Any("params", request.Params))

	// provider readiness
	if ready, err := ch.apiProvider.IsReady(); !ready {
		ch.logger.Error("API provider not ready", zap.Error(err))
		return nil, err
	}

	params, err := ch.parseParamsToolAddMessage(ctx, request)
	if err != nil {
		ch.logger.Error("Failed to parse add-message params", zap.Error(err))
		return nil, err
	}

	var options []slack.MsgOption
	if params.threadTs != "" {
		options = append(options, slack.MsgOptionTS(params.threadTs))
	}

	if params.blocks != nil {
		// Raw blocks provided: use them directly. If text is also provided, it
		// serves as the notification/fallback text. Caller-supplied blocks are a
		// verbatim path and are never mention-validated, for the same reason the
		// drafts tool exempts application/json.
		options = append(options, slack.MsgOptionBlocks(params.blocks...))
		if params.text != "" {
			options = append(options, slack.MsgOptionText(params.text, false))
		}
	} else {
		switch params.contentType {
		case "text/plain":
			// Exempt from mention validation: literal means literal — the text
			// carries no rich_text elements, so a <@U…>, <#C…> or <!subteam^S…>
			// here is resolved by Slack itself and passes through verbatim.
			//
			// Broadcasts are the one deliberate exception. MsgOptionDisableMarkdown
			// does NOT neutralise them: a text/plain message containing "<!here>"
			// was posted to a self-DM and came back rendered as a live amber @here
			// pill that fired a real mention notification — while the blocks path's
			// "@here" stayed inert text. Functional spec AC 2.3 is unconditional
			// ("in any form"), so the literal is rewritten to the same inert "@here"
			// the markdown path produces.
			options = append(options, slack.MsgOptionDisableMarkdown())
			options = append(options, slack.MsgOptionText(neutralizeBroadcasts(params.text), false))
		case "text/markdown":
			// Render markdown to a rich_text block, the same converter the drafts
			// path uses and the format Slack's native composer produces. Section
			// blocks (the previous approach) render bold/links/lists less
			// faithfully. params.text is also attached as the notification/fallback
			// text, exactly like the raw-blocks branch above.
			block, err := ch.convertMarkdownToRichText(params.text)
			if err != nil {
				ch.logger.Warn("Markdown parsing error", zap.Error(err))
				// The conversion failed, so there are no blocks: the fallback
				// text IS the whole message body. That makes neutralisation
				// MORE necessary here than on the success path below, not less
				// — a "<!here>" reaching Slack's text field with nothing
				// rendering it inert is a live workspace-wide broadcast, which
				// functional spec §2.3 forbids the assistant from raising. The
				// reader sees the same "@here" the blocks path produces, and
				// non-broadcast references still pass through verbatim.
				options = append(options, slack.MsgOptionDisableMarkdown())
				options = append(options, slack.MsgOptionText(neutralizeBroadcasts(params.text), false))
			} else {
				// A mention that cannot be verified stops the post, before
				// PostMessageContext runs (functional spec AC 2.4). This is a
				// hard return, NOT the plain-text fallback above: that fallback
				// exists for *conversion* errors, and reusing it here would post
				// the message with a raw "<@U…>" code in it — precisely the
				// defect this gate exists to prevent.
				if vErr := validateMentions(ctx, block, ch.mentionDir()); vErr != nil {
					ch.logger.Error("Message mention validation failed", zap.Error(vErr))
					return nil, fmt.Errorf("conversations_add_message: %w", vErr)
				}
				options = append(options, slack.MsgOptionBlocks(block))
				// The fallback carries the source markdown, minus its broadcast
				// literals: "<!here>" is Slack's own encoding of a live @here, and
				// the fallback text is what Slack builds the notification from. The
				// blocks already render broadcasts as inert "@here" text, so this
				// makes both halves of the message say the same inert thing. Other
				// reference classes stay verbatim on purpose — Slack resolving a
				// <@U…> for the notification is wanted.
				options = append(options, slack.MsgOptionText(neutralizeBroadcasts(params.text), false))
			}
		default:
			return nil, errors.New("content_type must be either 'text/plain' or 'text/markdown'")
		}
	}

	unfurlOpt := os.Getenv("SLACK_MCP_ADD_MESSAGE_UNFURLING")
	if text.IsUnfurlingEnabled(params.text, unfurlOpt, ch.logger) {
		options = append(options, slack.MsgOptionEnableLinkUnfurl())
	} else {
		options = append(options, slack.MsgOptionDisableLinkUnfurl())
		options = append(options, slack.MsgOptionDisableMediaUnfurl())
	}

	ch.logger.Debug("Posting Slack message",
		zap.String("channel", params.channel),
		zap.String("thread_ts", params.threadTs),
		zap.String("content_type", params.contentType),
	)
	respChannel, respTimestamp, err := ch.postMessageClient().PostMessageContext(ctx, params.channel, options...)
	if err != nil {
		ch.logger.Error("Slack PostMessageContext failed", zap.Error(err))
		return nil, err
	}

	toolConfig := os.Getenv("SLACK_MCP_ADD_MESSAGE_MARK")
	if toolConfig == "1" || toolConfig == "true" || toolConfig == "yes" {
		err := ch.apiProvider.Slack().MarkConversationContext(ctx, params.channel, respTimestamp)
		if err != nil {
			ch.logger.Error("Slack MarkConversationContext failed", zap.Error(err))
			return nil, err
		}
	}

	if params.threadTs != "" {
		return mcp.NewToolResultText(fmt.Sprintf("Successfully posted message to channel %s in thread %s (ts=%s)", respChannel, params.threadTs, respTimestamp)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Successfully posted message to channel %s (ts=%s)", respChannel, respTimestamp)), nil
}

// postMessageAPI is the narrow slice of the Slack client the add-message
// handler sends through. Like draftsAPI it exists as a seam so handler tests
// can record what was posted — and, on the mention-refusal path, prove that
// nothing was posted at all; production wiring falls through
// postMessageClient() to the real provider client, which already implements it.
type postMessageAPI interface {
	PostMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error)
}

// postMessageClient returns the client the add-message handler posts through:
// the injected test seam when set, otherwise the real provider client.
func (ch *ConversationsHandler) postMessageClient() postMessageAPI {
	if ch.postMessages != nil {
		return ch.postMessages
	}
	return ch.apiProvider.Slack()
}

// draftsAPI is the narrow slice of the Slack client the draft-message handler
// consumes. It exists as a seam so handler tests can record draft calls
// against an in-memory fake; production wiring falls through draftsClient()
// to the real provider client, which already implements it.
type draftsAPI interface {
	DraftsList(ctx context.Context, limit int) ([]edge.Draft, error)
	DraftsCreate(ctx context.Context, channelID, threadTs string, blocks json.RawMessage) (string, error)
	DraftsUpdate(ctx context.Context, draftID, clientMsgID, lastUpdatedTS, channelID, threadTs string, blocks json.RawMessage) error
}

// draftsClient returns the drafts API the handler talks to: the injected test
// seam when set, otherwise the real provider client.
func (ch *ConversationsHandler) draftsClient() draftsAPI {
	if ch.drafts != nil {
		return ch.drafts
	}
	return ch.apiProvider.Slack()
}

// convertMarkdownToRichText converts markdown to a rich_text block through the
// injected test seam when set, otherwise the real markdownToRichTextBlock.
func (ch *ConversationsHandler) convertMarkdownToRichText(markdown string) (*slack.RichTextBlock, error) {
	if ch.markdownToRichTextBlockFn != nil {
		return ch.markdownToRichTextBlockFn(markdown)
	}
	return markdownToRichTextBlock(markdown)
}

// checkDraftContentLoss runs the conversion loss check through the injected
// test seam when set, otherwise the real draftContentLoss.
func (ch *ConversationsHandler) checkDraftContentLoss(input string, rtb *slack.RichTextBlock) []string {
	if ch.draftContentLossFn != nil {
		return ch.draftContentLossFn(input, rtb)
	}
	return draftContentLoss(input, rtb)
}

// mentionDir returns the mention directory the handler verifies and names
// mentions against: the injected test seam when set, otherwise a directory
// built from the provider.
//
// Call it ONCE per tool call and pass the result to every consumer. The
// directory memoises lookups — including the single usergroups.list fetch —
// for its own lifetime, so a draft that mentions a user group costs one
// usergroups.list call when validation and readback share one directory, and
// two when they each build their own.
//
// A per-call directory (rather than a long-lived one) is equally deliberate:
// names resolve live, so a directory outliving the call would serve a renamed
// user's old handle indefinitely.
func (ch *ConversationsHandler) mentionDir() mentionResolver {
	if ch.mentions != nil {
		return ch.mentions
	}
	return newMentionDirectory(ch.apiProvider, ch.logger)
}

// tokenIsOAuth reports the provider's token-type check through the injected
// test seam when set, otherwise the real apiProvider.IsOAuth().
func (ch *ConversationsHandler) tokenIsOAuth() bool {
	if ch.isOAuthTokenFn != nil {
		return ch.isOAuthTokenFn()
	}
	return ch.apiProvider.IsOAuth()
}

// Values of draftActionResult.Action.
const (
	draftActionCreated       = "created"
	draftActionReplaced      = "replaced"
	draftActionExistingFound = "existing_draft_found"
	draftActionConflict      = "conflict"
)

// draftContent is one draft's content as reported to the caller: a best-effort
// readable rendering (display only), the normalised blocks (the authoritative
// representation, used for provenance comparison and exact restore), and the
// weak last_updated_client hint.
type draftContent struct {
	Text              string          `json:"text"`
	BlocksJSON        json.RawMessage `json:"blocks_json"`
	LastUpdatedClient string          `json:"last_updated_client"`
}

// draftActionResult is the JSON result of conversations_draft_message. DraftID
// is the id of the draft that now exists at the destination, confirmed by
// re-listing after every write — never an assumed id. Displaced is present
// only on "replaced" and carries the pre-update content so it can be shown to
// the user and restored. Note tells the calling agent what happened and what
// to do next, so the response is self-describing.
type draftActionResult struct {
	Action    string        `json:"action"`
	DraftID   string        `json:"draft_id"`
	ChannelID string        `json:"channel_id"`
	ThreadTS  string        `json:"thread_ts,omitempty"`
	Draft     draftContent  `json:"draft"`
	Displaced *draftContent `json:"displaced,omitempty"`
	Note      string        `json:"note"`
}

// Note texts for draftActionResult. The existing-draft note spells out the
// compare-then-consent protocol because the tool result is the only channel
// through which the calling agent can learn what to do next. It deliberately
// frames last_updated_client as a weak hint: this server reports as "Chrome"
// (utls UA mimicry), and so does a user browsing Slack in Chrome, so that
// field can never decide provenance — content comparison does.
const (
	draftNoteExistingFound = "An unsent draft already exists at this destination; nothing was written. Decide provenance by content, not by last_updated_client (a weak hint: this server and a user browsing Slack in Chrome both report \"Chrome\"): if draft.blocks_json is byte-identical to the blocks_json from your own last successful call for this destination, the draft is your own untouched work — re-call with overwrite=true. If it differs, or you never wrote here, show the user draft.text and get their explicit consent before re-calling with overwrite=true."
	draftNoteReplaced      = "Replaced the existing draft at this destination. displaced carries the overwritten content: its blocks_json restores it exactly (text is display-only, never a restore source). draft is what Slack now holds, confirmed by re-listing; memorise its blocks_json to recognise this draft as your own on future calls."
	draftNoteCreated       = "Created a new draft; no draft previously existed at this destination. draft is what Slack now holds, confirmed by re-listing; memorise its blocks_json to recognise this draft as your own on future calls."
	draftNoteConflict      = "The draft was edited by another client between listing and updating, so Slack rejected the write; the draft survives intact and nothing was written. draft carries its current content and the consent protocol restarts from it: if its blocks_json is byte-identical to your own last write, re-call with overwrite=true; otherwise show the user draft.text and get their explicit consent first. Do not blindly retry."

	// Mismatch variants: the confirming re-list found the draft, but its
	// content differs from what was just sent — propagation lag or a concurrent
	// edit. The note must not assert confirmation, and the agent must not
	// memorise draft.blocks_json as its own confirmed write.
	draftNoteReplacedMismatch = "Replaced the existing draft at this destination. displaced carries the overwritten content: its blocks_json restores it exactly (text is display-only, never a restore source). However, the confirming re-list returned content that differs from what was just sent — propagation lag or a concurrent edit by another client. draft is what the re-list returned; do not memorise its blocks_json as your own confirmed write. A later call for this destination will surface the draft's then-current content for comparison."
	draftNoteCreatedMismatch  = "Created a new draft; no draft previously existed at this destination. However, the confirming re-list returned content that differs from what was just sent — propagation lag or a concurrent edit by another client. draft is what the re-list returned; do not memorise its blocks_json as your own confirmed write. A later call for this destination will surface the draft's then-current content for comparison."

	// draftNoteListTruncated is appended to a create result's note when the
	// pre-write listing came back exactly at draftsListLimit: the window may
	// have hidden an existing draft at this destination, so "no draft
	// previously existed" cannot be asserted confidently.
	draftNoteListTruncated = "Caveat: the pre-write draft listing was truncated at the fetch limit, so an existing draft at this destination may have been missed and a duplicate created; check Slack's Drafts if exactly one draft matters here."
)

// draftsListLimit caps how many existing drafts we scan when looking for one to
// replace. Drafts are a per-user, small set, so this is generous in practice.
const draftsListLimit = 100

// normalizeTS strips trailing zeros from a Slack timestamp's fractional part so
// that two timestamps differing only in trailing-zero width compare equal.
// Without this, a thread_ts echoed by drafts.list in a different fractional
// width than the caller supplied would miss the match and create a duplicate.
func normalizeTS(ts string) string {
	i := strings.Index(ts, ".")
	if i < 0 {
		return ts
	}
	frac := strings.TrimRight(ts[i+1:], "0")
	if frac == "" {
		return ts[:i]
	}
	return ts[:i+1] + frac
}

// findDraftForDestination returns the active draft (if any) that targets the
// given channel and thread. Sent, deleted, and scheduled drafts are skipped so
// the upsert never clobbers a queued send or resurrects a removed draft. An
// empty threadTs matches a channel-level draft; a set threadTs matches only the
// draft for that exact thread (compared tolerant of trailing-zero ts widths).
func findDraftForDestination(drafts []edge.Draft, channel, threadTs string) (edge.Draft, bool) {
	wantThread := normalizeTS(threadTs)
	for _, d := range drafts {
		if d.IsSent || d.IsDeleted || d.DateScheduled != 0 {
			continue
		}
		for _, dest := range d.Destinations {
			if dest.ChannelID == channel && normalizeTS(dest.ThreadTS) == wantThread {
				return d, true
			}
		}
	}
	return edge.Draft{}, false
}

// draftTargetsDestination reports whether one of the draft's destinations is
// exactly the given channel and thread (tolerant of trailing-zero timestamp
// widths, like findDraftForDestination).
func draftTargetsDestination(d edge.Draft, channel, threadTs string) bool {
	wantThread := normalizeTS(threadTs)
	for _, dest := range d.Destinations {
		if dest.ChannelID == channel && normalizeTS(dest.ThreadTS) == wantThread {
			return true
		}
	}
	return false
}

// ConversationsDraftMessageHandler creates or replaces a native Slack draft
// under the compare-then-consent protocol and returns the JSON
// draftActionResult.
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

	// Defense in depth: registration already skips this tool for OAuth tokens
	// (xoxp/xoxb), but a misconfigured setup could still route a call here.
	// Drafts live behind the edge API, which only browser-session tokens can
	// reach — explain that rather than failing obscurely downstream.
	if ch.tokenIsOAuth() {
		ch.logger.Error("Draft-message tool called with a non-session token")
		return nil, errors.New("conversations_draft_message: drafts require a browser-session token (xoxc/xoxd); the configured token type cannot access Slack's drafts API")
	}

	// One directory for the whole call, shared by mention validation below and
	// by every readback further down. Building a second one would issue a
	// second usergroups.list for the same draft.
	mentions := ch.mentionDir()

	// drafts.create (the Slack message composer) only accepts rich_text blocks;
	// it silently drops section/header blocks. So, unlike conversations_add_message,
	// the draft body must be a single rich_text block — or, for
	// content_type application/json, a caller-supplied array of rich_text blocks.
	var richText *slack.RichTextBlock
	var blocksJSON json.RawMessage
	switch params.contentType {
	case "text/plain":
		// Exempt from mention validation: literal means literal. A "<@U123>" the
		// caller typed here is text, becomes no element, and notifies nobody.
		richText = plainRichTextBlock(params.text)
	case "text/markdown":
		converted, convErr := ch.convertMarkdownToRichText(params.text)
		if convErr != nil {
			ch.logger.Warn("Markdown parsing error, falling back to plain text", zap.Error(convErr))
			richText = plainRichTextBlock(params.text)
		} else if missing := ch.checkDraftContentLoss(params.text, converted); len(missing) > 0 {
			// Refuse to create a draft that would silently drop content; surface
			// the gap to the caller instead of producing a lossy draft.
			ch.logger.Error("Draft conversion dropped content",
				zap.Strings("missing_words", missing))
			return nil, fmt.Errorf("conversations_draft_message: refusing to create draft because markdown conversion dropped content (missing words: %s). Please report this input", strings.Join(missing, ", "))
		} else {
			// ORDERING CONTRACT (functional spec AC 2.4) — do not reorder.
			//
			// Mention validation runs here: after the loss check, and strictly
			// ABOVE the DraftsList call below. When it refuses, nothing has run
			// — not drafts.list, not drafts.create, not drafts.update — so a
			// draft already sitting at this destination is not read, not
			// listed, and categorically not modified. A refusal must never be
			// able to damage existing content.
			//
			// The loss check comes first because it is local and free while
			// validation may hit the network: a conversion bug should not cost
			// API calls.
			if vErr := validateMentions(ctx, converted, mentions); vErr != nil {
				ch.logger.Error("Draft mention validation failed", zap.Error(vErr))
				return nil, fmt.Errorf("conversations_draft_message: %w", vErr)
			}
			richText = converted
		}
	case "application/json":
		// Verbatim block JSON — the lossless restore path. The input is
		// already-valid Slack blocks, not markdown, so the markdown converter
		// and the loss check are bypassed entirely (the loss check would
		// false-positive on content that was never markdown).
		//
		// Mention validation is bypassed for the same reason, and it matters:
		// these blocks came from Slack itself, so a human-authored draft that
		// mentions a since-deactivated colleague would become permanently
		// unrestorable if it were validated — breaking the displaced.blocks_json
		// restore promise this content type exists to keep.
		verbatim, vErr := parseVerbatimDraftBlocks(json.RawMessage(params.text))
		if vErr != nil {
			ch.logger.Error("Invalid application/json draft blocks", zap.Error(vErr))
			return nil, fmt.Errorf("conversations_draft_message: %w", vErr)
		}
		blocksJSON = verbatim
	default:
		return nil, errors.New("content_type must be 'text/plain', 'text/markdown' or 'application/json'")
	}

	if blocksJSON == nil {
		blocksJSON, err = json.Marshal([]slack.Block{richText})
		if err != nil {
			ch.logger.Error("Failed to marshal blocks", zap.Error(err))
			return nil, err
		}
	}

	// Every call starts by listing drafts: the destination lookup is both the
	// non-destructive default's read path and, on an authorized overwrite, the
	// source of the concurrency token. Fail-safe invariant: a draft this
	// lookup misses is never overwritten — the miss lands in the create branch
	// and produces a visible, harmless duplicate.
	drafts := ch.draftsClient()
	listed, err := drafts.DraftsList(ctx, draftsListLimit)
	if err != nil {
		ch.logger.Error("Slack DraftsList failed", zap.Error(err))
		return nil, err
	}

	result := draftActionResult{
		ChannelID: params.channel,
		ThreadTS:  params.threadTs,
	}

	var existing edge.Draft
	var found bool
	if params.draftID != "" {
		// draft_id is targeting, not retargeting: the id must resolve within
		// the listed drafts and its destination must match the requested
		// channel/thread, which guarantees the channel policy check in
		// parseParamsToolDraftMessage covered the draft actually touched.
		existing, found = findDraftByID(listed, params.draftID)
		if !found {
			ch.logger.Warn("Requested draft_id not found among active drafts", zap.String("draft_id", params.draftID))
			return nil, fmt.Errorf("conversations_draft_message: draft %q was not found among your active drafts (it may be stale, sent or deleted); nothing was written", params.draftID)
		}
		if !draftTargetsDestination(existing, params.channel, params.threadTs) {
			ch.logger.Warn("Requested draft_id targets a different destination",
				zap.String("draft_id", params.draftID),
				zap.String("channel", params.channel),
				zap.String("thread_ts", params.threadTs),
			)
			return nil, fmt.Errorf("conversations_draft_message: draft %s targets a different destination than the requested channel/thread; draft_id targets a draft, it never retargets one — nothing was written", params.draftID)
		}
	} else {
		existing, found = findDraftForDestination(listed, params.channel, params.threadTs)
	}

	// A scheduled draft is a queued send and is never modified, regardless of
	// overwrite and regardless of how the draft was resolved:
	// findDraftForDestination skips scheduled drafts, and this guard keeps a
	// known draft_id from bypassing that skip.
	if found && existing.DateScheduled != 0 {
		ch.logger.Warn("Refusing to touch scheduled draft", zap.String("draft_id", existing.ID))
		return nil, fmt.Errorf("conversations_draft_message: draft %s is scheduled for sending and is never modified, regardless of overwrite; nothing was written", existing.ID)
	}

	// A draft addressed to more than one conversation is out of scope
	// (functional spec §3) and never touched or read: overwriting it would
	// silently drop the destinations beyond the requested one, and the channel
	// policy check only covered the requested channel — the other destinations
	// may be denied, so its content must not be returned either. Refuse before
	// any content payload is built, regardless of overwrite and regardless of
	// how the draft was resolved.
	if found && len(existing.Destinations) > 1 {
		ch.logger.Warn("Refusing to touch multi-destination draft",
			zap.String("draft_id", existing.ID),
			zap.Int("destinations", len(existing.Destinations)),
		)
		return nil, fmt.Errorf("conversations_draft_message: draft %s is addressed to more than one conversation; multi-destination drafts are out of scope and are never read or modified by this tool — manage it in Slack directly; nothing was written", existing.ID)
	}

	// Defensive: report the resolved draft's own matched destination thread_ts
	// rather than echoing the request. Identical by construction today — a
	// destination match is a precondition on every path that reports an
	// existing draft — but if the matcher were ever loosened, the result must
	// describe the draft actually resolved, not the request.
	if found {
		wantThread := normalizeTS(params.threadTs)
		for _, dest := range existing.Destinations {
			if dest.ChannelID == params.channel && normalizeTS(dest.ThreadTS) == wantThread {
				result.ThreadTS = dest.ThreadTS
				break
			}
		}
	}

	if found && !params.overwrite {
		// Non-destructive default: a draft already sits at this destination
		// and the caller has not asserted overwrite — write nothing and hand
		// back the existing content so the agent can run compare-then-consent.
		// This is a successful tool result, not an error: the caller must
		// receive structured content (blocks_json to compare, text to show
		// the user) and act on it.
		content, cErr := ch.draftContentPayload(ctx, mentions, existing)
		if cErr != nil {
			ch.logger.Error("Failed to decode existing draft content", zap.Error(cErr))
			return nil, fmt.Errorf("conversations_draft_message: a draft (%s) already exists at this destination and nothing was written, but its content could not be decoded for review: %w", existing.ID, cErr)
		}
		ch.logger.Debug("Refusing to overwrite existing Slack draft",
			zap.String("draft_id", existing.ID),
			zap.String("channel", params.channel),
			zap.String("thread_ts", params.threadTs),
		)
		result.Action = draftActionExistingFound
		result.DraftID = existing.ID
		result.Draft = content
		result.Note = draftNoteExistingFound
		return marshalDraftActionResult(result)
	}

	if found {
		// Authorized overwrite. Capture the displaced content before touching
		// anything: if it cannot be decoded it cannot be shown or restored,
		// so the update must not happen.
		displaced, cErr := ch.draftContentPayload(ctx, mentions, existing)
		if cErr != nil {
			ch.logger.Error("Failed to decode draft content before overwrite", zap.Error(cErr))
			return nil, fmt.Errorf("conversations_draft_message: refusing to overwrite draft %s because its current content could not be decoded for the displaced report: %w", existing.ID, cErr)
		}
		ch.logger.Debug("Replacing existing Slack draft",
			zap.String("draft_id", existing.ID),
			zap.String("channel", params.channel),
			zap.String("thread_ts", params.threadTs),
			zap.String("content_type", params.contentType),
		)
		if err := drafts.DraftsUpdate(ctx, existing.ID, existing.ClientMsgID, existing.LastUpdatedTS, params.channel, params.threadTs, blocksJSON); err != nil {
			if errors.Is(err, edge.ErrDraftConflict) {
				return ch.draftConflictResult(ctx, drafts, mentions, existing.ID, result)
			}
			ch.logger.Error("Slack DraftsUpdate failed", zap.Error(err))
			return nil, err
		}
		confirmed, err := ch.confirmDraftWrite(ctx, drafts, existing.ID, fmt.Sprintf("update of draft %s", existing.ID))
		if err != nil {
			return nil, err
		}
		content, cErr := ch.draftContentPayload(ctx, mentions, confirmed)
		if cErr != nil {
			ch.logger.Error("Failed to decode confirmed draft content", zap.Error(cErr))
			return nil, fmt.Errorf("conversations_draft_message: update of draft %s succeeded, but the confirming re-list returned content that could not be decoded: %w; verify the draft state in Slack", existing.ID, cErr)
		}
		result.Action = draftActionReplaced
		result.DraftID = confirmed.ID
		result.Draft = content
		result.Displaced = &displaced
		result.Note = draftNoteForWrite(blocksJSON, content.BlocksJSON, draftNoteReplaced, draftNoteReplacedMismatch)
		return marshalDraftActionResult(result)
	}

	// No draft at the destination — create, even when overwrite=true: the
	// assertion permits replacing, it does not require something to replace.
	ch.logger.Debug("Creating Slack draft",
		zap.String("channel", params.channel),
		zap.String("thread_ts", params.threadTs),
		zap.String("content_type", params.contentType),
	)
	createdID, err := drafts.DraftsCreate(ctx, params.channel, params.threadTs, blocksJSON)
	if err != nil {
		ch.logger.Error("Slack DraftsCreate failed", zap.Error(err))
		return nil, err
	}
	confirmed, err := ch.confirmDraftWrite(ctx, drafts, createdID, fmt.Sprintf("creation of draft %s", createdID))
	if err != nil {
		return nil, err
	}
	content, cErr := ch.draftContentPayload(ctx, mentions, confirmed)
	if cErr != nil {
		ch.logger.Error("Failed to decode confirmed draft content", zap.Error(cErr))
		return nil, fmt.Errorf("conversations_draft_message: creation of draft %s succeeded, but the confirming re-list returned content that could not be decoded: %w; verify the draft state in Slack", createdID, cErr)
	}
	result.Action = draftActionCreated
	result.DraftID = confirmed.ID
	result.Draft = content
	result.Note = draftNoteForWrite(blocksJSON, content.BlocksJSON, draftNoteCreated, draftNoteCreatedMismatch)
	if len(listed) == draftsListLimit {
		// The pre-write listing filled the whole window, so the destination
		// lookup may have missed an existing draft beyond it — the note must
		// not assert "no draft previously existed" confidently.
		result.Note += " " + draftNoteListTruncated
	}
	return marshalDraftActionResult(result)
}

// draftNoteForWrite picks the note for a confirmed write: the confirming
// re-list only proves the draft exists, so confirmation of *content* is a
// byte comparison of the normalised sent blocks against the normalised
// as-stored blocks (storedNormalized comes from draftContentPayload, already
// normalised). Equal → the confirmed note may assert the draft is what was
// sent; different → the mismatch note, which must not assert confirmation.
func draftNoteForWrite(sentBlocks, storedNormalized json.RawMessage, confirmedNote, mismatchNote string) string {
	sentNormalized, err := normalizeDraftBlocks(sentBlocks)
	if err != nil || !bytes.Equal(sentNormalized, storedNormalized) {
		return mismatchNote
	}
	return confirmedNote
}

// draftContentPayload normalises a listed draft's blocks and renders the
// readable text for the result payload. A draft whose blocks are absent or
// null (a listed draft with no content) is reported as empty content rather
// than an error — otherwise the refusal path would dead-end in a bare error
// the agent can neither review nor consent to.
//
// It takes a ctx and the call's mention directory because rendering names
// mentions against the workspace; the directory is the caller's, so the
// usergroups.list fetch is shared with mention validation rather than repeated.
// Only Text is affected: BlocksJSON stays the normalised as-stored bytes, which
// is what provenance comparison and restore use.
func (ch *ConversationsHandler) draftContentPayload(ctx context.Context, namer mentionNamer, d edge.Draft) (draftContent, error) {
	if trimmed := bytes.TrimSpace(d.Blocks); len(trimmed) == 0 || string(trimmed) == "null" {
		return draftContent{
			Text:              "",
			BlocksJSON:        json.RawMessage("[]"),
			LastUpdatedClient: d.LastUpdatedClient,
		}, nil
	}
	normalized, err := normalizeDraftBlocks(d.Blocks)
	if err != nil {
		return draftContent{}, fmt.Errorf("normalize blocks of draft %s: %w", d.ID, err)
	}
	return draftContent{
		Text:              renderDraftBlocksText(ctx, normalized, namer),
		BlocksJSON:        normalized,
		LastUpdatedClient: d.LastUpdatedClient,
	}, nil
}

// findDraftByID returns the listed draft with the given id, skipping sent and
// deleted entries.
func findDraftByID(drafts []edge.Draft, id string) (edge.Draft, bool) {
	for _, d := range drafts {
		if d.ID == id && !d.IsSent && !d.IsDeleted {
			return d, true
		}
	}
	return edge.Draft{}, false
}

// confirmDraftWrite re-lists drafts after a successful create/update and
// returns the as-stored entry for draftID. The re-list — not the write call's
// response — is what the result reports, so what the agent memorises for
// future comparison is what Slack actually holds. writeDesc names the write
// that succeeded, for the honest error when confirmation fails.
func (ch *ConversationsHandler) confirmDraftWrite(ctx context.Context, drafts draftsAPI, draftID, writeDesc string) (edge.Draft, error) {
	listed, err := drafts.DraftsList(ctx, draftsListLimit)
	if err != nil {
		ch.logger.Error("Confirming DraftsList failed after draft write", zap.String("draft_id", draftID), zap.Error(err))
		return edge.Draft{}, fmt.Errorf("conversations_draft_message: %s succeeded, but the confirming drafts.list failed: %v; verify the draft state in Slack before retrying", writeDesc, err)
	}
	d, ok := findDraftByID(listed, draftID)
	if !ok {
		ch.logger.Error("Confirming re-list could not find written draft", zap.String("draft_id", draftID))
		return edge.Draft{}, fmt.Errorf("conversations_draft_message: %s succeeded, but the confirming re-list could not find the draft; verify the draft state in Slack before retrying", writeDesc)
	}
	return d, nil
}

// draftConflictResult handles edge.ErrDraftConflict from drafts.update: the
// draft was edited by another client between our listing and our write, so
// Slack rejected the update and the draft survives intact. The write is never
// retried — the content the caller compared against is now stale. Instead the
// draft is re-listed and its current content returned as a successful
// "conflict" result, restarting the consent protocol from fresh state.
func (ch *ConversationsHandler) draftConflictResult(ctx context.Context, drafts draftsAPI, namer mentionNamer, draftID string, result draftActionResult) (*mcp.CallToolResult, error) {
	ch.logger.Warn("Draft was edited concurrently; Slack rejected the update", zap.String("draft_id", draftID))
	listed, err := drafts.DraftsList(ctx, draftsListLimit)
	if err != nil {
		ch.logger.Error("Re-list after draft conflict failed", zap.String("draft_id", draftID), zap.Error(err))
		return nil, fmt.Errorf("conversations_draft_message: draft %s was edited by another client and the update was rejected (the draft survives intact), but re-listing its current content failed: %v", draftID, err)
	}
	current, ok := findDraftByID(listed, draftID)
	if !ok {
		ch.logger.Error("Re-list after draft conflict could not find the draft", zap.String("draft_id", draftID))
		return nil, fmt.Errorf("conversations_draft_message: draft %s was edited by another client and the update was rejected, but the re-list could not find it; verify the draft state in Slack", draftID)
	}
	content, cErr := ch.draftContentPayload(ctx, namer, current)
	if cErr != nil {
		ch.logger.Error("Failed to decode conflicting draft content", zap.Error(cErr))
		return nil, fmt.Errorf("conversations_draft_message: draft %s was edited by another client and the update was rejected, but its current content could not be decoded: %w", draftID, cErr)
	}
	result.Action = draftActionConflict
	result.DraftID = current.ID
	result.Draft = content
	result.Note = draftNoteConflict
	return marshalDraftActionResult(result)
}

// marshalDraftActionResult renders the JSON tool result for the draft handler.
func marshalDraftActionResult(r draftActionResult) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(b)), nil
}

// ConversationsMarkHandler marks one or more conversations as read
func (ch *ConversationsHandler) ConversationsMarkHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ConversationsMarkHandler called", zap.Any("params", request.Params))

	if ready, err := ch.apiProvider.IsReady(); !ready {
		ch.logger.Error("API provider not ready", zap.Error(err))
		return nil, err
	}

	args := request.GetArguments()
	rawChannels, ok := args["channels"]
	if !ok {
		return nil, errors.New("channels is required")
	}

	jsonBytes, err := json.Marshal(rawChannels)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal channels argument: %w", err)
	}

	var params conversationsMarkParams
	if err := json.Unmarshal(jsonBytes, &params.Channels); err != nil {
		return nil, fmt.Errorf("failed to parse channels argument: %w", err)
	}

	if len(params.Channels) == 0 {
		return nil, errors.New("channels must contain at least one entry")
	}

	var results []string
	for _, entry := range params.Channels {
		if entry.ChannelID == "" {
			results = append(results, fmt.Sprintf("SKIP: missing channel_id"))
			continue
		}
		if !strings.Contains(entry.Timestamp, ".") {
			results = append(results, fmt.Sprintf("SKIP %s: invalid timestamp %q (must be in format 1234567890.123456)", entry.ChannelID, entry.Timestamp))
			continue
		}

		channelID, err := ch.resolveChannelID(ctx, entry.ChannelID)
		if err != nil {
			ch.logger.Error("Failed to resolve channel", zap.String("channel", entry.ChannelID), zap.Error(err))
			results = append(results, fmt.Sprintf("FAIL %s: %v", entry.ChannelID, err))
			continue
		}

		err = ch.apiProvider.Slack().MarkConversationContext(ctx, channelID, entry.Timestamp)
		if err != nil {
			ch.logger.Error("MarkConversationContext failed", zap.String("channel", channelID), zap.String("timestamp", entry.Timestamp), zap.Error(err))
			results = append(results, fmt.Sprintf("FAIL %s: %v", entry.ChannelID, err))
			continue
		}

		results = append(results, fmt.Sprintf("OK %s: marked as read up to %s", entry.ChannelID, entry.Timestamp))
	}

	return mcp.NewToolResultText(strings.Join(results, "\n")), nil
}

// ReactionsAddHandler adds an emoji reaction to a message
func (ch *ConversationsHandler) ReactionsAddHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ReactionsAddHandler called", zap.Any("params", request.Params))

	// provider readiness
	if ready, err := ch.apiProvider.IsReady(); !ready {
		ch.logger.Error("API provider not ready", zap.Error(err))
		return nil, err
	}

	params, err := ch.parseParamsToolReaction(ctx, request)
	if err != nil {
		ch.logger.Error("Failed to parse add-reaction params", zap.Error(err))
		return nil, err
	}

	itemRef := slack.ItemRef{
		Channel:   params.channel,
		Timestamp: params.timestamp,
	}

	ch.logger.Debug("Adding reaction to Slack message",
		zap.String("channel", params.channel),
		zap.String("timestamp", params.timestamp),
		zap.String("emoji", params.emoji),
	)

	err = ch.apiProvider.Slack().AddReactionContext(ctx, params.emoji, itemRef)
	if err != nil {
		ch.logger.Error("Slack AddReactionContext failed", zap.Error(err))
		return nil, err
	}

	return mcp.NewToolResultText(fmt.Sprintf("Successfully added :%s: reaction to message %s in channel %s", params.emoji, params.timestamp, params.channel)), nil
}

// ReactionsRemoveHandler removes an emoji reaction from a message
func (ch *ConversationsHandler) ReactionsRemoveHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ReactionsRemoveHandler called", zap.Any("params", request.Params))

	// provider readiness
	if ready, err := ch.apiProvider.IsReady(); !ready {
		ch.logger.Error("API provider not ready", zap.Error(err))
		return nil, err
	}

	params, err := ch.parseParamsToolReaction(ctx, request)
	if err != nil {
		ch.logger.Error("Failed to parse remove-reaction params", zap.Error(err))
		return nil, err
	}

	itemRef := slack.ItemRef{
		Channel:   params.channel,
		Timestamp: params.timestamp,
	}

	ch.logger.Debug("Removing reaction from Slack message",
		zap.String("channel", params.channel),
		zap.String("timestamp", params.timestamp),
		zap.String("emoji", params.emoji),
	)

	err = ch.apiProvider.Slack().RemoveReactionContext(ctx, params.emoji, itemRef)
	if err != nil {
		ch.logger.Error("Slack RemoveReactionContext failed", zap.Error(err))
		return nil, err
	}

	return mcp.NewToolResultText(fmt.Sprintf("Successfully removed :%s: reaction from message %s in channel %s", params.emoji, params.timestamp, params.channel)), nil
}

func (ch *ConversationsHandler) UsersSearchHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("UsersSearchHandler called", zap.Any("params", request.Params))

	if ready, err := ch.apiProvider.IsReady(); !ready {
		ch.logger.Error("API provider not ready", zap.Error(err))
		return nil, err
	}

	params, err := ch.parseParamsToolUsersSearch(request)
	if err != nil {
		ch.logger.Error("Failed to parse users-search params", zap.Error(err))
		return nil, err
	}

	ch.logger.Debug("Searching for users",
		zap.String("query", params.query),
		zap.Int("limit", params.limit),
	)

	users, err := ch.apiProvider.SearchUsers(ctx, params.query, params.limit)
	if err != nil {
		ch.logger.Error("UsersSearch failed", zap.Error(err))
		return nil, fmt.Errorf("users search failed: %w", err)
	}

	channelsMap := ch.apiProvider.ProvideChannelsMaps()

	results := make([]UserSearchResult, 0, len(users))
	for _, user := range users {
		if user.Deleted {
			continue
		}

		dmChannelID := ""
		for _, ch := range channelsMap.Channels {
			if ch.IsIM && ch.User == user.ID {
				dmChannelID = ch.ID
				break
			}
		}

		results = append(results, UserSearchResult{
			UserID:      user.ID,
			UserName:    user.Name,
			RealName:    user.RealName,
			DisplayName: user.Profile.DisplayName,
			Email:       user.Profile.Email,
			Title:       user.Profile.Title,
			DMChannelID: dmChannelID,
		})
	}

	if len(results) == 0 {
		return mcp.NewToolResultText("No users found matching the query."), nil
	}

	csvBytes, err := gocsv.MarshalBytes(&results)
	if err != nil {
		ch.logger.Error("Failed to marshal users to CSV", zap.Error(err))
		return nil, err
	}

	return mcp.NewToolResultText(string(csvBytes)), nil
}

func (ch *ConversationsHandler) FilesGetHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("FilesGetHandler called", zap.Any("params", request.Params))

	if ready, err := ch.apiProvider.IsReady(); !ready {
		ch.logger.Error("API provider not ready", zap.Error(err))
		return nil, err
	}

	params, err := ch.parseParamsToolFilesGet(request)
	if err != nil {
		ch.logger.Error("Failed to parse attachment_get_data params", zap.Error(err))
		return nil, err
	}

	fileInfo, _, _, err := ch.apiProvider.Slack().GetFileInfoContext(ctx, params.fileID, 0, 0)
	if err != nil {
		ch.logger.Error("Slack GetFileInfoContext failed", zap.Error(err))
		return nil, err
	}

	if fileInfo.Size > maxFileSizeBytes {
		return nil, fmt.Errorf("file size %d bytes exceeds maximum allowed size of %d bytes", fileInfo.Size, maxFileSizeBytes)
	}

	var buf bytes.Buffer
	downloadURL := fileInfo.URLPrivateDownload
	if downloadURL == "" {
		downloadURL = fileInfo.URLPrivate
	}
	if downloadURL == "" {
		return nil, errors.New("file has no downloadable URL")
	}

	err = ch.apiProvider.Slack().GetFileContext(ctx, downloadURL, &buf)
	if err != nil {
		ch.logger.Error("Slack GetFileContext failed", zap.Error(err))
		return nil, err
	}

	content := buf.Bytes()

	// For image files, return as native MCP image content so the client
	// can render them directly without base64-in-JSON overflow.
	if isImageMimetype(fileInfo.Mimetype) {
		imageData := base64.StdEncoding.EncodeToString(content)
		metadata := fmt.Sprintf(`{"file_id":"%s","filename":"%s","mimetype":"%s","size":%d}`,
			fileInfo.ID,
			escapeJSON(fileInfo.Name),
			escapeJSON(fileInfo.Mimetype),
			len(content))
		return mcp.NewToolResultImage(metadata, imageData, fileInfo.Mimetype), nil
	}

	encoding := "none"
	var contentStr string

	if isTextMimetype(fileInfo.Mimetype) {
		contentStr = string(content)
	} else {
		contentStr = base64.StdEncoding.EncodeToString(content)
		encoding = "base64"
	}

	result := fmt.Sprintf(`{"file_id":"%s","filename":"%s","mimetype":"%s","size":%d,"encoding":"%s","content":"%s"}`,
		fileInfo.ID,
		escapeJSON(fileInfo.Name),
		escapeJSON(fileInfo.Mimetype),
		len(content),
		encoding,
		escapeJSON(contentStr))

	return mcp.NewToolResultText(result), nil
}

func isImageMimetype(mimetype string) bool {
	return strings.HasPrefix(mimetype, "image/")
}

func isTextMimetype(mimetype string) bool {
	if strings.HasPrefix(mimetype, "text/") {
		return true
	}
	textMimetypes := map[string]bool{
		"application/json":       true,
		"application/xml":        true,
		"application/javascript": true,
		"application/x-yaml":     true,
		"application/x-sh":       true,
	}
	return textMimetypes[mimetype]
}

func escapeJSON(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}

// ConversationsHistoryHandler streams conversation history as CSV
func (ch *ConversationsHandler) ConversationsHistoryHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ConversationsHistoryHandler called", zap.Any("params", request.Params))

	params, err := ch.parseParamsToolConversations(ctx, request)
	if err != nil {
		ch.logger.Error("Failed to parse history params", zap.Error(err))
		return nil, err
	}
	ch.logger.Debug("History params parsed",
		zap.String("channel", params.channel),
		zap.Int("limit", params.limit),
		zap.String("oldest", params.oldest),
		zap.String("latest", params.latest),
		zap.Bool("include_activity", params.activity),
	)

	historyParams := slack.GetConversationHistoryParameters{
		ChannelID: params.channel,
		Limit:     params.limit,
		Oldest:    params.oldest,
		Latest:    params.latest,
		Cursor:    params.cursor,
		Inclusive: false,
	}
	history, err := ch.apiProvider.Slack().GetConversationHistoryContext(ctx, &historyParams)
	if err != nil {
		ch.logger.Error("GetConversationHistoryContext failed", zap.Error(err))
		return nil, err
	}

	ch.logger.Debug("Fetched conversation history", zap.Int("message_count", len(history.Messages)))

	messages := ch.convertMessagesFromHistory(ctx, history.Messages, params.channel, params.activity)

	if len(messages) > 0 && history.HasMore {
		messages[len(messages)-1].Cursor = history.ResponseMetaData.NextCursor
	}
	return marshalMessagesToCSV(messages)
}

// ConversationsRepliesHandler streams thread replies as CSV
func (ch *ConversationsHandler) ConversationsRepliesHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ConversationsRepliesHandler called", zap.Any("params", request.Params))

	params, err := ch.parseParamsToolConversations(ctx, request)
	if err != nil {
		ch.logger.Error("Failed to parse replies params", zap.Error(err))
		return nil, err
	}
	threadTs := request.GetString("thread_ts", "")
	if threadTs == "" {
		ch.logger.Error("thread_ts not provided for replies", zap.String("thread_ts", threadTs))
		return nil, errors.New("thread_ts must be a string")
	}

	repliesParams := slack.GetConversationRepliesParameters{
		ChannelID: params.channel,
		Timestamp: threadTs,
		Limit:     params.limit,
		Oldest:    params.oldest,
		Latest:    params.latest,
		Cursor:    params.cursor,
		Inclusive: false,
	}
	replies, hasMore, nextCursor, err := ch.apiProvider.Slack().GetConversationRepliesContext(ctx, &repliesParams)
	if err != nil {
		ch.logger.Error("GetConversationRepliesContext failed", zap.Error(err))
		return nil, err
	}
	ch.logger.Debug("Fetched conversation replies", zap.Int("count", len(replies)))

	messages := ch.convertMessagesFromHistory(ctx, replies, params.channel, params.activity)
	if len(messages) > 0 && hasMore {
		messages[len(messages)-1].Cursor = nextCursor
	}
	return marshalMessagesToCSV(messages)
}

func (ch *ConversationsHandler) ConversationsSearchHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ConversationsSearchHandler called", zap.Any("params", request.Params))

	params, err := ch.parseParamsToolSearch(ctx, request)
	if err != nil {
		ch.logger.Error("Failed to parse search params", zap.Error(err))
		return nil, err
	}
	ch.logger.Debug("Search params parsed", zap.String("query", params.query), zap.Int("limit", params.limit), zap.Int("page", params.page))

	searchParams := slack.SearchParameters{
		Sort:          slack.DEFAULT_SEARCH_SORT,
		SortDirection: slack.DEFAULT_SEARCH_SORT_DIR,
		Highlight:     false,
		Count:         params.limit,
		Page:          params.page,
	}

	rl := limiter.Tier2.Limiter()
	messagesRes, err := limiter.CallWithRetry(ctx, rl, 2, slackRetryAfter, func() (*slack.SearchMessages, error) {
		msgs, _, err := ch.apiProvider.Slack().SearchContext(ctx, params.query, searchParams)
		return msgs, err
	})
	if err != nil {
		ch.logger.Error("Slack SearchContext failed", zap.Error(err))
		return nil, err
	}
	ch.logger.Debug("Search completed", zap.Int("matches", len(messagesRes.Matches)))

	messages := ch.convertMessagesFromSearch(ctx, messagesRes.Matches)
	if len(messages) > 0 && messagesRes.Pagination.Page < messagesRes.Pagination.PageCount {
		nextCursor := fmt.Sprintf("page:%d", messagesRes.Pagination.Page+1)
		messages[len(messages)-1].Cursor = base64.StdEncoding.EncodeToString([]byte(nextCursor))
	}
	return marshalMessagesToCSV(messages)
}

// UnreadChannel represents a channel with unread messages
type UnreadChannel struct {
	ChannelID   string `json:"channelID"`
	ChannelName string `json:"channelName"`
	ChannelType string `json:"channelType"` // "dm", "group_dm", "partner", "internal"
	UnreadCount int    `json:"unreadCount"`
	LastRead    string `json:"lastRead"`
	Latest      string `json:"latest"`
}

// UnreadMessage extends Message with channel context
type UnreadMessage struct {
	Message
	ChannelType string `json:"channelType"`
}

// ConversationsUnreadsHandler returns unread messages across all channels
func (ch *ConversationsHandler) ConversationsUnreadsHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ConversationsUnreadsHandler called", zap.Any("params", request.Params))

	params := ch.parseParamsToolUnreads(request)

	// Fetch muted channels unless the caller wants them included
	if !params.includeMuted {
		mutedChannels, err := ch.apiProvider.Slack().GetMutedChannels(ctx)
		if err != nil {
			ch.logger.Warn("Failed to fetch muted channels, proceeding without mute filter", zap.Error(err))
			params.mutedUnavailable = true
		} else if len(mutedChannels) > 0 {
			params.mutedChannels = mutedChannels
			ch.logger.Debug("Loaded muted channels", zap.Int("count", len(mutedChannels)))
		}
	}

	// Route based on token type:
	// - xoxc/xoxd (browser session): use fast client.counts API
	// - xoxp (OAuth user): fall back to conversations.info/history approach
	// - xoxb (bot): not supported — unreads is a user-level concept
	if ch.apiProvider.IsOAuth() {
		if ch.apiProvider.IsBotToken() {
			return nil, fmt.Errorf(
				"conversations_unreads requires a user token (xoxp) or browser session tokens (xoxc/xoxd); " +
					"bot tokens (xoxb) do not support unread tracking",
			)
		}
		ch.logger.Info("OAuth token detected, using conversations.info fallback for unreads")
		return ch.getUnreadsViaConversationsInfo(ctx, params)
	}

	counts, err := ch.apiProvider.Slack().ClientCounts(ctx)
	if err != nil {
		ch.logger.Error("ClientCounts failed", zap.Error(err))
		return nil, fmt.Errorf("failed to get client counts: %v", err)
	}

	return ch.processClientCountsResponse(ctx, params, counts)
}

func (ch *ConversationsHandler) processClientCountsResponse(ctx context.Context, params *unreadsParams, counts edge.ClientCountsResponse) (*mcp.CallToolResult, error) {
	ch.logger.Debug("Got counts data",
		zap.Int("channels", len(counts.Channels)),
		zap.Int("mpims", len(counts.MPIMs)),
		zap.Int("ims", len(counts.IMs)))

	// Get users map and channels map for resolving names
	usersMap := ch.apiProvider.ProvideUsersMap()
	channelsMaps := ch.apiProvider.ProvideChannelsMaps()

	// Collect channels with unreads
	var unreadChannels []UnreadChannel

	// Process regular channels (public, private)
	for _, snap := range counts.Channels {
		if !snap.HasUnreads {
			continue
		}

		// Skip muted channels (unless include_muted is set)
		if params.mutedChannels[snap.ID] {
			continue
		}

		// Priority Inbox: skip channels without @mentions
		if params.mentionsOnly && snap.MentionCount == 0 {
			continue
		}

		// Get channel info from cache to determine type and name
		channelName := snap.ID
		channelType := "internal"
		if cached, ok := channelsMaps.Channels[snap.ID]; ok {
			// The cached name may already have # prefix, so handle both cases
			name := cached.Name
			if strings.HasPrefix(name, "#") {
				channelName = name
			} else {
				channelName = "#" + name
			}
			// Check if it's a partner/external channel using Slack's metadata
			if cached.IsExtShared {
				channelType = "partner"
			}
		}

		// Filter by requested channel types
		if params.channelTypes != "all" && channelType != params.channelTypes {
			continue
		}

		unreadChannels = append(unreadChannels, UnreadChannel{
			ChannelID:   snap.ID,
			ChannelName: channelName,
			ChannelType: channelType,
			UnreadCount: snap.MentionCount,
			LastRead:    snap.LastRead.SlackString(),
			Latest:      snap.Latest.SlackString(),
		})
	}

	// Process MPIMs (group DMs)
	for _, snap := range counts.MPIMs {
		if !snap.HasUnreads {
			continue
		}

		// Skip muted channels (unless include_muted is set)
		if params.mutedChannels[snap.ID] {
			continue
		}

		// Priority Inbox: skip channels without @mentions
		if params.mentionsOnly && snap.MentionCount == 0 {
			continue
		}

		// Filter by requested channel types
		if params.channelTypes != "all" && params.channelTypes != "group_dm" {
			continue
		}

		channelName := snap.ID
		if cached, ok := channelsMaps.Channels[snap.ID]; ok {
			channelName = cached.Name
		}

		unreadChannels = append(unreadChannels, UnreadChannel{
			ChannelID:   snap.ID,
			ChannelName: channelName,
			ChannelType: "group_dm",
			UnreadCount: snap.MentionCount,
			LastRead:    snap.LastRead.SlackString(),
			Latest:      snap.Latest.SlackString(),
		})
	}

	// Process IMs (direct messages)
	for _, snap := range counts.IMs {
		if !snap.HasUnreads {
			continue
		}

		// Skip muted channels (unless include_muted is set)
		if params.mutedChannels[snap.ID] {
			continue
		}

		// Priority Inbox: skip channels without @mentions
		if params.mentionsOnly && snap.MentionCount == 0 {
			continue
		}

		// Filter by requested channel types
		if params.channelTypes != "all" && params.channelTypes != "dm" {
			continue
		}

		// Get display name for DM from channel cache or users
		channelName := snap.ID
		if cached, ok := channelsMaps.Channels[snap.ID]; ok {
			if cached.User != "" {
				if u, ok := usersMap.Users[cached.User]; ok {
					channelName = "@" + u.Name
				} else {
					channelName = "@" + cached.User
				}
			}
		}

		unreadChannels = append(unreadChannels, UnreadChannel{
			ChannelID:   snap.ID,
			ChannelName: channelName,
			ChannelType: "dm",
			UnreadCount: snap.MentionCount,
			LastRead:    snap.LastRead.SlackString(),
			Latest:      snap.Latest.SlackString(),
		})
	}

	// Sort by priority: DMs > partner channels > internal
	ch.sortChannelsByPriority(unreadChannels)

	// Limit channels
	if len(unreadChannels) > params.maxChannels {
		unreadChannels = unreadChannels[:params.maxChannels]
	}

	ch.logger.Debug("Found unread channels", zap.Int("count", len(unreadChannels)))

	// Backfill real unread counts for channels where client.counts only gave us
	// HasUnreads=true but MentionCount=0 (unreads without @mentions).
	// DMs and group DMs don't need this — every DM message counts as a mention.
	//
	// NOTE: conversations.info does not return unread_count with browser tokens
	// (xoxc/xoxd), so we use conversations.history to count messages since the
	// last-read timestamp. Limit kept small (20) for speed; the exact count
	// matters less than surfacing that unreads exist.
	const backfillLimit = 20
	backfilled := 0
	for i := range unreadChannels {
		if unreadChannels[i].UnreadCount > 0 {
			continue // MentionCount was positive, good enough
		}
		if unreadChannels[i].LastRead == "" {
			// No last-read timestamp means we can't bound the query.
			// Conservatively report 1 unread since HasUnreads was true.
			unreadChannels[i].UnreadCount = 1
			backfilled++
			continue
		}
		history, err := ch.apiProvider.Slack().GetConversationHistoryContext(ctx,
			&slack.GetConversationHistoryParameters{
				ChannelID: unreadChannels[i].ChannelID,
				Oldest:    unreadChannels[i].LastRead,
				Limit:     backfillLimit,
				Inclusive: false,
			})
		if err != nil {
			ch.logger.Debug("Failed to backfill unread count",
				zap.String("channel", unreadChannels[i].ChannelID),
				zap.Error(err))
			continue
		}
		if len(history.Messages) > 0 {
			unreadChannels[i].UnreadCount = len(history.Messages)
		}
		backfilled++
	}
	if backfilled > 0 {
		ch.logger.Debug("Backfilled unread counts via conversations.history",
			zap.Int("backfilled", backfilled))
	}

	// If not including messages, just return channel summary
	if !params.includeMessages {
		return ch.marshalUnreadChannelsToCSV(unreadChannels)
	}

	// Fetch messages for each unread channel
	var allMessages []Message

	for i := range unreadChannels {
		historyParams := slack.GetConversationHistoryParameters{
			ChannelID: unreadChannels[i].ChannelID,
			Oldest:    unreadChannels[i].LastRead,
			Limit:     params.maxMessagesPerChannel,
			Inclusive: false,
		}

		history, err := ch.apiProvider.Slack().GetConversationHistoryContext(ctx, &historyParams)
		if err != nil {
			ch.logger.Warn("Failed to get history for channel",
				zap.String("channel", unreadChannels[i].ChannelID),
				zap.Error(err))
			continue
		}

		// Update unread count from actual message count
		unreadChannels[i].UnreadCount = len(history.Messages)

		// Convert messages
		channelMessages := ch.convertMessagesFromHistory(ctx, history.Messages, unreadChannels[i].ChannelName, false)
		allMessages = append(allMessages, channelMessages...)
	}

	ch.logger.Debug("Fetched unread messages", zap.Int("total", len(allMessages)))

	return marshalMessagesToCSV(allMessages)
}

func (ch *ConversationsHandler) getUnreadsViaConversationsInfo(ctx context.Context, params *unreadsParams) (*mcp.CallToolResult, error) {
	usersMap := ch.apiProvider.ProvideUsersMap()

	// Define channel type groups in priority order.
	// users.conversations returns channels in creation order (NOT by activity),
	// so we scan each type independently with its own budget/scan cap.
	type typeGroup struct {
		slackTypes  []string // users.conversations "types" parameter values
		channelType string   // our internal type label
		budget      int      // max unread channels to return for this group
		isDM        bool     // DMs get unread_count directly from conversations.info
	}

	// Allocate budgets: DMs get the full max_channels allocation,
	// MPIMs and channels each get half. Each type group scans up to
	// budget*2 channels to find unreads (see scanTypeGroupForUnreads).
	dmBudget := params.maxChannels
	mpimBudget := params.maxChannels / 2
	channelBudget := params.maxChannels / 2

	groups := []typeGroup{
		{slackTypes: []string{"im"}, channelType: "dm", budget: dmBudget, isDM: true},
		{slackTypes: []string{"mpim"}, channelType: "group_dm", budget: mpimBudget, isDM: false},
		{slackTypes: []string{"public_channel", "private_channel"}, channelType: "", budget: channelBudget, isDM: false},
	}

	var unreadChannels []UnreadChannel
	totalAPIcalls := 0
	totalScanned := 0
	totalRateLimited := 0

	for _, group := range groups {
		// Apply channel_types filter: skip groups that don't match the requested filter.
		if params.channelTypes != "all" {
			match := false
			switch params.channelTypes {
			case "dm":
				match = group.channelType == "dm"
			case "group_dm":
				match = group.channelType == "group_dm"
			case "internal", "partner":
				// Both internal and partner channels come from public/private channel types
				match = group.channelType == ""
			}
			if !match {
				continue
			}
		}

		found, apiCalls, scanned, rateLimited := ch.scanTypeGroupForUnreads(ctx, params, usersMap, group.slackTypes, group.channelType, group.budget, group.isDM)
		unreadChannels = append(unreadChannels, found...)
		totalAPIcalls += apiCalls
		totalScanned += scanned
		totalRateLimited += rateLimited
	}

	ch.sortChannelsByPriority(unreadChannels)

	ch.logger.Info("Found unread channels via xoxp fallback",
		zap.Int("count", len(unreadChannels)),
		zap.Int("scanned", totalScanned),
		zap.Int("apiCalls", totalAPIcalls),
		zap.Int("rateLimited", totalRateLimited))

	// Prepend a note about xoxp limitations so the LLM understands
	// these results may be partial.
	mutedNote := ""
	if params.mutedUnavailable && !params.includeMuted {
		mutedNote = "Muted channel filtering is unavailable with xoxp tokens; results may include muted channels. "
	}
	rateLimitNote := ""
	if totalRateLimited > 0 {
		rateLimitNote = fmt.Sprintf("WARNING: %d channels were skipped due to Slack rate limiting (even after retries) — results are degraded. Try again after a brief cooldown. ", totalRateLimited)
	}
	xoxpNote := fmt.Sprintf(
		"[xoxp token: scanned %d channels (%d API calls), found %d with unreads. %s%s"+
			"Results may be incomplete — increase max_channels for broader coverage, "+
			"or use xoxc/xoxd browser tokens for complete results.]\n\n",
		totalScanned, totalAPIcalls, len(unreadChannels), rateLimitNote, mutedNote,
	)

	if !params.includeMessages {
		result, err := ch.marshalUnreadChannelsToCSV(unreadChannels)
		if err != nil {
			return nil, err
		}
		// Prepend the xoxp note to the CSV output
		if len(result.Content) > 0 {
			if tc, ok := result.Content[0].(mcp.TextContent); ok {
				tc.Text = xoxpNote + tc.Text
				result.Content[0] = tc
			}
		}
		return result, nil
	}

	// Fetch actual unread messages for each discovered channel
	rl := limiter.Tier3.Limiter()
	var allMessages []Message
	for _, uc := range unreadChannels {
		historyParams := slack.GetConversationHistoryParameters{
			ChannelID: uc.ChannelID,
			Oldest:    uc.LastRead,
			Limit:     params.maxMessagesPerChannel,
			Inclusive: false,
		}

		history, err := limiter.CallWithRetry(ctx, rl, 2, slackRetryAfter, func() (*slack.GetConversationHistoryResponse, error) {
			return ch.apiProvider.Slack().GetConversationHistoryContext(ctx, &historyParams)
		})
		if err != nil {
			ch.logger.Warn("Failed to get history for channel",
				zap.String("channel", uc.ChannelID),
				zap.Error(err))
			continue
		}

		channelMessages := ch.convertMessagesFromHistory(ctx, history.Messages, uc.ChannelName, false)
		allMessages = append(allMessages, channelMessages...)
	}

	ch.logger.Debug("Fetched unread messages via fallback", zap.Int("total", len(allMessages)))

	result, err := marshalMessagesToCSV(allMessages)
	if err != nil {
		return nil, err
	}
	// Prepend the xoxp note to the messages CSV output
	if len(result.Content) > 0 {
		if tc, ok := result.Content[0].(mcp.TextContent); ok {
			tc.Text = xoxpNote + tc.Text
			result.Content[0] = tc
		}
	}
	return result, nil
}

// slackRetryAfter checks if an error is a Slack rate limit error and returns
// the retry-after duration. Returns 0 for non-rate-limit errors.
// Used as the retryAfter callback for limiter.CallWithRetry.
func slackRetryAfter(err error) time.Duration {
	var rle *slack.RateLimitedError
	if errors.As(err, &rle) {
		return rle.RetryAfter
	}
	return 0
}

// scanTypeGroupForUnreads fetches channels of the given Slack types via users.conversations
// and checks each for unreads via conversations.info.
//
// users.conversations returns only channels the calling user is a member of, which is
// significantly more efficient than conversations.list (excludes non-member public channels
// and closed DMs that cannot have unreads).
//
// Neither endpoint sorts by activity — channels are returned in creation order.
// Unread channels can appear anywhere in the list, so we scan up to a capped number
// per type group to keep API call count bounded (each channel costs 1-2 API calls).
//
// Returns the discovered unread channels, total API calls made, channels scanned,
// and the number of channels skipped due to rate limiting.
func (ch *ConversationsHandler) scanTypeGroupForUnreads(
	ctx context.Context,
	params *unreadsParams,
	usersMap *provider.UsersCache,
	slackTypes []string,
	groupType string,
	budget int,
	isDM bool,
) ([]UnreadChannel, int, int, int) {
	var unreadChannels []UnreadChannel
	apiCalls := 0
	scanned := 0
	rateLimited := 0

	// Proactive rate limiter for conversations.info (Tier 3: ~50 req/min).
	// Without this, the scan fires requests as fast as the network allows,
	// triggering cascading 429s that silently skip channels.
	rl := limiter.Tier3.Limiter()

	// users.conversations returns channels in creation order (not by activity),
	// so unread channels can appear anywhere. We scan up to budget*2 channels
	// per type group — a trade-off between coverage and API call count.
	// Each channel costs 1 API call (DMs: conversations.info returns
	// unread_count directly) or up to 2 calls (non-DMs: conversations.info
	// for last_read + conversations.history to detect new messages).
	//
	// With the default max_channels=50, this gives:
	//   DMs:      scan up to 100 channels (~100 API calls)
	//   MPIMs:    scan up to  50 channels (~100 API calls)
	//   Channels: scan up to  50 channels (~100 API calls)
	//   Total:    ~300 API calls max (vs ~2000+ unbounded)
	maxScan := budget * 2
	if maxScan < 50 {
		maxScan = 50
	}

	cursor := ""
	for {
		if len(unreadChannels) >= budget {
			break
		}
		if maxScan > 0 && scanned >= maxScan {
			break
		}

		// Fetch a page of channels via users.conversations (only channels the
		// calling user is a member of). This is significantly more efficient than
		// conversations.list for xoxp tokens because it excludes non-member public
		// channels and closed DMs — channels that cannot have unreads.
		// Empirically tested: 37% fewer channels on a 2700-channel workspace
		// (1724 vs 2737), with 85% reduction for public channels (156 vs 1046).
		// Both methods miss the same set of archived channels, so zero real
		// unreads are lost by this switch.
		userConvParams := &slack.GetConversationsForUserParameters{
			Types:           slackTypes,
			Limit:           200,
			ExcludeArchived: true,
			Cursor:          cursor,
		}

		channels, nextCursor, err := ch.apiProvider.Slack().GetConversationsForUserContext(ctx, userConvParams)
		apiCalls++
		if err != nil {
			ch.logger.Warn("Failed to list conversations for type group",
				zap.Strings("types", slackTypes),
				zap.Error(err))
			break
		}

		if len(channels) == 0 {
			break
		}

		for _, channel := range channels {
			if len(unreadChannels) >= budget {
				break
			}
			if maxScan > 0 && scanned >= maxScan {
				break
			}

			// Skip muted channels
			if params.mutedChannels[channel.ID] {
				continue
			}

			scanned++

			// Get full channel info including last_read and latest.
			// Uses rate limiting + retry to avoid cascading 429 errors
			// that silently skip channels (see: slack-go does NOT auto-retry
			// on *RateLimitedError for standard client methods).
			info, err := limiter.CallWithRetry(ctx, rl, 2, slackRetryAfter, func() (*slack.Channel, error) {
				return ch.apiProvider.Slack().GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{
					ChannelID: channel.ID,
				})
			})
			apiCalls++
			if err != nil {
				var rle *slack.RateLimitedError
				if errors.As(err, &rle) {
					rateLimited++
					ch.logger.Warn("Rate limited on conversation info (retries exhausted)",
						zap.String("channel", channel.ID),
						zap.Duration("retryAfter", rle.RetryAfter))
				} else {
					ch.logger.Debug("Failed to get conversation info",
						zap.String("channel", channel.ID),
						zap.Error(err))
				}
				continue
			}

			// Determine if channel has unreads
			hasUnreads := false
			unreadCount := 0

			if isDM {
				// DMs: conversations.info returns unread_count directly
				if info.UnreadCount > 0 {
					hasUnreads = true
					unreadCount = info.UnreadCount
				}
			} else {
				// Channels/groups/MPIMs: conversations.info does NOT return
				// unread_count or latest message for xoxp tokens. Use last_read
				// with conversations.history to detect unreads.
				//
				// last_read values:
				//   ""                    → never visited (skip — noise on large workspaces)
				//   "0000000000.000000"   → never read (Slack sentinel, treat as "never visited")
				//   "<timestamp>"         → last read position
				lastRead := info.LastRead
				neverVisited := lastRead == "" || lastRead == "0000000000.000000"

				if neverVisited && groupType != "group_dm" {
					// For regular channels: skip if never visited/read. On large
					// workspaces this includes hundreds of auto-joined or dormant
					// channels — skip to avoid flooding results with stale unreads.
					// MPIMs are always intentional (someone added you), so check them.
					continue
				}

				if neverVisited {
					lastRead = "0" // normalize sentinel to valid oldest param
				}

				historyParams := slack.GetConversationHistoryParameters{
					ChannelID: channel.ID,
					Oldest:    lastRead,
					Limit:     params.maxMessagesPerChannel,
					Inclusive: false,
				}
				history, err := limiter.CallWithRetry(ctx, rl, 2, slackRetryAfter, func() (*slack.GetConversationHistoryResponse, error) {
					return ch.apiProvider.Slack().GetConversationHistoryContext(ctx, &historyParams)
				})
				apiCalls++
				if err != nil {
					var rle *slack.RateLimitedError
					if errors.As(err, &rle) {
						rateLimited++
						ch.logger.Warn("Rate limited on conversation history (retries exhausted)",
							zap.String("channel", channel.ID),
							zap.Duration("retryAfter", rle.RetryAfter))
					} else {
						ch.logger.Debug("Failed to get history for unread check",
							zap.String("channel", channel.ID),
							zap.Error(err))
					}
				} else if len(history.Messages) > 0 {
					hasUnreads = true
					unreadCount = len(history.Messages)
				}
			}

			if !hasUnreads {
				continue
			}

			// Determine channel type and display name
			channelType := groupType
			if channelType == "" {
				// For public/private channels, categorize as internal or partner
				if info.IsExtShared {
					channelType = "partner"
				} else {
					channelType = "internal"
				}
			}

			// Apply channel_types filter for internal vs partner
			if params.channelTypes != "all" && params.channelTypes != channelType {
				continue
			}

			channelName := ch.getChannelDisplayName(info, channelType, usersMap)

			latestTs := ""
			if info.Latest != nil {
				latestTs = info.Latest.Timestamp
			}

			unreadChannels = append(unreadChannels, UnreadChannel{
				ChannelID:   channel.ID,
				ChannelName: channelName,
				ChannelType: channelType,
				UnreadCount: unreadCount,
				LastRead:    info.LastRead,
				Latest:      latestTs,
			})
		}

		// Move to next page if available
		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}

	ch.logger.Debug("Scanned type group",
		zap.Strings("types", slackTypes),
		zap.Int("scanned", scanned),
		zap.Int("found", len(unreadChannels)),
		zap.Int("apiCalls", apiCalls),
		zap.Int("rateLimited", rateLimited))

	return unreadChannels, apiCalls, scanned, rateLimited
}

// getChannelDisplayName returns a human-readable name for a channel from conversations.info data.
func (ch *ConversationsHandler) getChannelDisplayName(info *slack.Channel, channelType string, usersMap *provider.UsersCache) string {
	switch channelType {
	case "dm":
		if info.User != "" {
			if u, ok := usersMap.Users[info.User]; ok {
				return "@" + u.Name
			}
			return "@" + info.User
		}
		return info.ID
	case "group_dm":
		return info.Name
	default:
		name := info.Name
		if !strings.HasPrefix(name, "#") {
			return "#" + name
		}
		return name
	}
}

func (ch *ConversationsHandler) ConversationsLeaveHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ConversationsLeaveHandler called", zap.Any("params", request.Params))

	channel := request.GetString("channel_id", "")
	if channel == "" {
		return nil, fmt.Errorf("channel_id is required")
	}

	channel, err := ch.resolveChannelID(ctx, channel)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve channel: %w", err)
	}

	notInChannel, err := ch.apiProvider.Slack().LeaveConversationContext(ctx, channel)
	if err != nil {
		ch.logger.Error("Failed to leave conversation", zap.Error(err))
		return nil, fmt.Errorf("failed to leave conversation: %v", err)
	}

	if notInChannel {
		ch.logger.Info("Was not in channel", zap.String("channel", channel))
		return mcp.NewToolResultText(fmt.Sprintf("Not a member of %s", channel)), nil
	}

	ch.logger.Info("Left conversation", zap.String("channel", channel))
	return mcp.NewToolResultText(fmt.Sprintf("Successfully left %s", channel)), nil
}

func (ch *ConversationsHandler) ConversationsJoinHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ConversationsJoinHandler called", zap.Any("params", request.Params))

	channel := request.GetString("channel_id", "")
	if channel == "" {
		return nil, fmt.Errorf("channel_id is required")
	}

	channel, err := ch.resolveChannelID(ctx, channel)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve channel: %w", err)
	}

	_, _, _, err = ch.apiProvider.Slack().JoinConversationContext(ctx, channel)
	if err != nil {
		ch.logger.Error("Failed to join conversation", zap.Error(err))
		return nil, fmt.Errorf("failed to join conversation: %v", err)
	}

	ch.logger.Info("Joined conversation", zap.String("channel", channel))
	return mcp.NewToolResultText(fmt.Sprintf("Successfully joined %s", channel)), nil
}

// sortChannelsByPriority sorts channels: DMs > group_dm > partner > internal
func (ch *ConversationsHandler) sortChannelsByPriority(channels []UnreadChannel) {
	priority := map[string]int{
		"dm":       0,
		"group_dm": 1,
		"partner":  2,
		"internal": 3,
	}

	sort.Slice(channels, func(i, j int) bool {
		pi := priority[channels[i].ChannelType]
		pj := priority[channels[j].ChannelType]
		return pi < pj
	})
}

// marshalUnreadChannelsToCSV converts unread channels to CSV format
func (ch *ConversationsHandler) marshalUnreadChannelsToCSV(channels []UnreadChannel) (*mcp.CallToolResult, error) {
	csvBytes, err := gocsv.MarshalBytes(&channels)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(csvBytes)), nil
}

func isChannelAllowedForConfig(channel, config string) bool {
	if config == "" || config == "true" || config == "1" {
		return true
	}
	items := strings.Split(config, ",")
	isNegated := strings.HasPrefix(strings.TrimSpace(items[0]), "!")
	for _, item := range items {
		item = strings.TrimSpace(item)
		if isNegated {
			if strings.TrimPrefix(item, "!") == channel {
				return false
			}
		} else {
			if item == channel {
				return true
			}
		}
	}
	return isNegated
}

func isChannelAllowed(channel string) bool {
	return isChannelAllowedForConfig(channel, os.Getenv("SLACK_MCP_ADD_MESSAGE_TOOL"))
}

func isDraftChannelAllowed(channel string) bool {
	return isChannelAllowedForConfig(channel, os.Getenv("SLACK_MCP_DRAFT_MESSAGE_TOOL"))
}

func (ch *ConversationsHandler) resolveChannelID(ctx context.Context, channel string) (string, error) {
	return resolveChannelID(ctx, ch.apiProvider, ch.logger, channel)
}

// resolveChannelID resolves a channel reference (raw ID, or #name / @username_dm)
// to a channel ID. #/@-prefixed refs are looked up via ChannelsInv with a single
// ForceRefreshChannels retry on a miss; a still-missing ref returns a clear
// "not found" error. Shared by ConversationsHandler and ChannelsHandler so the
// two stay in lockstep.
func resolveChannelID(ctx context.Context, apiProvider *provider.ApiProvider, logger *zap.Logger, channel string) (string, error) {
	if !strings.HasPrefix(channel, "#") && !strings.HasPrefix(channel, "@") {
		return channel, nil
	}

	// First attempt: try to resolve from current cache
	channelsMaps := apiProvider.ProvideChannelsMaps()
	chn, ok := channelsMaps.ChannelsInv[channel]
	if ok {
		return channelsMaps.Channels[chn].ID, nil
	}

	// Channel not found - try refreshing cache and retry once
	logger.Debug("Channel not found in cache, attempting refresh",
		zap.String("channel", channel))

	refreshErr := apiProvider.ForceRefreshChannels(ctx)
	wasRateLimited := errors.Is(refreshErr, provider.ErrRefreshRateLimited)

	if refreshErr != nil && !wasRateLimited {
		logger.Error("Failed to refresh channels cache",
			zap.String("channel", channel),
			zap.Error(refreshErr))
		return "", fmt.Errorf("channel %q not found and cache refresh failed: %w", channel, refreshErr)
	}

	// If rate-limited, cache wasn't refreshed - no point in a second lookup
	if wasRateLimited {
		logger.Warn("Channel not found; cache refresh was rate-limited",
			zap.String("channel", channel))
		return "", fmt.Errorf("channel %q not found (cache refresh was rate-limited, try again later)", channel)
	}

	// Second attempt after successful refresh
	channelsMaps = apiProvider.ProvideChannelsMaps()
	chn, ok = channelsMaps.ChannelsInv[channel]
	if !ok {
		logger.Error("Channel not found even after cache refresh",
			zap.String("channel", channel))
		return "", fmt.Errorf("channel %q not found", channel)
	}

	logger.Debug("Channel found after cache refresh",
		zap.String("channel", channel),
		zap.String("channel_id", channelsMaps.Channels[chn].ID))

	return channelsMaps.Channels[chn].ID, nil
}

func (ch *ConversationsHandler) convertMessagesFromHistory(ctx context.Context, slackMessages []slack.Message, channel string, includeActivity bool) []Message {
	resolver := ch.newUserResolver(ctx)
	var messages []Message
	warn := false

	for _, msg := range slackMessages {
		if (msg.SubType != "" && msg.SubType != "bot_message" && msg.SubType != "thread_broadcast") && !includeActivity {
			continue
		}

		userName, realName, ok := resolver.resolve(msg.User)

		if !ok && msg.SubType == "bot_message" {
			userName, realName, ok = getBotInfo(msg.Username)
		}

		if !ok {
			warn = true
		}

		timestamp, err := text.TimestampToIsoRFC3339(msg.Timestamp)
		if err != nil {
			ch.logger.Error("Failed to convert timestamp to RFC3339", zap.Error(err))
			continue
		}

		msgText := msg.Text
		if msgText == "" {
			msgText = text.BlocksToText(msg.Blocks)
		}
		if msgText == "" {
			msgText = text.FilesToText(msg.Files)
		}
		msgText += text.AttachmentsTo2CSV(msgText, msg.Attachments)

		var reactionParts []string
		for _, r := range msg.Reactions {
			reactionParts = append(reactionParts, fmt.Sprintf("%s:%d", r.Name, r.Count))
		}
		reactionsString := strings.Join(reactionParts, "|")

		botName := ""
		if msg.BotProfile != nil && msg.BotProfile.Name != "" {
			botName = msg.BotProfile.Name
		}

		fileCount := len(msg.Files)
		hasMedia := fileCount > 0 || hasImageBlocks(msg.Blocks)

		var attachmentIDs []string
		for _, f := range msg.Files {
			if f.Name != "" {
				attachmentIDs = append(attachmentIDs, fmt.Sprintf("%s (%s)", f.ID, f.Name))
			} else {
				attachmentIDs = append(attachmentIDs, f.ID)
			}
		}
		attachmentIDsStr := strings.Join(attachmentIDs, ", ")

		messages = append(messages, Message{
			MsgID:         msg.Timestamp,
			UserID:        msg.User,
			UserName:      userName,
			RealName:      realName,
			Text:          text.ProcessText(msgText),
			Channel:       channel,
			ThreadTs:      msg.ThreadTimestamp,
			Time:          timestamp,
			Reactions:     reactionsString,
			BotName:       botName,
			FileCount:     fileCount,
			AttachmentIDs: attachmentIDsStr,
			HasMedia:      hasMedia,
		})
	}

	if ready, err := ch.apiProvider.IsReady(); !ready {
		if warn && errors.Is(err, provider.ErrUsersNotReady) {
			ch.logger.Warn(
				"WARNING: Slack users sync is not ready yet, you may experience some limited functionality and see UIDs instead of resolved names as well as unable to query users by their @handles. Users sync is part of channels sync and operations on channels depend on users collection (IM, MPIM). Please wait until users are synced and try again",
				zap.Error(err),
			)
		}
	}
	return messages
}

func (ch *ConversationsHandler) convertMessagesFromSearch(ctx context.Context, slackMessages []slack.SearchMessage) []Message {
	resolver := ch.newUserResolver(ctx)
	var messages []Message
	warn := false

	for _, msg := range slackMessages {
		userName, realName, ok := resolver.resolve(msg.User)

		if !ok && msg.User == "" && msg.Username != "" {
			userName, realName, ok = getBotInfo(msg.Username)
		}

		if !ok {
			warn = true
		}

		threadTs, _ := extractThreadTS(msg.Permalink)

		timestamp, err := text.TimestampToIsoRFC3339(msg.Timestamp)
		if err != nil {
			ch.logger.Error("Failed to convert timestamp to RFC3339", zap.Error(err))
			continue
		}

		msgText := msg.Text
		if msgText == "" {
			msgText = text.BlocksToText(msg.Blocks)
		}
		msgText += text.AttachmentsTo2CSV(msgText, msg.Attachments)

		hasMedia := hasImageBlocks(msg.Blocks)

		messages = append(messages, Message{
			MsgID:     msg.Timestamp,
			UserID:    msg.User,
			UserName:  userName,
			RealName:  realName,
			Text:      text.ProcessText(msgText),
			Channel:   fmt.Sprintf("%s (#%s)", msg.Channel.ID, msg.Channel.Name),
			ThreadTs:  threadTs,
			Time:      timestamp,
			Permalink: msg.Permalink,
			Reactions: "",
			HasMedia:  hasMedia,
		})
	}

	if ready, err := ch.apiProvider.IsReady(); !ready {
		if warn && errors.Is(err, provider.ErrUsersNotReady) {
			ch.logger.Warn(
				"Slack users sync not ready; you may see raw UIDs instead of names and lose some functionality.",
				zap.Error(err),
			)
		}
	}
	return messages
}

func (ch *ConversationsHandler) parseParamsToolConversations(ctx context.Context, request mcp.CallToolRequest) (*conversationParams, error) {
	channel := request.GetString("channel_id", "")
	if channel == "" {
		ch.logger.Error("channel_id missing in conversations params")
		return nil, errors.New("channel_id must be a string")
	}

	limit := request.GetString("limit", "")
	cursor := request.GetString("cursor", "")
	activity := request.GetBool("include_activity_messages", false)

	var (
		paramLimit  int
		paramOldest string
		paramLatest string
		err         error
	)
	if strings.HasSuffix(limit, "d") || strings.HasSuffix(limit, "w") || strings.HasSuffix(limit, "m") {
		paramLimit, paramOldest, paramLatest, err = limitByExpression(limit, defaultConversationsExpressionLimit)
		if err != nil {
			ch.logger.Error("Invalid duration limit", zap.String("limit", limit), zap.Error(err))
			return nil, err
		}
	} else if cursor == "" {
		paramLimit, err = limitByNumeric(limit, defaultConversationsNumericLimit)
		if err != nil {
			ch.logger.Error("Invalid numeric limit", zap.String("limit", limit), zap.Error(err))
			return nil, err
		}
	}

	if strings.HasPrefix(channel, "#") || strings.HasPrefix(channel, "@") {
		if ready, err := ch.apiProvider.IsReady(); !ready {
			if errors.Is(err, provider.ErrUsersNotReady) {
				ch.logger.Warn(
					"WARNING: Slack users sync is not ready yet, you may experience some limited functionality and see UIDs instead of resolved names as well as unable to query users by their @handles. Users sync is part of channels sync and operations on channels depend on users collection (IM, MPIM). Please wait until users are synced and try again",
					zap.Error(err),
				)
			}
			if errors.Is(err, provider.ErrChannelsNotReady) {
				ch.logger.Warn(
					"WARNING: Slack channels sync is not ready yet, you may experience some limited functionality and be able to request conversation only by Channel ID, not by its name. Please wait until channels are synced and try again.",
					zap.Error(err),
				)
			}
			return nil, fmt.Errorf("channel %q not found in empty cache", channel)
		}
		// Use resolveChannelID which includes refresh-on-error logic
		resolvedChannel, err := ch.resolveChannelID(ctx, channel)
		if err != nil {
			return nil, err
		}
		channel = resolvedChannel
	}

	return &conversationParams{
		channel:  channel,
		limit:    paramLimit,
		oldest:   paramOldest,
		latest:   paramLatest,
		cursor:   cursor,
		activity: activity,
	}, nil
}

func (ch *ConversationsHandler) parseParamsToolAddMessage(ctx context.Context, request mcp.CallToolRequest) (*addMessageParams, error) {
	toolConfig := os.Getenv("SLACK_MCP_ADD_MESSAGE_TOOL")
	enabledTools := os.Getenv("SLACK_MCP_ENABLED_TOOLS")

	if toolConfig == "" {
		if !strings.Contains(enabledTools, "conversations_add_message") {
			ch.logger.Error("Add-message tool disabled by default")
			return nil, errors.New(
				"by default, the conversations_add_message tool is disabled to guard Slack workspaces against accidental spamming. " +
					"To enable it, set the SLACK_MCP_ADD_MESSAGE_TOOL environment variable to true, 1, or comma separated list of channels " +
					"to limit where the MCP can post messages, e.g. 'SLACK_MCP_ADD_MESSAGE_TOOL=C1234567890,D0987654321', 'SLACK_MCP_ADD_MESSAGE_TOOL=!C1234567890' " +
					"to enable all except one or 'SLACK_MCP_ADD_MESSAGE_TOOL=true' for all channels and DMs",
			)
		}
		toolConfig = "true"
	}

	channel := request.GetString("channel_id", "")
	if channel == "" {
		ch.logger.Error("channel_id missing in add-message params")
		return nil, errors.New("channel_id must be a string")
	}
	channel, err := ch.resolveChannelID(ctx, channel)
	if err != nil {
		ch.logger.Error("Channel not found", zap.String("channel", channel), zap.Error(err))
		return nil, err
	}
	if !isChannelAllowed(channel) {
		ch.logger.Warn("Add-message tool not allowed for channel", zap.String("channel", channel), zap.String("policy", toolConfig))
		return nil, fmt.Errorf("conversations_add_message tool is not allowed for channel %q, applied policy: %s", channel, toolConfig)
	}

	threadTs := request.GetString("thread_ts", "")
	if threadTs != "" && !strings.Contains(threadTs, ".") {
		ch.logger.Error("Invalid thread_ts format", zap.String("thread_ts", threadTs))
		return nil, errors.New("thread_ts must be a valid timestamp in format 1234567890.123456")
	}

	msgText := request.GetString("text", "")
	if msgText == "" {
		// Backward compatibility with "payload" parameter
		msgText = request.GetString("payload", "")
	}

	contentType := request.GetString("content_type", "text/markdown")
	if contentType != "text/plain" && contentType != "text/markdown" {
		ch.logger.Error("Invalid content_type", zap.String("content_type", contentType))
		return nil, errors.New("content_type must be either 'text/plain' or 'text/markdown'")
	}

	// Parse optional raw blocks JSON. Accepts blocks as either:
	// - A JSON string containing a blocks array: "blocks": "[{...}]"
	// - A raw JSON array (parsed by MCP SDK): "blocks": [{...}]
	var blocks []slack.Block
	args := request.GetArguments()
	if rawBlocks, ok := args["blocks"]; ok && rawBlocks != nil {
		var blocksJSON []byte
		switch v := rawBlocks.(type) {
		case string:
			if v != "" {
				blocksJSON = []byte(v)
			}
		default:
			// Raw JSON array/object passed directly - re-marshal to bytes
			var err error
			blocksJSON, err = json.Marshal(v)
			if err != nil {
				ch.logger.Error("Failed to marshal blocks argument", zap.Error(err))
				return nil, fmt.Errorf("blocks must be valid Slack Block Kit JSON: %w", err)
			}
		}
		if blocksJSON != nil {
			var slackBlocks slack.Blocks
			if err := json.Unmarshal(blocksJSON, &slackBlocks); err != nil {
				ch.logger.Error("Failed to parse blocks JSON", zap.Error(err))
				return nil, fmt.Errorf("blocks must be valid Slack Block Kit JSON: %w", err)
			}
			blocks = slackBlocks.BlockSet
		}
	}

	// Require either text or blocks
	if msgText == "" && blocks == nil {
		ch.logger.Error("Message text and blocks both missing")
		return nil, errors.New("either text or blocks must be provided")
	}

	return &addMessageParams{
		channel:     channel,
		threadTs:    threadTs,
		text:        msgText,
		contentType: contentType,
		blocks:      blocks,
	}, nil
}

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
	if contentType != "text/plain" && contentType != "text/markdown" && contentType != "application/json" {
		ch.logger.Error("Invalid content_type", zap.String("content_type", contentType))
		return nil, errors.New("content_type must be 'text/plain', 'text/markdown' or 'application/json'")
	}

	return &draftMessageParams{
		channel:     channel,
		threadTs:    threadTs,
		text:        msgText,
		contentType: contentType,
		overwrite:   request.GetBool("overwrite", false),
		draftID:     request.GetString("draft_id", ""),
	}, nil
}

func (ch *ConversationsHandler) parseParamsToolReaction(ctx context.Context, request mcp.CallToolRequest) (*addReactionParams, error) {
	toolConfig := os.Getenv("SLACK_MCP_REACTION_TOOL")
	enabledTools := os.Getenv("SLACK_MCP_ENABLED_TOOLS")

	if toolConfig == "" {
		if !strings.Contains(enabledTools, "reactions_add") && !strings.Contains(enabledTools, "reactions_remove") {
			ch.logger.Error("Reactions tool disabled by default")
			return nil, errors.New(
				"by default, the reactions tools are disabled to guard Slack workspaces against accidental spamming. " +
					"To enable them, set the SLACK_MCP_REACTION_TOOL environment variable to true, 1, or comma separated list of channels " +
					"to limit where the MCP can manage reactions, e.g. 'SLACK_MCP_REACTION_TOOL=C1234567890,D0987654321', 'SLACK_MCP_REACTION_TOOL=!C1234567890' " +
					"to enable all except one or 'SLACK_MCP_REACTION_TOOL=true' for all channels and DMs",
			)
		}
		toolConfig = "true"
	}

	channel := request.GetString("channel_id", "")
	if channel == "" {
		return nil, errors.New("channel_id is required")
	}
	channel, err := ch.resolveChannelID(ctx, channel)
	if err != nil {
		ch.logger.Error("Channel not found", zap.String("channel", channel), zap.Error(err))
		return nil, err
	}
	if !isChannelAllowedForConfig(channel, toolConfig) {
		ch.logger.Warn("Reactions tool not allowed for channel", zap.String("channel", channel), zap.String("policy", toolConfig))
		return nil, fmt.Errorf("reactions tools are not allowed for channel %q, applied policy: %s", channel, toolConfig)
	}

	timestamp := request.GetString("timestamp", "")
	if timestamp == "" {
		return nil, errors.New("timestamp is required")
	}

	emoji := strings.Trim(request.GetString("emoji", ""), ":")
	if emoji == "" {
		return nil, errors.New("emoji is required")
	}

	return &addReactionParams{
		channel:   channel,
		timestamp: timestamp,
		emoji:     emoji,
	}, nil
}

func (ch *ConversationsHandler) parseParamsToolFilesGet(request mcp.CallToolRequest) (*filesGetParams, error) {
	toolConfig := os.Getenv("SLACK_MCP_ATTACHMENT_TOOL")
	enabledTools := os.Getenv("SLACK_MCP_ENABLED_TOOLS")

	if toolConfig == "" {
		if !strings.Contains(enabledTools, "attachment_get_data") {
			ch.logger.Error("Attachment tool disabled by default")
			return nil, errors.New(
				"by default, the attachment_get_data tool is disabled. " +
					"To enable it, set the SLACK_MCP_ATTACHMENT_TOOL environment variable to true or 1",
			)
		}
		toolConfig = "true"
	}
	if toolConfig != "true" && toolConfig != "1" && toolConfig != "yes" {
		ch.logger.Error("Attachment tool disabled", zap.String("config", toolConfig))
		return nil, errors.New("SLACK_MCP_ATTACHMENT_TOOL must be set to 'true', '1', or 'yes' to enable")
	}

	fileID := request.GetString("file_id", "")
	if fileID == "" {
		return nil, errors.New("file_id is required")
	}

	return &filesGetParams{
		fileID: fileID,
	}, nil
}

func (ch *ConversationsHandler) parseParamsToolUsersSearch(request mcp.CallToolRequest) (*usersSearchParams, error) {
	query := strings.TrimSpace(request.GetString("query", ""))
	if query == "" {
		return nil, errors.New("query is required")
	}

	limit := request.GetInt("limit", 10)
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	return &usersSearchParams{
		query: query,
		limit: limit,
	}, nil
}

func (ch *ConversationsHandler) parseParamsToolUnreads(request mcp.CallToolRequest) *unreadsParams {
	return &unreadsParams{
		includeMessages:       request.GetBool("include_messages", true),
		channelTypes:          request.GetString("channel_types", "all"),
		maxChannels:           request.GetInt("max_channels", 50),
		maxMessagesPerChannel: request.GetInt("max_messages_per_channel", 10),
		mentionsOnly:          request.GetBool("mentions_only", false),
		includeMuted:          request.GetBool("include_muted", false),
	}
}

func (ch *ConversationsHandler) parseParamsToolSearch(ctx context.Context, req mcp.CallToolRequest) (*searchParams, error) {
	rawQuery := strings.TrimSpace(req.GetString("search_query", ""))
	freeText, filters := splitQuery(rawQuery)

	if req.GetBool("filter_threads_only", false) {
		addFilter(filters, "is", "thread")
	}
	if chName := req.GetString("filter_in_channel", ""); chName != "" {
		f, err := ch.paramFormatChannel(chName)
		if err != nil {
			ch.logger.Error("Invalid channel filter", zap.String("filter", chName), zap.Error(err))
			return nil, err
		}
		addFilter(filters, "in", f)
	} else if im := req.GetString("filter_in_im_or_mpim", ""); im != "" {
		f, err := ch.paramFormatUser(ctx, im)
		if err != nil {
			ch.logger.Error("Invalid IM/MPIM filter", zap.String("filter", im), zap.Error(err))
			return nil, err
		}
		addFilter(filters, "in", f)
	}
	if with := req.GetString("filter_users_with", ""); with != "" {
		f, err := ch.paramFormatUser(ctx, with)
		if err != nil {
			ch.logger.Error("Invalid with-user filter", zap.String("filter", with), zap.Error(err))
			return nil, err
		}
		addFilter(filters, "with", f)
	}
	if from := req.GetString("filter_users_from", ""); from != "" {
		f, err := ch.paramFormatUser(ctx, from)
		if err != nil {
			ch.logger.Error("Invalid from-user filter", zap.String("filter", from), zap.Error(err))
			return nil, err
		}
		addFilter(filters, "from", f)
	}
	if emoji := req.GetString("filter_has_reaction", ""); emoji != "" {
		addFilter(filters, "has", normalizeEmojiFilter(emoji))
	}
	if emoji := req.GetString("filter_my_reaction", ""); emoji != "" {
		addFilter(filters, "hasmy", normalizeEmojiFilter(emoji))
	}

	dateMap, err := buildDateFilters(
		req.GetString("filter_date_before", ""),
		req.GetString("filter_date_after", ""),
		req.GetString("filter_date_on", ""),
		req.GetString("filter_date_during", ""),
	)
	if err != nil {
		ch.logger.Error("Invalid date filters", zap.Error(err))
		return nil, err
	}
	for key, val := range dateMap {
		addFilter(filters, key, val)
	}

	finalQuery := buildQuery(freeText, filters)
	limit := req.GetInt("limit", 100)
	cursor := req.GetString("cursor", "")

	var (
		page          int
		decodedCursor []byte
	)
	if cursor != "" {
		decodedCursor, err = base64.StdEncoding.DecodeString(cursor)
		if err != nil {
			ch.logger.Error("Invalid cursor decoding", zap.String("cursor", cursor), zap.Error(err))
			return nil, fmt.Errorf("invalid cursor: %v", err)
		}
		parts := strings.Split(string(decodedCursor), ":")
		if len(parts) != 2 {
			ch.logger.Error("Invalid cursor format", zap.String("cursor", cursor))
			return nil, fmt.Errorf("invalid cursor: %v", cursor)
		}
		page, err = strconv.Atoi(parts[1])
		if err != nil || page < 1 {
			ch.logger.Error("Invalid cursor page", zap.String("cursor", cursor), zap.Error(err))
			return nil, fmt.Errorf("invalid cursor page: %v", err)
		}
	} else {
		page = 1
	}

	ch.logger.Debug("Search parameters built",
		zap.String("query", finalQuery),
		zap.Int("limit", limit),
		zap.Int("page", page),
	)
	return &searchParams{
		query: finalQuery,
		limit: limit,
		page:  page,
	}, nil
}

// Slack user IDs may begin with U or W: https://docs.slack.dev/changelog/2016/08/11/user-id-format-changes
func isSlackUserIDPrefix(s string) bool {
	return strings.HasPrefix(s, "U") || strings.HasPrefix(s, "W")
}

func (ch *ConversationsHandler) paramFormatUser(ctx context.Context, raw string) (string, error) {
	users := ch.apiProvider.ProvideUsersMap()
	raw = strings.TrimSpace(raw)
	if isSlackUserIDPrefix(raw) {
		u, ok := users.Users[raw]
		if !ok {
			// Targeted fetch: single users.info call instead of full cache rebuild
			patched, err := ch.apiProvider.PatchUser(ctx, raw)
			if err != nil {
				ch.logger.Debug("Targeted user fetch failed, user not found",
					zap.String("user_id", raw), zap.Error(err))
				return "", fmt.Errorf("user %q not found", raw)
			}
			return fmt.Sprintf("<@%s>", patched.ID), nil
		}
		return fmt.Sprintf("<@%s>", u.ID), nil
	}
	if strings.HasPrefix(raw, "<@") {
		raw = raw[2:]
	}
	if strings.HasPrefix(raw, "@") {
		raw = raw[1:]
	}
	uid, ok := users.UsersInv[raw]
	if !ok {
		return "", fmt.Errorf("user %q not found", raw)
	}
	return fmt.Sprintf("<@%s>", uid), nil
}

func (ch *ConversationsHandler) paramFormatChannel(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	cms := ch.apiProvider.ProvideChannelsMaps()
	if strings.HasPrefix(raw, "#") {
		if id, ok := cms.ChannelsInv[raw]; ok {
			return cms.Channels[id].Name, nil
		}
		return "", fmt.Errorf("channel %q not found", raw)
	}
	// Handle both C (standard channels) and G (private groups/channels) prefixes
	if strings.HasPrefix(raw, "C") || strings.HasPrefix(raw, "G") {
		if chn, ok := cms.Channels[raw]; ok {
			return chn.Name, nil
		}
		return "", fmt.Errorf("channel %q not found", raw)
	}
	return "", fmt.Errorf("invalid channel format: %q", raw)
}

func marshalMessagesToCSV(messages []Message) (*mcp.CallToolResult, error) {
	csvBytes, err := gocsv.MarshalBytes(&messages)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(csvBytes)), nil
}

func getUserInfo(userID string, usersMap map[string]slack.User) (userName, realName string, ok bool) {
	if u, ok := usersMap[userID]; ok {
		return u.Name, u.RealName, true
	}
	return userID, userID, false
}

// userResolver resolves user IDs to names, fetching unknown users from the
// Slack API on demand. It caches the snapshot locally and remembers which IDs
// it already tried to fetch, so a user that doesn't exist in Slack is only
// looked up once per batch rather than once per message.
type userResolver struct {
	apiProvider  *provider.ApiProvider
	ctx          context.Context
	usersMap     *provider.UsersCache
	attemptedIDs map[string]bool
}

func (ch *ConversationsHandler) newUserResolver(ctx context.Context) *userResolver {
	return &userResolver{
		apiProvider:  ch.apiProvider,
		ctx:          ctx,
		usersMap:     ch.apiProvider.ProvideUsersMap(),
		attemptedIDs: make(map[string]bool),
	}
}

func (r *userResolver) resolve(userID string) (userName, realName string, ok bool) {
	if u, ok := r.usersMap.Users[userID]; ok {
		return u.Name, u.RealName, true
	}
	if userID == "" || r.attemptedIDs[userID] {
		return userID, userID, false
	}
	r.attemptedIDs[userID] = true
	patched, err := r.apiProvider.PatchUser(r.ctx, userID)
	if err != nil {
		return userID, userID, false
	}
	r.usersMap = r.apiProvider.ProvideUsersMap()
	return patched.Name, patched.RealName, true
}

func getBotInfo(botID string) (userName, realName string, ok bool) {
	return botID, botID, true
}

func limitByNumeric(limit string, defaultLimit int) (int, error) {
	if limit == "" {
		return defaultLimit, nil
	}
	n, err := strconv.Atoi(limit)
	if err != nil {
		return 0, fmt.Errorf("invalid numeric limit: %q", limit)
	}
	return n, nil
}

func limitByExpression(limit, defaultLimit string) (slackLimit int, oldest, latest string, err error) {
	if limit == "" {
		limit = defaultLimit
	}
	if len(limit) < 2 {
		return 0, "", "", fmt.Errorf("invalid duration limit %q: too short", limit)
	}
	suffix := limit[len(limit)-1]
	numStr := limit[:len(limit)-1]
	n, err := strconv.Atoi(numStr)
	if err != nil || n <= 0 {
		return 0, "", "", fmt.Errorf("invalid duration limit %q: must be a positive integer followed by 'd', 'w', or 'm'", limit)
	}
	now := time.Now()
	loc := now.Location()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	var oldestTime time.Time
	switch suffix {
	case 'd':
		oldestTime = startOfToday.AddDate(0, 0, -n+1)
	case 'w':
		oldestTime = startOfToday.AddDate(0, 0, -n*7+1)
	case 'm':
		oldestTime = startOfToday.AddDate(0, -n, 0)
	default:
		return 0, "", "", fmt.Errorf("invalid duration limit %q: must end in 'd', 'w', or 'm'", limit)
	}
	latest = fmt.Sprintf("%d.000000", now.Unix())
	oldest = fmt.Sprintf("%d.000000", oldestTime.Unix())
	return 100, oldest, latest, nil
}

func extractThreadTS(rawurl string) (string, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return "", err
	}
	return u.Query().Get("thread_ts"), nil
}

func parseFlexibleDate(dateStr string) (time.Time, string, error) {
	dateStr = strings.TrimSpace(dateStr)
	standardFormats := []string{
		"2006-01-02",      // YYYY-MM-DD
		"2006/01/02",      // YYYY/MM/DD
		"01-02-2006",      // MM-DD-YYYY
		"01/02/2006",      // MM/DD/YYYY
		"02-01-2006",      // DD-MM-YYYY
		"02/01/2006",      // DD/MM/YYYY
		"Jan 2, 2006",     // Jan 2, 2006
		"January 2, 2006", // January 2, 2006
		"2 Jan 2006",      // 2 Jan 2006
		"2 January 2006",  // 2 January 2006
	}
	for _, fmtStr := range standardFormats {
		if t, err := time.Parse(fmtStr, dateStr); err == nil {
			return t, t.Format("2006-01-02"), nil
		}
	}

	monthMap := map[string]int{
		"january": 1, "jan": 1,
		"february": 2, "feb": 2,
		"march": 3, "mar": 3,
		"april": 4, "apr": 4,
		"may":  5,
		"june": 6, "jun": 6,
		"july": 7, "jul": 7,
		"august": 8, "aug": 8,
		"september": 9, "sep": 9, "sept": 9,
		"october": 10, "oct": 10,
		"november": 11, "nov": 11,
		"december": 12, "dec": 12,
	}

	// Month-Year patterns
	monthYear := regexp.MustCompile(`^(\d{4})\s+([A-Za-z]+)$|^([A-Za-z]+)\s+(\d{4})$`)
	if m := monthYear.FindStringSubmatch(dateStr); m != nil {
		var year int
		var monStr string
		if m[1] != "" && m[2] != "" {
			year, _ = strconv.Atoi(m[1])
			monStr = strings.ToLower(m[2])
		} else {
			year, _ = strconv.Atoi(m[4])
			monStr = strings.ToLower(m[3])
		}
		if mon, ok := monthMap[monStr]; ok {
			t := time.Date(year, time.Month(mon), 1, 0, 0, 0, 0, time.UTC)
			return t, t.Format("2006-01-02"), nil
		}
	}

	// Day-Month-Year and Month-Day-Year patterns
	dmy1 := regexp.MustCompile(`^(\d{1,2})[-\s]+([A-Za-z]+)[-\s]+(\d{4})$`)
	if m := dmy1.FindStringSubmatch(dateStr); m != nil {
		day, _ := strconv.Atoi(m[1])
		year, _ := strconv.Atoi(m[3])
		monStr := strings.ToLower(m[2])
		if mon, ok := monthMap[monStr]; ok {
			t := time.Date(year, time.Month(mon), day, 0, 0, 0, 0, time.UTC)
			if t.Day() == day {
				return t, t.Format("2006-01-02"), nil
			}
		}
	}
	mdy := regexp.MustCompile(`^([A-Za-z]+)[-\s]+(\d{1,2})[-\s]+(\d{4})$`)
	if m := mdy.FindStringSubmatch(dateStr); m != nil {
		monStr := strings.ToLower(m[1])
		day, _ := strconv.Atoi(m[2])
		year, _ := strconv.Atoi(m[3])
		if mon, ok := monthMap[monStr]; ok {
			t := time.Date(year, time.Month(mon), day, 0, 0, 0, 0, time.UTC)
			if t.Day() == day {
				return t, t.Format("2006-01-02"), nil
			}
		}
	}
	ymd := regexp.MustCompile(`^(\d{4})[-\s]+([A-Za-z]+)[-\s]+(\d{1,2})$`)
	if m := ymd.FindStringSubmatch(dateStr); m != nil {
		year, _ := strconv.Atoi(m[1])
		monStr := strings.ToLower(m[2])
		day, _ := strconv.Atoi(m[3])
		if mon, ok := monthMap[monStr]; ok {
			t := time.Date(year, time.Month(mon), day, 0, 0, 0, 0, time.UTC)
			if t.Day() == day {
				return t, t.Format("2006-01-02"), nil
			}
		}
	}

	lower := strings.ToLower(dateStr)
	now := time.Now().UTC()
	switch lower {
	case "today":
		t := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		return t, t.Format("2006-01-02"), nil
	case "yesterday":
		t := now.AddDate(0, 0, -1)
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		return t, t.Format("2006-01-02"), nil
	case "tomorrow":
		t := now.AddDate(0, 0, 1)
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		return t, t.Format("2006-01-02"), nil
	}

	daysAgo := regexp.MustCompile(`^(\d+)\s+days?\s+ago$`)
	if m := daysAgo.FindStringSubmatch(lower); m != nil {
		days, _ := strconv.Atoi(m[1])
		t := now.AddDate(0, 0, -days)
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		return t, t.Format("2006-01-02"), nil
	}

	return time.Time{}, "", fmt.Errorf("unable to parse date: %s", dateStr)
}

func buildDateFilters(before, after, on, during string) (map[string]string, error) {
	out := make(map[string]string)
	if on != "" {
		if during != "" || before != "" || after != "" {
			return nil, fmt.Errorf("'on' cannot be combined with other date filters")
		}
		_, normalized, err := parseFlexibleDate(on)
		if err != nil {
			return nil, fmt.Errorf("invalid 'on' date: %v", err)
		}
		out["on"] = normalized
		return out, nil
	}
	if during != "" {
		if before != "" || after != "" {
			return nil, fmt.Errorf("'during' cannot be combined with 'before' or 'after'")
		}
		_, normalized, err := parseFlexibleDate(during)
		if err != nil {
			return nil, fmt.Errorf("invalid 'during' date: %v", err)
		}
		out["during"] = normalized
		return out, nil
	}
	if after != "" {
		_, normalized, err := parseFlexibleDate(after)
		if err != nil {
			return nil, fmt.Errorf("invalid 'after' date: %v", err)
		}
		out["after"] = normalized
	}
	if before != "" {
		_, normalized, err := parseFlexibleDate(before)
		if err != nil {
			return nil, fmt.Errorf("invalid 'before' date: %v", err)
		}
		out["before"] = normalized
	}
	if after != "" && before != "" {
		a, _, _ := parseFlexibleDate(after)
		b, _, _ := parseFlexibleDate(before)
		if a.After(b) {
			return nil, fmt.Errorf("'after' date is after 'before' date")
		}
	}
	return out, nil
}

// normalizeEmojiFilter converts an emoji name like "pushpin" or ":pushpin:"
// to the ":pushpin:" form Slack search expects in has:/hasmy: modifiers.
// Unicode emoji characters are passed through unchanged.
func normalizeEmojiFilter(raw string) string {
	name := strings.Trim(strings.TrimSpace(raw), ":")
	for _, r := range name {
		isName := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '_' || r == '-' || r == '+'
		if !isName {
			return strings.TrimSpace(raw)
		}
	}
	return ":" + name + ":"
}

func isFilterKey(key string) bool {
	_, ok := validFilterKeys[strings.ToLower(key)]
	return ok
}

func splitQuery(q string) (freeText []string, filters map[string][]string) {
	filters = make(map[string][]string)
	for _, tok := range strings.Fields(q) {
		parts := strings.SplitN(tok, ":", 2)
		if len(parts) == 2 && isFilterKey(parts[0]) {
			key := strings.ToLower(parts[0])
			filters[key] = append(filters[key], parts[1])
		} else {
			freeText = append(freeText, tok)
		}
	}
	return
}

func addFilter(filters map[string][]string, key, val string) {
	for _, existing := range filters[key] {
		if existing == val {
			return
		}
	}
	filters[key] = append(filters[key], val)
}

func buildQuery(freeText []string, filters map[string][]string) string {
	var out []string
	out = append(out, freeText...)
	for _, key := range []string{"is", "in", "from", "with", "before", "after", "on", "during", "has", "hasmy"} {
		for _, val := range filters[key] {
			out = append(out, fmt.Sprintf("%s:%s", key, val))
		}
	}
	return strings.Join(out, " ")
}

func hasImageBlocks(blocks slack.Blocks) bool {
	for _, block := range blocks.BlockSet {
		if block.BlockType() == slack.MBTImage {
			return true
		}
	}
	return false
}
