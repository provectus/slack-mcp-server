// @layer: integration
// @spec: 001-channel-members
package provider

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

// This file is the internal (package provider) half of a two-file test seam.
// pkg/provider/channels_members_handler_gaps_test.go needs to drive the real
// handler.ChannelsMembersHandler end-to-end against a mocked SlackAPI, which
// means it must import pkg/handler. An internal `package provider` test file
// cannot do that: pkg/handler imports pkg/provider, and Go treats the
// internal-test-augmented "provider" package as the same identity as plain
// "provider", so provider(+tests) -> handler -> provider is rejected as an
// import cycle. The fix is the standard Go pattern: keep the handler import in
// an *external* `package provider_test` file (which Go treats as a genuinely
// separate package, so no cycle), and expose just enough of ApiProvider's
// unexported machinery through small, clearly-named EXPORTED test-only helpers
// here, in an internal file that only needs slack-go/testify/zap — not
// pkg/handler.
//
// Nothing here changes production behavior: these helpers only assemble an
// ApiProvider from its already-existing unexported fields (the same fields
// api_members_test.go's newMembersTestProvider pokes directly) and are only
// ever compiled into `go test` binaries.

// NewTestApiProviderForHandlerGaps builds an ApiProvider wired to the given
// mock SlackAPI client, with users+channels marked ready (empty snapshots), an
// empty channel-members snapshot, a 1h cache TTL, and a 1h min-refresh-interval
// (so refresh_cache tests can opt into deterministic rate-limiting via
// SetLastForcedMembersRefreshForTest, or opt out via SetMinRefreshInterval(0)).
func NewTestApiProviderForHandlerGaps(t *testing.T, client SlackAPI) *ApiProvider {
	t.Helper()

	ap := &ApiProvider{
		client:                   client,
		logger:                   zap.NewNop(),
		rateLimiter:              rate.NewLimiter(rate.Inf, 100),
		cacheTTL:                 time.Hour,
		minRefreshInterval:       time.Hour,
		membersCachePath:         filepath.Join(t.TempDir(), "channel_members_cache.json"),
		lastForcedMembersRefresh: make(map[string]time.Time),
	}
	ap.membersSnapshot.Store(&ChannelMembersCache{Channels: map[string]ChannelMemberRoster{}})
	ap.usersSnapshot.Store(&UsersCache{Users: map[string]slack.User{}, UsersInv: map[string]string{}})
	ap.channelsSnapshot.Store(&ChannelsCache{Channels: map[string]Channel{}, ChannelsInv: map[string]string{}})
	ap.usersReady.Store(true)
	ap.channelsReady.Store(true)

	return ap
}

// SetMinRefreshInterval overrides the forced-refresh throttle window (e.g. 0 to
// disable throttling for a test that wants an unconditional refetch).
func (ap *ApiProvider) SetMinRefreshInterval(d time.Duration) {
	ap.minRefreshInterval = d
}

// SetMembersSnapshotForTest replaces the channel-members snapshot wholesale,
// mirroring the copy-on-write pattern fetchAndStoreChannelMembers itself uses.
func (ap *ApiProvider) SetMembersSnapshotForTest(cache *ChannelMembersCache) {
	ap.membersSnapshot.Store(cache)
}

// SetLastForcedMembersRefreshForTest seeds the per-channel forced-refresh
// throttle timestamp so a subsequent ForceRefreshChannelMembers call for that
// channel falls inside (or outside) the min-refresh-interval window.
func (ap *ApiProvider) SetLastForcedMembersRefreshForTest(channelID string, ts time.Time) {
	ap.membersMu.Lock()
	defer ap.membersMu.Unlock()
	ap.lastForcedMembersRefresh[channelID] = ts
}

// MarkMembersRefreshInFlightForTest simulates a background gather already
// running for channelID, so a subsequent GetChannelMembers call for a
// not-yet-cached channel returns ErrChannelMembersNotReady instead of
// launching a synchronous fetch.
func (ap *ApiProvider) MarkMembersRefreshInFlightForTest(channelID string) {
	ap.refreshingMembers.Store(channelID, struct{}{})
}
