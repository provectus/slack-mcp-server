package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gocarina/gocsv"
	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// newMembersHandlerFixture builds a ChannelsHandler backed by a real, network-free
// *provider.ApiProvider. It mirrors the provider-tier harness (temp cache path +
// pre-populated members snapshot) but from package handler, where the ApiProvider
// fields are unexported: it boots the provider in "demo" mode (New skips the Slack
// client, initializes empty snapshots, and honors the *_CACHE env overrides), marks
// the caches ready via SkipCache, seeds users/channels by mutating the live snapshot
// maps returned by Provide*, and loads the member rosters from a temp cache file via
// RefreshChannelMembers. Because every seeded roster is fresh (FetchedAt=now, default
// 24h TTL), GetChannelMembers serves each read from cache with no Slack call.
func newMembersHandlerFixture(
	t *testing.T,
	rosters map[string]provider.ChannelMemberRoster,
	users map[string]slack.User,
	channels map[string]provider.Channel,
	channelsInv map[string]string,
) *ChannelsHandler {
	t.Helper()

	dir := t.TempDir()
	membersPath := filepath.Join(dir, "TEAMA_channel_members_cache.json")

	cache := provider.ChannelMembersCache{Channels: rosters}
	data, err := json.MarshalIndent(cache, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(membersPath, data, 0600))

	// Boot the provider offline. "demo" short-circuits auth/network in
	// provider.New; the *_CACHE overrides keep every file under t.TempDir.
	t.Setenv("SLACK_MCP_XOXP_TOKEN", "demo")
	t.Setenv("SLACK_MCP_XOXB_TOKEN", "")
	t.Setenv("SLACK_MCP_XOXC_TOKEN", "")
	t.Setenv("SLACK_MCP_XOXD_TOKEN", "")
	t.Setenv("SLACK_MCP_CHANNEL_MEMBERS_CACHE", membersPath)
	t.Setenv("SLACK_MCP_USERS_CACHE", filepath.Join(dir, "users_cache.json"))
	t.Setenv("SLACK_MCP_CHANNELS_CACHE", filepath.Join(dir, "channels_cache.json"))

	ap := provider.New("stdio", zap.NewNop())
	ap.SkipCache() // mark users+channels ready for IsReady() without a fetch

	um := ap.ProvideUsersMap()
	for id, u := range users {
		um.Users[id] = u
	}

	cm := ap.ProvideChannelsMaps()
	for id, c := range channels {
		cm.Channels[id] = c
	}
	for name, id := range channelsInv {
		cm.ChannelsInv[name] = id
	}

	require.NoError(t, ap.RefreshChannelMembers(context.Background()))

	return NewChannelsHandler(ap, zap.NewNop())
}

// callMembers invokes ChannelsMembersHandler and returns the text payload.
func callMembers(t *testing.T, h *ChannelsHandler, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	var req mcp.CallToolRequest
	req.Params.Name = "channels_members"
	req.Params.Arguments = args
	res, err := h.ChannelsMembersHandler(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	return res
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.Len(t, res.Content, 1)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected TextContent")
	return tc.Text
}

// parseMemberCSV parses the handler's CSV payload back into ChannelMember rows.
func parseMemberCSV(t *testing.T, csvText string) []ChannelMember {
	t.Helper()
	var rows []ChannelMember
	require.NoError(t, gocsv.UnmarshalString(csvText, &rows))
	return rows
}

func memberIDs(rows []ChannelMember) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.UserID)
	}
	return ids
}

// TestChannelsMembersHandlerIDvsNameParity asserts that referencing a channel by
// its raw ID and by its #name resolve to the identical roster (spec: "same members
// are returned as if the ID had been supplied").
func TestChannelsMembersHandlerIDvsNameParity(t *testing.T) {
	rosters := map[string]provider.ChannelMemberRoster{
		"C1": {MemberIDs: []string{"U1", "U2"}, FetchedAt: time.Now()},
	}
	users := map[string]slack.User{
		"U1": {ID: "U1", Name: "alice", RealName: "Alice A", Profile: slack.UserProfile{DisplayName: "al"}},
		"U2": {ID: "U2", Name: "bob", RealName: "Bob B", Profile: slack.UserProfile{DisplayName: "bo"}},
	}
	channels := map[string]provider.Channel{"C1": {ID: "C1", Name: "general"}}
	inv := map[string]string{"#general": "C1"}

	h := newMembersHandlerFixture(t, rosters, users, channels, inv)

	byID := resultText(t, callMembers(t, h, map[string]any{"channel_id": "C1"}))
	byName := resultText(t, callMembers(t, h, map[string]any{"channel_id": "#general"}))

	assert.Equal(t, byID, byName, "ID and #name references must yield the identical roster")
	assert.Equal(t, []string{"U1", "U2"}, memberIDs(parseMemberCSV(t, byID)))
}

// TestChannelsMembersHandlerDefaultRoster verifies the default (unfiltered) roster
// includes a normal user, a bot, a deactivated user, and a member present in the
// roster but ABSENT from the users map (which appears by ID with blank names), and
// that the CSV header columns are exactly UserID,DisplayName,RealName.
func TestChannelsMembersHandlerDefaultRoster(t *testing.T) {
	rosters := map[string]provider.ChannelMemberRoster{
		"C1": {MemberIDs: []string{"U_norm", "U_bot", "U_del", "U_unknown"}, FetchedAt: time.Now()},
	}
	users := map[string]slack.User{
		"U_norm": {ID: "U_norm", Name: "norm", RealName: "Norm Human", Profile: slack.UserProfile{DisplayName: "normie"}},
		"U_bot":  {ID: "U_bot", Name: "botty", RealName: "Bot User", IsBot: true, Profile: slack.UserProfile{DisplayName: "bot"}},
		"U_del":  {ID: "U_del", Name: "ghost", RealName: "Deleted User", Deleted: true, Profile: slack.UserProfile{DisplayName: "gone"}},
		// U_unknown deliberately omitted from the users map.
	}
	h := newMembersHandlerFixture(t, rosters, users, nil, nil)

	csvText := resultText(t, callMembers(t, h, map[string]any{"channel_id": "C1"}))

	header := strings.SplitN(strings.TrimSpace(csvText), "\n", 2)[0]
	assert.Equal(t, "UserID,DisplayName,RealName", strings.TrimRight(header, "\r"),
		"CSV header columns must be exactly UserID,DisplayName,RealName")

	rows := parseMemberCSV(t, csvText)
	assert.ElementsMatch(t, []string{"U_norm", "U_bot", "U_del", "U_unknown"}, memberIDs(rows),
		"default roster includes everyone: normal, bot, deactivated, and unknown-by-ID")

	byID := map[string]ChannelMember{}
	for _, r := range rows {
		byID[r.UserID] = r
	}
	// Names come from the users snapshot for known members...
	assert.Equal(t, "normie", byID["U_norm"].DisplayName)
	assert.Equal(t, "Norm Human", byID["U_norm"].RealName)
	// ...and a member missing from the users map is still present, by ID, with blank names.
	require.Contains(t, byID, "U_unknown")
	assert.Empty(t, byID["U_unknown"].DisplayName)
	assert.Empty(t, byID["U_unknown"].RealName)
}

// TestChannelsMembersHandlerFilters verifies exclude_bots / exclude_deactivated and
// their combination against an all-known roster (normal + bot + deactivated).
func TestChannelsMembersHandlerFilters(t *testing.T) {
	rosters := map[string]provider.ChannelMemberRoster{
		"C1": {MemberIDs: []string{"U_norm", "U_bot", "U_del"}, FetchedAt: time.Now()},
	}
	users := map[string]slack.User{
		"U_norm": {ID: "U_norm", Name: "norm", RealName: "Norm Human"},
		"U_bot":  {ID: "U_bot", Name: "botty", RealName: "Bot User", IsBot: true},
		"U_del":  {ID: "U_del", Name: "ghost", RealName: "Deleted User", Deleted: true},
	}

	cases := []struct {
		name    string
		args    map[string]any
		wantIDs []string
	}{
		{"exclude_bots omits only the bot", map[string]any{"channel_id": "C1", "exclude_bots": true}, []string{"U_norm", "U_del"}},
		{"exclude_deactivated omits only the deleted user", map[string]any{"channel_id": "C1", "exclude_deactivated": true}, []string{"U_norm", "U_bot"}},
		{"both leave only the active human", map[string]any{"channel_id": "C1", "exclude_bots": true, "exclude_deactivated": true}, []string{"U_norm"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newMembersHandlerFixture(t, rosters, users, nil, nil)
			rows := parseMemberCSV(t, resultText(t, callMembers(t, h, tc.args)))
			assert.ElementsMatch(t, tc.wantIDs, memberIDs(rows))
		})
	}
}

// TestChannelsMembersHandlerCompleteRoster verifies a large (>1000) roster is
// returned in full, with no pagination/truncation (spec: complete roster in one
// response).
func TestChannelsMembersHandlerCompleteRoster(t *testing.T) {
	const n = 1500
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("U%04d", i)
	}
	rosters := map[string]provider.ChannelMemberRoster{
		"C_big": {MemberIDs: ids, FetchedAt: time.Now()},
	}
	h := newMembersHandlerFixture(t, rosters, nil, nil, nil)

	rows := parseMemberCSV(t, resultText(t, callMembers(t, h, map[string]any{"channel_id": "C_big"})))
	assert.Len(t, rows, n, "every member must appear; no truncation")
	assert.ElementsMatch(t, ids, memberIDs(rows))
}
