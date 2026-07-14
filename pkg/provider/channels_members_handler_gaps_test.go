// @layer: integration
// @spec: 001-channel-members
package provider_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/korotovsky/slack-mcp-server/pkg/handler"
	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// This file closes three handler-level gaps flagged for the "List Channel
// Members" feature (context/spec/001-channel-members) that the slice-2 handler
// tests (pkg/handler/channels_members_test.go) could not exercise: that
// fixture boots the provider via provider.New with a "demo" token, which
// leaves the SlackAPI client as a nil *MCPSlackClient. Any code path that
// reaches a real Slack call (ForceRefreshChannels's #name-miss retry,
// ForceRefreshChannelMembers) panics on that nil client, so the "unknown
// channel", "not-ready", and "refresh_cache" paths were untestable from
// package handler.
//
// The seam: this is an *external* `provider_test` package (not `provider`),
// so it can safely import both pkg/provider and pkg/handler without an import
// cycle (pkg/handler imports pkg/provider; an *internal* `package provider`
// test file cannot also import pkg/handler for that reason — see
// testseam_channels_members_test.go for the internal half of this seam, which
// exposes just enough of ApiProvider's unexported wiring through small
// exported test-only helpers). This drives the real
// handler.ChannelsHandler.ChannelsMembersHandler end-to-end against a mocked
// SlackAPI, with no production code changed.

// gapsMock is a minimal provider.SlackAPI mock for these tests. It implements
// only GetConversationsContext (consulted by resolveChannelID's
// ForceRefreshChannels retry when a #name reference misses the cache) and
// GetUsersInConversationContext (consulted by the channel-members
// fetch/refresh path).
type gapsMock struct {
	provider.SlackAPI // embed nil interface; only override what these tests exercise

	convChannels []slack.Channel

	mu           sync.Mutex
	membersIDs   []string
	membersCalls int
}

func (m *gapsMock) GetConversationsContext(ctx context.Context, params *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	return m.convChannels, "", nil
}

func (m *gapsMock) GetUsersInConversationContext(ctx context.Context, params *slack.GetUsersInConversationParameters) ([]string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.membersCalls++
	return m.membersIDs, "", nil
}

func (m *gapsMock) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.membersCalls
}

func callMembersHandler(t *testing.T, h *handler.ChannelsHandler, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	var req mcp.CallToolRequest
	req.Params.Name = "channels_members"
	req.Params.Arguments = args
	res, err := h.ChannelsMembersHandler(context.Background(), req)
	require.NoError(t, err, "handler must return a clean tool result, not a protocol error")
	require.NotNil(t, res)
	return res
}

func membersResultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.Len(t, res.Content, 1)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected TextContent")
	return tc.Text
}

// TestChannelsMembersHandlerUnknownChannelNotFound covers the functional-spec
// AC (section 2, "identifies the channel by either its ID or its name"):
// "Given a channel reference that matches no known channel ... receives a
// clear message saying the channel was not found, and no partial or
// misleading list."
//
// RED validation: resolveChannelID only validates #/@-prefixed references
// against ChannelsInv; a raw channel ID is passed through unresolved (matching
// every other handler in this codebase, e.g. conversations.go). Swapping this
// test's channel_id from "#does-not-exist" to a raw unknown ID such as
// "CUNKNOWN" demonstrates the counterfactual failure this test guards
// against: with a raw ID, resolveChannelID returns it unchanged,
// GetChannelMembers treats it as a cache miss, and the handler happily
// fetches+returns gapsMock's scripted members as a real (misleading) roster
// instead of a not-found message. That confirms the assertions below actually
// discriminate correct behavior from the bug the AC guards against, without
// needing to revert production code.
func TestChannelsMembersHandlerUnknownChannelNotFound(t *testing.T) {
	mock := &gapsMock{membersIDs: []string{"USHOULDNOTAPPEAR"}}
	ap := provider.NewTestApiProviderForHandlerGaps(t, mock)
	h := handler.NewChannelsHandler(ap, zap.NewNop())

	res := callMembersHandler(t, h, map[string]any{"channel_id": "#does-not-exist"})
	text := membersResultText(t, res)

	assert.Contains(t, strings.ToLower(text), "not found",
		"unknown channel reference must yield a clear not-found message")
	assert.NotContains(t, text, "UserID",
		"no partial/misleading member list must be returned for an unknown channel")
	assert.NotContains(t, text, "USHOULDNOTAPPEAR")
}

// TestChannelsMembersHandlerNotReadySentinel covers the functional-spec AC
// (section 2, "While the roster is first being gathered..."): "Given a channel
// whose roster has never been gathered and a background gather is in progress
// ... receives a short 'still preparing, please retry shortly' message rather
// than an empty or partial list."
//
// RED validation: if the handler's
// `errors.Is(err, provider.ErrChannelMembersNotReady)` branch were
// removed/broken, GetChannelMembers's ErrChannelMembersNotReady would fall
// through to the generic `return nil, fmt.Errorf(...)` path, so the handler
// would return (nil, non-nil error) instead of a clean TextContent result. The
// require.NoError/require.NotNil in callMembersHandler would fail immediately
// in that scenario, proving this test fails without the sentinel-translation
// logic.
func TestChannelsMembersHandlerNotReadySentinel(t *testing.T) {
	mock := &gapsMock{}
	ap := provider.NewTestApiProviderForHandlerGaps(t, mock)
	// Simulate a gather already in flight for this channel (no roster yet).
	ap.MarkMembersRefreshInFlightForTest("C1")
	h := handler.NewChannelsHandler(ap, zap.NewNop())

	res := callMembersHandler(t, h, map[string]any{"channel_id": "C1"})
	text := membersResultText(t, res)

	assert.Equal(t, provider.ErrChannelMembersNotReady.Error(), text,
		"handler must surface the channel-members not-ready sentinel verbatim")
}

// TestChannelsMembersHandlerRefreshRateLimitedServesCached covers the
// technical-considerations §4 case: "refresh_cache=true triggers
// ForceRefreshChannelMembers; rate-limited refresh logs and serves cached data
// rather than erroring" (the handler-level counterpart of the flagged gap:
// pkg/provider/api_members_test.go only asserts the throttle at the provider
// layer, never that the handler tolerates it and still answers successfully).
//
// RED validation: if the handler treated ErrRefreshRateLimited as a hard error
// (naive `if err != nil { return nil, err }`), this call would return a
// non-nil error instead of a result, and the returned roster would never be
// observed — both require.NoError(err) and the "UCACHED" assertion below
// would fail.
func TestChannelsMembersHandlerRefreshRateLimitedServesCached(t *testing.T) {
	mock := &gapsMock{membersIDs: []string{"USHOULDNOTAPPEAR"}}
	ap := provider.NewTestApiProviderForHandlerGaps(t, mock)
	ap.SetMembersSnapshotForTest(&provider.ChannelMembersCache{Channels: map[string]provider.ChannelMemberRoster{
		"C1": {MemberIDs: []string{"UCACHED"}, FetchedAt: time.Now()},
	}})
	// A forced refresh "just happened" for C1, so the next one falls inside
	// the 1h min-refresh-interval and must be throttled.
	ap.SetLastForcedMembersRefreshForTest("C1", time.Now())
	h := handler.NewChannelsHandler(ap, zap.NewNop())

	res := callMembersHandler(t, h, map[string]any{"channel_id": "C1", "refresh_cache": true})
	text := membersResultText(t, res)

	assert.Contains(t, text, "UCACHED",
		"a rate-limited refresh_cache request must still serve the existing cached roster")
	assert.NotContains(t, text, "USHOULDNOTAPPEAR",
		"a rate-limited refresh must not perform a new Slack fetch")
	assert.Equal(t, 0, mock.callCount(), "rate-limited refresh must not call Slack at all")
}

// TestChannelsMembersHandlerRefreshCacheForcesRefetch is the positive
// counterpart of the rate-limited case above, covering the functional-spec AC:
// "When the assistant explicitly asks to refresh a channel's members, then the
// roster is re-fetched from Slack and the returned list reflects the current
// membership." No prior handler-level test exercised a successful
// refresh_cache round-trip end-to-end.
//
// RED validation: if the handler ignored `refresh_cache` (never called
// ForceRefreshChannelMembers), GetChannelMembers would serve the stale "UOLD"
// entry unchanged (still fresh within the 1h TTL) and the mock would never be
// called — both the "UNEW1"/"UNEW2" assertions and the call-count assertion
// below would fail.
func TestChannelsMembersHandlerRefreshCacheForcesRefetch(t *testing.T) {
	mock := &gapsMock{membersIDs: []string{"UNEW1", "UNEW2"}}
	ap := provider.NewTestApiProviderForHandlerGaps(t, mock)
	ap.SetMinRefreshInterval(0) // disable throttling to isolate the refetch behavior
	ap.SetMembersSnapshotForTest(&provider.ChannelMembersCache{Channels: map[string]provider.ChannelMemberRoster{
		"C1": {MemberIDs: []string{"UOLD"}, FetchedAt: time.Now()},
	}})
	h := handler.NewChannelsHandler(ap, zap.NewNop())

	res := callMembersHandler(t, h, map[string]any{"channel_id": "C1", "refresh_cache": true})
	text := membersResultText(t, res)

	assert.Contains(t, text, "UNEW1")
	assert.Contains(t, text, "UNEW2")
	assert.NotContains(t, text, "UOLD",
		"explicit refresh must reflect current membership, not the stale cache")
	assert.Equal(t, 1, mock.callCount(), "exactly one Slack fetch performed for the forced refresh")
}
