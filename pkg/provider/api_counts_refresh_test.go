package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/korotovsky/slack-mcp-server/pkg/provider/edge"
	"github.com/korotovsky/slack-mcp-server/pkg/provider/edge/fasttime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestUnitMergeChannelCounts(t *testing.T) {
	original := &ChannelsCache{
		Channels: map[string]Channel{
			"C001": {ID: "C001", Name: "#general", HasUnreads: false, MentionCount: 0},
			"C002": {ID: "C002", Name: "#random", HasUnreads: true, MentionCount: 5},
		},
		ChannelsInv: map[string]string{"#general": "C001", "#random": "C002"},
	}

	counts := map[string]edge.ChannelSnapshot{
		"C001": {
			ID:           "C001",
			HasUnreads:   true,
			MentionCount: 3,
			LastRead:     fasttime.Time(time.UnixMicro(1700000000123456)),
		},
	}

	merged := mergeChannelCounts(original, counts)

	// Matched channel gets fresh unread info
	assert.True(t, merged.Channels["C001"].HasUnreads)
	assert.Equal(t, 3, merged.Channels["C001"].MentionCount)
	assert.NotEmpty(t, merged.Channels["C001"].LastRead)

	// Unmatched channel is preserved as-is
	assert.True(t, merged.Channels["C002"].HasUnreads)
	assert.Equal(t, 5, merged.Channels["C002"].MentionCount)

	// Name lookup map is preserved
	assert.Equal(t, "C001", merged.ChannelsInv["#general"])

	// Original snapshot is not mutated (immutable snapshot pattern)
	assert.False(t, original.Channels["C001"].HasUnreads)
	assert.Equal(t, 0, original.Channels["C001"].MentionCount)
}

func TestUnitRefreshChannelCountsUnavailable(t *testing.T) {
	// A client that is not an *MCPSlackClient has no edge client, so counts
	// are unavailable and the caller must fall back to a full refresh.
	ap := &ApiProvider{
		client: &mockSlackClient{},
		logger: zap.NewNop(),
	}
	snapshot := &ChannelsCache{
		Channels:    map[string]Channel{"C001": {ID: "C001", Name: "#general"}},
		ChannelsInv: map[string]string{"#general": "C001"},
	}
	ap.channelsSnapshot.Store(snapshot)

	err := ap.RefreshChannelCounts(context.Background())
	require.ErrorIs(t, err, ErrCountsUnavailable)

	// Snapshot must be untouched
	assert.Equal(t, snapshot, ap.channelsSnapshot.Load())
}

func TestUnitChannelsCacheExpired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "channels_cache_v2.json")
	require.NoError(t, os.WriteFile(path, []byte("[]"), 0600))

	ap := &ApiProvider{logger: zap.NewNop()}
	ap.channelsCachePath = path

	// Fresh file, 1h TTL: not expired
	ap.cacheTTL = time.Hour
	assert.False(t, ap.channelsCacheExpired())

	// Old file, 1h TTL: expired
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))
	assert.True(t, ap.channelsCacheExpired())

	// TTL disabled: never expired
	ap.cacheTTL = 0
	assert.False(t, ap.channelsCacheExpired())

	// Missing file: treated as expired so a refresh gets scheduled
	ap.cacheTTL = time.Hour
	ap.channelsCachePath = filepath.Join(dir, "missing.json")
	assert.True(t, ap.channelsCacheExpired())
}
