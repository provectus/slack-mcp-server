package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

// newMembersTestProvider builds an ApiProvider wired for channel-member tests:
// an infinite-rate limiter (no throttling delays), a 1h TTL, throttling
// disabled by default, and a temp cache path. Mirrors the field-injection
// harness used by the other provider tests (newTestApiProvider).
func newMembersTestProvider(client SlackAPI, cachePath string) *ApiProvider {
	ap := &ApiProvider{
		client:                   client,
		logger:                   zap.NewNop(),
		rateLimiter:              rate.NewLimiter(rate.Inf, 100),
		cacheTTL:                 time.Hour,
		membersCachePath:         cachePath,
		lastForcedMembersRefresh: make(map[string]time.Time),
	}
	ap.membersSnapshot.Store(&ChannelMembersCache{Channels: map[string]ChannelMemberRoster{}})
	return ap
}

// TestChannelMembersColdFetchMultiPage drives the real MCPSlackClient pagination
// against an httptest server so the production multi-page assembly is exercised
// end-to-end: several cursor-paginated pages are stitched into one complete
// roster, and GetChannelMembers persists it to the cache file atomically.
func TestChannelMembersColdFetchMultiPage(t *testing.T) {
	pages := map[string]struct {
		members    []string
		nextCursor string
	}{
		"":     {members: []string{"U1", "U2"}, nextCursor: "cur1"},
		"cur1": {members: []string{"U3", "U4"}, nextCursor: "cur2"},
		"cur2": {members: []string{"U5"}, nextCursor: ""},
	}
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		assert.Equal(t, "C123", r.FormValue("channel"))
		atomic.AddInt32(&hits, 1)
		p := pages[r.FormValue("cursor")]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":                true,
			"members":           p.members,
			"response_metadata": map[string]string{"next_cursor": p.nextCursor},
		})
	}))
	defer srv.Close()

	client := &MCPSlackClient{
		slackClient: slack.New("xoxp-test", slack.OptionAPIURL(srv.URL+"/")),
		isOAuth:     true,
	}
	path := filepath.Join(t.TempDir(), "TEAMA_channel_members_cache.json")
	ap := newMembersTestProvider(client, path)

	ids, err := ap.GetChannelMembers(context.Background(), "C123")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"U1", "U2", "U3", "U4", "U5"}, ids,
		"all pages must be assembled into the complete roster")
	assert.Equal(t, int32(3), atomic.LoadInt32(&hits), "should have paginated 3 pages")

	// Cache file written atomically and unmarshals with every ID.
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var cache ChannelMembersCache
	require.NoError(t, json.Unmarshal(data, &cache))
	roster, ok := cache.Channels["C123"]
	require.True(t, ok, "channel roster must be persisted")
	assert.ElementsMatch(t, []string{"U1", "U2", "U3", "U4", "U5"}, roster.MemberIDs)
	assert.False(t, roster.FetchedAt.IsZero(), "FetchedAt must be stamped")
}

// TestChannelMembersReadHitWithinTTL verifies a fresh cached roster is served
// from the snapshot with zero Slack calls, even on repeated reads.
func TestChannelMembersReadHitWithinTTL(t *testing.T) {
	mock := &mockSlackClient{
		membersScript: []scriptedMembers{{ids: []string{"SHOULD_NOT_FETCH"}}},
	}
	ap := newMembersTestProvider(mock, filepath.Join(t.TempDir(), "m.json"))
	ap.membersSnapshot.Store(&ChannelMembersCache{Channels: map[string]ChannelMemberRoster{
		"C1": {MemberIDs: []string{"U1", "U2"}, FetchedAt: time.Now()},
	}})

	ids, err := ap.GetChannelMembers(context.Background(), "C1")
	require.NoError(t, err)
	assert.Equal(t, []string{"U1", "U2"}, ids)

	// Second read is still served from the snapshot.
	ids2, err := ap.GetChannelMembers(context.Background(), "C1")
	require.NoError(t, err)
	assert.Equal(t, []string{"U1", "U2"}, ids2)

	assert.Equal(t, 0, mock.membersCallCount(), "a fresh cached entry must not hit Slack")
}

// TestChannelMembersTTLExpiryServesStaleAndRefreshes verifies a per-entry
// expired roster is served stale immediately while a background refresh updates
// the entry (IDs replaced, FetchedAt advanced).
func TestChannelMembersTTLExpiryServesStaleAndRefreshes(t *testing.T) {
	mock := &mockSlackClient{
		membersScript: []scriptedMembers{{ids: []string{"U_NEW1", "U_NEW2"}}},
	}
	ap := newMembersTestProvider(mock, filepath.Join(t.TempDir(), "m.json"))
	ap.cacheTTL = time.Hour

	old := time.Now().Add(-2 * time.Hour) // older than the TTL
	ap.membersSnapshot.Store(&ChannelMembersCache{Channels: map[string]ChannelMemberRoster{
		"C1": {MemberIDs: []string{"U_OLD"}, FetchedAt: old},
	}})

	// The read returns the stale IDs synchronously.
	ids, err := ap.GetChannelMembers(context.Background(), "C1")
	require.NoError(t, err)
	assert.Equal(t, []string{"U_OLD"}, ids, "expired entry is served stale")

	// The spawned background refresh replaces the entry with fresh data.
	require.Eventually(t, func() bool {
		r := ap.membersSnapshot.Load().Channels["C1"]
		return r.FetchedAt.After(old) && len(r.MemberIDs) == 2
	}, 2*time.Second, 10*time.Millisecond, "background refresh should update the entry")

	r := ap.membersSnapshot.Load().Channels["C1"]
	assert.ElementsMatch(t, []string{"U_NEW1", "U_NEW2"}, r.MemberIDs)
	assert.True(t, r.FetchedAt.After(old), "FetchedAt must advance")
	assert.GreaterOrEqual(t, mock.membersCallCount(), 1)
}

// TestChannelMembersRestartSurvival verifies a provider pointed at a
// pre-written cache file loads it via RefreshChannelMembers on boot and then
// serves reads from disk with no Slack call.
func TestChannelMembersRestartSurvival(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "TEAMA_channel_members_cache.json")

	cache := ChannelMembersCache{Channels: map[string]ChannelMemberRoster{
		"C1": {MemberIDs: []string{"U1", "U2", "U3"}, FetchedAt: time.Now()},
	}}
	data, err := json.MarshalIndent(cache, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))

	mock := &mockSlackClient{membersScript: []scriptedMembers{{ids: []string{"SHOULD_NOT_FETCH"}}}}
	ap := newMembersTestProvider(mock, path)
	// Fresh boot: snapshot starts empty until the disk load runs.
	ap.membersSnapshot.Store(&ChannelMembersCache{Channels: map[string]ChannelMemberRoster{}})

	require.NoError(t, ap.RefreshChannelMembers(context.Background()))

	ids, err := ap.GetChannelMembers(context.Background(), "C1")
	require.NoError(t, err)
	assert.Equal(t, []string{"U1", "U2", "U3"}, ids)
	assert.Equal(t, 0, mock.membersCallCount(), "roster served from disk, no Slack call")
}

// TestChannelMembersEmptyChannel verifies an empty channel yields an empty
// roster (not an error), cached as a valid entry that is not re-fetched.
func TestChannelMembersEmptyChannel(t *testing.T) {
	mock := &mockSlackClient{membersScript: []scriptedMembers{{ids: nil}}} // empty page, no error
	ap := newMembersTestProvider(mock, filepath.Join(t.TempDir(), "m.json"))

	ids, err := ap.GetChannelMembers(context.Background(), "C_EMPTY")
	require.NoError(t, err, "empty channel must not be an error")
	assert.Empty(t, ids)

	roster, ok := ap.membersSnapshot.Load().Channels["C_EMPTY"]
	require.True(t, ok, "empty roster must be cached as a valid entry")
	assert.Empty(t, roster.MemberIDs)
	assert.False(t, roster.FetchedAt.IsZero())

	// The cached empty entry is served without another fetch.
	_, err = ap.GetChannelMembers(context.Background(), "C_EMPTY")
	require.NoError(t, err)
	assert.Equal(t, 1, mock.membersCallCount(), "empty entry is cached, not re-fetched")
}

// TestChannelMembersForceRefreshThrottle verifies ForceRefreshChannelMembers is
// rate-limited within minRefreshInterval and re-fetches once the window passes.
func TestChannelMembersForceRefreshThrottle(t *testing.T) {
	mock := &mockSlackClient{membersScript: []scriptedMembers{{ids: []string{"U1"}}}}
	ap := newMembersTestProvider(mock, filepath.Join(t.TempDir(), "m.json"))
	ap.minRefreshInterval = time.Hour

	ids, err := ap.ForceRefreshChannelMembers(context.Background(), "C1")
	require.NoError(t, err)
	assert.Equal(t, []string{"U1"}, ids)
	assert.Equal(t, 1, mock.membersCallCount())

	// A second force within the window is rate-limited with no fetch.
	_, err = ap.ForceRefreshChannelMembers(context.Background(), "C1")
	require.ErrorIs(t, err, ErrRefreshRateLimited)
	assert.Equal(t, 1, mock.membersCallCount())

	// Simulate the throttle window elapsing (no real sleep) and force again.
	ap.membersMu.Lock()
	ap.lastForcedMembersRefresh["C1"] = time.Now().Add(-2 * time.Hour)
	ap.membersMu.Unlock()

	_, err = ap.ForceRefreshChannelMembers(context.Background(), "C1")
	require.NoError(t, err)
	assert.Equal(t, 2, mock.membersCallCount(), "re-fetches after the throttle window")
}

// TestChannelMembersConcurrentGuard verifies the per-channel in-flight guard:
// with a gather already registered, a concurrent first-time read gets
// ErrChannelMembersNotReady instead of launching a duplicate fetch; without the
// guard, the read performs the synchronous first fetch.
func TestChannelMembersConcurrentGuard(t *testing.T) {
	t.Run("in-flight guard returns not-ready", func(t *testing.T) {
		mock := &mockSlackClient{membersScript: []scriptedMembers{{ids: []string{"U1"}}}}
		ap := newMembersTestProvider(mock, filepath.Join(t.TempDir(), "m.json"))
		// Simulate a gather already in progress for this channel.
		ap.refreshingMembers.Store("C1", struct{}{})

		_, err := ap.GetChannelMembers(context.Background(), "C1")
		require.ErrorIs(t, err, ErrChannelMembersNotReady)
		assert.Equal(t, 0, mock.membersCallCount(), "must not launch a duplicate fetch")
	})

	t.Run("no guard performs the synchronous first fetch", func(t *testing.T) {
		mock := &mockSlackClient{membersScript: []scriptedMembers{{ids: []string{"U1", "U2"}}}}
		ap := newMembersTestProvider(mock, filepath.Join(t.TempDir(), "m.json"))

		ids, err := ap.GetChannelMembers(context.Background(), "C1")
		require.NoError(t, err)
		assert.Equal(t, []string{"U1", "U2"}, ids)
		assert.Equal(t, 1, mock.membersCallCount())
	})
}

// blockingMembersMock is a SlackAPI whose members fetch runs an onFetch hook
// (used to synchronize concurrent first-run callers) and returns a single page.
type blockingMembersMock struct {
	SlackAPI // embed interface; only override the members fetch
	ids      []string
	onFetch  func()
}

func (m *blockingMembersMock) GetUsersInConversationContext(ctx context.Context, params *slack.GetUsersInConversationParameters) ([]string, string, error) {
	if m.onFetch != nil {
		m.onFetch()
	}
	return m.ids, "", nil
}

// TestChannelMembersFirstRunConcurrentGuard exercises the real first-run guard
// end-to-end (no manual refreshingMembers.Store / test seam): two concurrent
// first-time callers race on the same absent channel. Exactly one performs the
// underlying Slack fetch; the other must receive ErrChannelMembersNotReady
// instead of launching a duplicate gather. The winner is held inside the fetch
// via a barrier so the race resolves deterministically without arbitrary sleeps.
func TestChannelMembersFirstRunConcurrentGuard(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls int32
	mock := &blockingMembersMock{
		ids: []string{"U1", "U2"},
		onFetch: func() {
			atomic.AddInt32(&calls, 1)
			entered <- struct{}{} // signal: guard claimed, now inside the fetch
			<-release             // block until the test lets the winner finish
		},
	}
	ap := newMembersTestProvider(mock, filepath.Join(t.TempDir(), "m.json"))

	type result struct {
		ids []string
		err error
	}
	winnerCh := make(chan result, 1)
	go func() {
		ids, err := ap.GetChannelMembers(context.Background(), "C1")
		winnerCh <- result{ids, err}
	}()

	// The winning goroutine has claimed the per-channel guard and is now blocked
	// inside the fetch. A second first-time caller races in and must lose.
	<-entered
	_, loserErr := ap.GetChannelMembers(context.Background(), "C1")
	require.ErrorIs(t, loserErr, ErrChannelMembersNotReady,
		"the concurrent first-time caller must get not-ready, not a duplicate fetch")

	// Let the winner complete and verify it got the full roster.
	close(release)
	w := <-winnerCh
	require.NoError(t, w.err)
	assert.Equal(t, []string{"U1", "U2"}, w.ids)

	assert.Equal(t, int32(1), atomic.LoadInt32(&calls),
		"exactly one underlying Slack fetch across both concurrent callers")

	// After the winner finishes, the guard is released and the entry is cached:
	// a subsequent read is served from the snapshot with no further fetch.
	ids, err := ap.GetChannelMembers(context.Background(), "C1")
	require.NoError(t, err)
	assert.Equal(t, []string{"U1", "U2"}, ids)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "cached entry must not be re-fetched")
}

// TestChannelMembersIntermediatePage429Resumes drives the real MCPSlackClient
// pagination against an httptest server that returns HTTP 429 exactly once on an
// intermediate page (page 2), then succeeds. It asserts the roster completes
// with every member and that already-fetched earlier pages are not re-fetched —
// i.e. the retry resumes from the current cursor instead of restarting page 1.
func TestChannelMembersIntermediatePage429Resumes(t *testing.T) {
	pages := map[string]struct {
		members    []string
		nextCursor string
	}{
		"":     {members: []string{"U1", "U2"}, nextCursor: "cur1"},
		"cur1": {members: []string{"U3", "U4"}, nextCursor: "cur2"},
		"cur2": {members: []string{"U5"}, nextCursor: ""},
	}
	// Per-cursor request counters: page 1 (cursor "") must be fetched exactly
	// once even though page 2 (cursor "cur1") 429s on its first attempt.
	var hitsMu sync.Mutex
	hits := map[string]int{}
	var rl429 int32 // whether we've already injected the single 429 on page 2

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		assert.Equal(t, "C123", r.FormValue("channel"))
		cursor := r.FormValue("cursor")

		hitsMu.Lock()
		hits[cursor]++
		hitsMu.Unlock()

		// Inject a single 429 on the first attempt of the intermediate page.
		if cursor == "cur1" && atomic.CompareAndSwapInt32(&rl429, 0, 1) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}

		p := pages[cursor]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":                true,
			"members":           p.members,
			"response_metadata": map[string]string{"next_cursor": p.nextCursor},
		})
	}))
	defer srv.Close()

	client := &MCPSlackClient{
		slackClient: slack.New("xoxp-test", slack.OptionAPIURL(srv.URL+"/")),
		isOAuth:     true,
	}
	path := filepath.Join(t.TempDir(), "TEAMA_channel_members_cache.json")
	ap := newMembersTestProvider(client, path)

	ids, err := ap.GetChannelMembers(context.Background(), "C123")
	require.NoError(t, err, "an intermediate-page 429 must be retried, not surfaced")
	assert.ElementsMatch(t, []string{"U1", "U2", "U3", "U4", "U5"}, ids,
		"the complete roster must be assembled across the 429 retry")

	hitsMu.Lock()
	defer hitsMu.Unlock()
	assert.Equal(t, 1, hits[""], "page 1 must not be re-fetched when a later page 429s")
	assert.Equal(t, 2, hits["cur1"], "page 2 is retried once (429 then success)")
	assert.Equal(t, 1, hits["cur2"], "page 3 fetched once")
}

// TestChannelMembers429Retry verifies a single 429 is retried transparently and
// the roster completes on the following success.
func TestChannelMembers429Retry(t *testing.T) {
	mock := &mockSlackClient{membersScript: []scriptedMembers{
		{err: &slack.RateLimitedError{RetryAfter: time.Millisecond}},
		{ids: []string{"U1", "U2", "U3"}},
	}}
	ap := newMembersTestProvider(mock, filepath.Join(t.TempDir(), "m.json"))

	ids, err := ap.GetChannelMembers(context.Background(), "C1")
	require.NoError(t, err, "a 429 must be retried, not surfaced")
	assert.Equal(t, []string{"U1", "U2", "U3"}, ids)
	assert.Equal(t, 2, mock.membersCallCount(), "one rate-limited call then one success")
}

// TestChannelMembersWorkspaceIsolation verifies two providers with distinct
// (TeamID-namespaced) cache paths keep rosters for the same channel ID separate.
func TestChannelMembersWorkspaceIsolation(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "TEAMA_channel_members_cache.json")
	pathB := filepath.Join(dir, "TEAMB_channel_members_cache.json")

	mockA := &mockSlackClient{membersScript: []scriptedMembers{{ids: []string{"UA1", "UA2"}}}}
	mockB := &mockSlackClient{membersScript: []scriptedMembers{{ids: []string{"UB1"}}}}
	apA := newMembersTestProvider(mockA, pathA)
	apB := newMembersTestProvider(mockB, pathB)

	idsA, err := apA.GetChannelMembers(context.Background(), "C1")
	require.NoError(t, err)
	idsB, err := apB.GetChannelMembers(context.Background(), "C1")
	require.NoError(t, err)

	// Same channel ID, different workspaces -> distinct rosters, no crossover.
	assert.Equal(t, []string{"UA1", "UA2"}, idsA)
	assert.Equal(t, []string{"UB1"}, idsB)
	assert.Equal(t, []string{"UA1", "UA2"}, apA.membersSnapshot.Load().Channels["C1"].MemberIDs)
	assert.Equal(t, []string{"UB1"}, apB.membersSnapshot.Load().Channels["C1"].MemberIDs)

	rawA, err := os.ReadFile(pathA)
	require.NoError(t, err)
	rawB, err := os.ReadFile(pathB)
	require.NoError(t, err)
	assert.NotContains(t, string(rawA), "UB1", "workspace A file must not contain B's members")
	assert.NotContains(t, string(rawB), "UA1", "workspace B file must not contain A's members")
}
