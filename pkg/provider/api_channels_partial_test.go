package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

// scriptedConversationsPage is one scripted response for the mock's
// GetConversationsContext: either a page of channels with a next cursor, or
// an error.
type scriptedConversationsPage struct {
	channels []slack.Channel
	cursor   string
	err      error
}

// conversationsMockClient is a minimal SlackAPI mock that scripts
// GetConversationsContext responses in order, mirroring the membersScript
// pattern in api_patch_test.go's mockSlackClient.
type conversationsMockClient struct {
	SlackAPI // embed interface to satisfy all methods; only override what we need

	calls  int
	script []scriptedConversationsPage
}

func (m *conversationsMockClient) GetConversationsContext(ctx context.Context, params *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	idx := m.calls
	m.calls++
	if idx >= len(m.script) {
		idx = len(m.script) - 1
	}
	r := m.script[idx]
	return r.channels, r.cursor, r.err
}

// newChannelsPartialTestProvider builds an ApiProvider wired for
// fetchAndStoreChannels tests: an infinite-rate limiter (no throttling
// delays), a non-nil empty users snapshot (read by getChannelsMultiType via
// ProvideUsersMap), and a client that is not *MCPSlackClient so
// fetchChannelCounts safely no-ops.
func newChannelsPartialTestProvider(client SlackAPI, cachePath string) *ApiProvider {
	ap := &ApiProvider{
		client:      client,
		logger:      zap.NewNop(),
		rateLimiter: rate.NewLimiter(rate.Inf, 100),
	}
	ap.channelsCachePath = cachePath
	ap.usersSnapshot.Store(&UsersCache{Users: map[string]slack.User{}, UsersInv: map[string]string{}})
	return ap
}

// TestFetchAndStoreChannelsPartialPagination is a regression test for GH #14:
// a mid-pagination error used to `break` and return the partial page list as
// if it were complete, overwriting the in-memory snapshot and the on-disk
// cache with a shrunken channel list that then survived restarts.
func TestFetchAndStoreChannelsPartialPagination(t *testing.T) {
	t.Run("second page error keeps existing snapshot and does not write partial cache", func(t *testing.T) {
		goodSnapshot := &ChannelsCache{
			Channels: map[string]Channel{
				"C001": {ID: "C001", Name: "general"},
				"C002": {ID: "C002", Name: "random"},
				"C003": {ID: "C003", Name: "eng"},
			},
			ChannelsInv: map[string]string{
				"general": "C001",
				"random":  "C002",
				"eng":     "C003",
			},
		}

		client := &conversationsMockClient{
			script: []scriptedConversationsPage{
				{
					channels: []slack.Channel{
						{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C010"}, Name: "p1a"}},
						{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C011"}, Name: "p1b"}},
					},
					cursor: "next-page-cursor",
					err:    nil,
				},
				{
					channels: nil,
					cursor:   "",
					err:      errors.New("simulated conversations.list pagination failure"),
				},
			},
		}

		dir := t.TempDir()
		cachePath := filepath.Join(dir, "channels_cache_v2.json")

		ap := newChannelsPartialTestProvider(client, cachePath)
		ap.channelsSnapshot.Store(goodSnapshot)
		ap.channelsReady.Store(true)

		err := ap.fetchAndStoreChannels(context.Background())
		require.NoError(t, err, "fetchAndStoreChannels should swallow the pagination error and keep the existing cache")

		snapshot := ap.channelsSnapshot.Load()
		assert.Len(t, snapshot.Channels, 3, "the 2-channel partial page must not replace the good 3-channel snapshot")
		assert.Equal(t, goodSnapshot, snapshot, "snapshot must be untouched by the failed refresh")

		_, statErr := os.Stat(cachePath)
		assert.True(t, os.IsNotExist(statErr), "on-disk cache must not be written when the fetch failed mid-pagination")
	})

	t.Run("first page error on cold start returns error and leaves snapshot unset", func(t *testing.T) {
		client := &conversationsMockClient{
			script: []scriptedConversationsPage{
				{
					channels: nil,
					cursor:   "",
					err:      errors.New("simulated conversations.list failure on first page"),
				},
			},
		}

		dir := t.TempDir()
		cachePath := filepath.Join(dir, "channels_cache_v2.json")

		ap := newChannelsPartialTestProvider(client, cachePath)
		// channelsReady left false, no pre-seeded snapshot: cold start.

		err := ap.fetchAndStoreChannels(context.Background())
		assert.Error(t, err, "cold start with no existing cache must surface the pagination error")

		assert.Nil(t, ap.channelsSnapshot.Load(), "snapshot must remain unset on a cold-start failure")
		assert.False(t, ap.channelsReady.Load())

		_, statErr := os.Stat(cachePath)
		assert.True(t, os.IsNotExist(statErr), "on-disk cache must not be written on a cold-start failure")
	})
}
