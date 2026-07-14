// @layer: integration
// @spec: 001-channel-members
package handler

import (
	"testing"
	"time"

	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"github.com/stretchr/testify/assert"
)

// TestChannelsMembersHandlerEmptyChannel closes a whole-feature acceptance gap:
// functional-spec.md AC "Given an empty or members-hidden channel, when the
// assistant requests its members, then it receives an empty list rather than
// an error." pkg/provider/api_members_test.go already proves the provider
// layer treats an empty roster as a valid, cacheable entry
// (TestChannelMembersEmptyChannel); this test proves the same guarantee holds
// through the actual user-facing surface (ChannelsMembersHandler): no error,
// a well-formed CSV header, and zero data rows.
//
// RED validation: if the handler (or the provider read path underneath it)
// ever special-cased a zero-length roster as an error/not-ready condition
// instead of a valid empty result, callMembers's require.NoError(t, err) or
// require.NotNil(t, res) in the shared test helper would fail, and
// parseMemberCSV would either error on malformed output or return a non-empty
// row set. This reuses the same real (non-mocked, network-free) provider
// fixture as TestChannelsMembersHandlerDefaultRoster in
// channels_members_test.go, so it is exercised against production code, not a
// stub.
func TestChannelsMembersHandlerEmptyChannel(t *testing.T) {
	rosters := map[string]provider.ChannelMemberRoster{
		"C_EMPTY": {MemberIDs: []string{}, FetchedAt: time.Now()},
	}
	h := newMembersHandlerFixture(t, rosters, nil, nil, nil)

	csvText := resultText(t, callMembers(t, h, map[string]any{"channel_id": "C_EMPTY"}))

	rows := parseMemberCSV(t, csvText)
	assert.Empty(t, rows, "an empty channel must yield an empty list, not an error or partial data")
}
