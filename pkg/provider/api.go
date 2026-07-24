package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/korotovsky/slack-mcp-server/pkg/limiter"
	"github.com/korotovsky/slack-mcp-server/pkg/provider/edge"
	"github.com/korotovsky/slack-mcp-server/pkg/transport"
	edgeslack "github.com/rusq/slack"
	"github.com/rusq/slackdump/v3/auth"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

const usersNotReadyMsg = "users cache is not ready yet, sync process is still running... please wait"
const channelsNotReadyMsg = "channels cache is not ready yet, sync process is still running... please wait"
const channelMembersNotReadyMsg = "channel members cache is not ready yet, sync process is still running... please wait"
const defaultUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
const defaultCacheTTL = 24 * time.Hour
const defaultMinRefreshInterval = 30 * time.Second

var AllChanTypes = []string{"mpim", "im", "public_channel", "private_channel"}
var PrivateChanType = "private_channel"
var PubChanType = "public_channel"

var ErrUsersNotReady = errors.New(usersNotReadyMsg)
var ErrChannelsNotReady = errors.New(channelsNotReadyMsg)
var ErrChannelMembersNotReady = errors.New(channelMembersNotReadyMsg)
var ErrRefreshRateLimited = errors.New("refresh skipped due to rate limiting")
var ErrCountsUnavailable = errors.New("client counts unavailable, edge client is missing or the call failed")

// atomicWriteFile writes data to a file atomically using a temp file and rename.
// Uses os.CreateTemp for unpredictable temp file names (prevents symlink attacks)
// and cleans up the temp file on rename failure.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cache_*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("setting file permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming temp file: %w", err)
	}
	return nil
}

// getCacheDir returns the appropriate cache directory for slack-mcp-server
func getCacheDir() string {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		// Fallback to current directory if we can't get user cache dir
		return "."
	}

	dir := filepath.Join(cacheDir, "slack-mcp-server")
	if err := os.MkdirAll(dir, 0700); err != nil {
		// Fallback to current directory if we can't create cache dir
		return "."
	}
	return dir
}

// getCacheTTL returns the cache TTL from SLACK_MCP_CACHE_TTL env var or default (24 hours).
// Supports formats: "1h", "30m", "3600" (seconds), "0" (disable TTL, cache forever)
// Negative values are rejected and fall back to default.
func getCacheTTL() time.Duration {
	ttlStr := os.Getenv("SLACK_MCP_CACHE_TTL")
	if ttlStr == "" {
		return defaultCacheTTL
	}

	// Try parsing as duration first (e.g., "1h", "30m")
	if d, err := time.ParseDuration(ttlStr); err == nil {
		if d < 0 {
			return defaultCacheTTL // Reject negative TTL
		}
		return d
	}

	// Try parsing as seconds (e.g., "3600")
	if secs, err := strconv.ParseInt(ttlStr, 10, 64); err == nil {
		if secs < 0 {
			return defaultCacheTTL // Reject negative TTL
		}
		return time.Duration(secs) * time.Second
	}

	return defaultCacheTTL
}

// getMinRefreshInterval returns the minimum interval between forced refreshes from
// SLACK_MCP_MIN_REFRESH_INTERVAL env var or default (30s).
// Supports formats: "30s", "1m", "60" (seconds), "0" (disable rate limiting)
// Negative values are rejected and fall back to default.
func getMinRefreshInterval() time.Duration {
	intervalStr := os.Getenv("SLACK_MCP_MIN_REFRESH_INTERVAL")
	if intervalStr == "" {
		return defaultMinRefreshInterval
	}

	// Try parsing as duration first (e.g., "30s", "1m")
	if d, err := time.ParseDuration(intervalStr); err == nil {
		if d < 0 {
			return defaultMinRefreshInterval // Reject negative interval
		}
		return d
	}

	// Try parsing as seconds (e.g., "60")
	if secs, err := strconv.ParseInt(intervalStr, 10, 64); err == nil {
		if secs < 0 {
			return defaultMinRefreshInterval // Reject negative interval
		}
		return time.Duration(secs) * time.Second
	}

	return defaultMinRefreshInterval
}

// validateAuthAndGetTeamID performs auth validation on startup and returns the TeamID.
// This ensures tokens are valid before proceeding and enables cache namespacing
// to prevent cache contamination when using multiple Slack workspaces.
// Returns an error if authentication fails - the server should not start with invalid credentials.
func validateAuthAndGetTeamID(authProvider auth.Provider, logger *zap.Logger) (string, error) {
	xoxpToken := os.Getenv("SLACK_MCP_XOXP_TOKEN")
	xoxcToken := os.Getenv("SLACK_MCP_XOXC_TOKEN")
	xoxdToken := os.Getenv("SLACK_MCP_XOXD_TOKEN")
	if xoxpToken == "demo" || (xoxcToken == "demo" && xoxdToken == "demo") {
		return "demo", nil
	}

	httpClient := transport.ProvideHTTPClient(authProvider.Cookies(), logger)
	slackOpts := []slack.Option{slack.OptionHTTPClient(httpClient)}
	if os.Getenv("SLACK_MCP_GOVSLACK") == "true" {
		slackOpts = append(slackOpts, slack.OptionAPIURL("https://slack-gov.com/api/"))
	}
	slackClient := slack.New(authProvider.SlackToken(), slackOpts...)

	authResp, err := slackClient.AuthTest()
	if err != nil {
		return "", err
	}

	logger.Info("Authenticated to Slack",
		zap.String("team", authResp.Team),
		zap.String("team_id", authResp.TeamID),
		zap.String("user", authResp.User))

	return authResp.TeamID, nil
}

// getCachePathWithTeamID returns a cache file path prefixed with TeamID for workspace isolation.
// If TeamID is empty, returns the default filename without prefix.
func getCachePathWithTeamID(teamID, filename string) string {
	cacheDir := getCacheDir()
	if teamID != "" {
		return filepath.Join(cacheDir, teamID+"_"+filename)
	}
	return filepath.Join(cacheDir, filename)
}

type UsersCache struct {
	Users    map[string]slack.User `json:"users"`
	UsersInv map[string]string     `json:"users_inv"`
}

type ChannelsCache struct {
	Channels    map[string]Channel `json:"channels"`
	ChannelsInv map[string]string  `json:"channels_inv"`
}

type ChannelMemberRoster struct {
	MemberIDs []string  `json:"member_ids"`
	FetchedAt time.Time `json:"fetched_at"`
}

type ChannelMembersCache struct {
	Channels map[string]ChannelMemberRoster `json:"channels"` // keyed by channel ID
}

type Channel struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Topic        string   `json:"topic"`
	Purpose      string   `json:"purpose"`
	MemberCount  int      `json:"memberCount"`
	IsMpIM       bool     `json:"mpim"`
	IsIM         bool     `json:"im"`
	IsPrivate    bool     `json:"private"`
	IsMember     bool     `json:"is_member"`
	IsExtShared  bool     `json:"is_ext_shared"`     // Shared with external organizations
	User         string   `json:"user,omitempty"`    // User ID for IM channels
	Members      []string `json:"members,omitempty"` // Member IDs for the channel
	HasUnreads   bool     `json:"has_unreads"`
	LastRead     string   `json:"last_read,omitempty"`
	MentionCount int      `json:"mention_count"`
}

type SlackAPI interface {
	// Standard slack-go API methods
	AuthTest() (*slack.AuthTestResponse, error)
	AuthTestContext(ctx context.Context) (*slack.AuthTestResponse, error)
	GetUsersContext(ctx context.Context, options ...slack.GetUsersOption) ([]slack.User, error)
	GetUsersInfo(users ...string) (*[]slack.User, error)
	PostMessageContext(ctx context.Context, channel string, options ...slack.MsgOption) (string, string, error)
	MarkConversationContext(ctx context.Context, channel, ts string) error
	AddReactionContext(ctx context.Context, name string, item slack.ItemRef) error
	RemoveReactionContext(ctx context.Context, name string, item slack.ItemRef) error
	LeaveConversationContext(ctx context.Context, channelID string) (bool, error)
	JoinConversationContext(ctx context.Context, channelID string) (*slack.Channel, string, []string, error)

	// Used to get messages
	GetConversationHistoryContext(ctx context.Context, params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error)
	GetConversationRepliesContext(ctx context.Context, params *slack.GetConversationRepliesParameters) (msgs []slack.Message, hasMore bool, nextCursor string, err error)
	SearchContext(ctx context.Context, query string, params slack.SearchParameters) (*slack.SearchMessages, *slack.SearchFiles, error)

	// Used to get files
	GetFileInfoContext(ctx context.Context, fileID string, count, page int) (*slack.File, []slack.Comment, *slack.Paging, error)
	GetFileContext(ctx context.Context, downloadURL string, writer io.Writer) error

	// Used to get channel info (for unread counts with xoxp tokens)
	GetConversationInfoContext(ctx context.Context, input *slack.GetConversationInfoInput) (*slack.Channel, error)

	// Used to get channels list from both Slack and Enterprise Grid versions
	GetConversationsContext(ctx context.Context, params *slack.GetConversationsParameters) ([]slack.Channel, string, error)

	// Used to list only channels the calling user is a member of (users.conversations).
	// For xoxp tokens this is more efficient than conversations.list because it excludes
	// non-member public channels and closed DMs that cannot have unreads.
	GetConversationsForUserContext(ctx context.Context, params *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error)

	// Used to get the complete member roster of a channel (conversations.members).
	GetUsersInConversationContext(ctx context.Context, params *slack.GetUsersInConversationParameters) ([]string, string, error)

	// Edge API methods
	ClientUserBoot(ctx context.Context) (*edge.ClientUserBootResponse, error)
	UsersSearch(ctx context.Context, query string, count int) ([]slack.User, error)
	DraftsCreate(ctx context.Context, channelID, threadTs string, blocks json.RawMessage) (string, error)
	DraftsList(ctx context.Context, limit int) ([]edge.Draft, error)
	DraftsUpdate(ctx context.Context, draftID, clientMsgID, lastUpdatedTS, channelID, threadTs string, blocks json.RawMessage) error
	ClientCounts(ctx context.Context) (edge.ClientCountsResponse, error)
	GetMutedChannels(ctx context.Context) (map[string]bool, error)
	SavedList(ctx context.Context, filter string, limit int, cursor string) (edge.SavedListResponse, error)
	SavedUpdate(ctx context.Context, itemType, itemID, ts, mark string, dateDue int64) error
	SavedClearCompleted(ctx context.Context) error

	// User groups API methods
	GetUserGroupsContext(ctx context.Context, options ...slack.GetUserGroupsOption) ([]slack.UserGroup, error)
	GetUserGroupMembersContext(ctx context.Context, userGroup string, options ...slack.GetUserGroupMembersOption) ([]string, error)
	CreateUserGroupContext(ctx context.Context, userGroup slack.UserGroup, options ...slack.CreateUserGroupOption) (slack.UserGroup, error)
	UpdateUserGroupContext(ctx context.Context, userGroupID string, options ...slack.UpdateUserGroupsOption) (slack.UserGroup, error)
	UpdateUserGroupMembersContext(ctx context.Context, userGroup string, members string, options ...slack.UpdateUserGroupMembersOption) (slack.UserGroup, error)
}

type MCPSlackClient struct {
	slackClient *slack.Client
	edgeClient  *edge.Client

	authResponse *slack.AuthTestResponse
	authProvider auth.Provider

	isEnterprise bool
	isOAuth      bool
	isBotToken   bool
	edgeFailed   bool // set when edge API fails; subsequent calls skip straight to standard API
	teamEndpoint string
}

type ApiProvider struct {
	transport string
	client    SlackAPI
	logger    *zap.Logger

	rateLimiter        *rate.Limiter
	cacheTTL           time.Duration
	minRefreshInterval time.Duration

	// Users cache: atomic pointer to immutable snapshot (no copy on read)
	usersSnapshot          atomic.Pointer[UsersCache]
	usersCachePath         string
	usersReady             atomic.Bool
	refreshingUsers        atomic.Bool // true while a background refresh goroutine is running
	lastForcedUsersRefresh time.Time
	usersMu                sync.RWMutex // protects lastForcedUsersRefresh
	fetchUsersMu           sync.Mutex   // serializes fetchAndStoreUsers calls

	// Channels cache: atomic pointer to immutable snapshot (no copy on read)
	channelsSnapshot          atomic.Pointer[ChannelsCache]
	channelsCachePath         string
	channelsReady             atomic.Bool
	refreshingChannels        atomic.Bool // true while a background refresh goroutine is running
	lastForcedChannelsRefresh time.Time
	channelsMu                sync.RWMutex // protects lastForcedChannelsRefresh
	fetchChannelsMu           sync.Mutex   // serializes fetchAndStoreChannels calls

	// Channel members cache: atomic pointer to immutable snapshot (no copy on read).
	// Rosters are per-channel, so the in-flight guard and forced-refresh throttle
	// are keyed by channel ID rather than a single atomic.Bool / time.Time.
	membersSnapshot          atomic.Pointer[ChannelMembersCache]
	membersCachePath         string
	refreshingMembers        sync.Map             // channelID -> struct{}: per-channel in-flight gather guard
	lastForcedMembersRefresh map[string]time.Time // per-channel forced-refresh throttle
	membersMu                sync.RWMutex         // protects lastForcedMembersRefresh + snapshot swaps
	fetchMembersMu           sync.Mutex           // serializes disk writes
}

func NewMCPSlackClient(authProvider auth.Provider, logger *zap.Logger) (*MCPSlackClient, error) {
	httpClient := transport.ProvideHTTPClient(authProvider.Cookies(), logger)

	slackOpts := []slack.Option{slack.OptionHTTPClient(httpClient)}
	if os.Getenv("SLACK_MCP_GOVSLACK") == "true" {
		slackOpts = append(slackOpts, slack.OptionAPIURL("https://slack-gov.com/api/"))
	}
	slackClient := slack.New(authProvider.SlackToken(), slackOpts...)

	authResp, err := slackClient.AuthTest()
	if err != nil {
		return nil, err
	}

	authResponse := &slack.AuthTestResponse{
		URL:          authResp.URL,
		Team:         authResp.Team,
		User:         authResp.User,
		TeamID:       authResp.TeamID,
		UserID:       authResp.UserID,
		EnterpriseID: authResp.EnterpriseID,
		BotID:        authResp.BotID,
	}

	slackClient = slack.New(authProvider.SlackToken(),
		slack.OptionHTTPClient(httpClient),
		slack.OptionAPIURL(authResp.URL+"api/"),
	)

	edgeClient, err := edge.NewWithInfo(authResponse, authProvider,
		edge.OptionHTTPClient(httpClient),
	)
	if err != nil {
		return nil, err
	}

	isEnterprise := authResp.EnterpriseID != ""
	token := authProvider.SlackToken()

	// Token type detection
	// isOAuth: Official OAuth tokens (xoxp or xoxb) - uses Standard API
	// isBotToken: Bot token - determines feature availability (e.g., search)
	// xoxe.xoxp- and xoxe.xoxb- are token-rotation variants of xoxp/xoxb (same scopes, 12h expiry)
	isOAuth := strings.HasPrefix(token, "xoxp-") || strings.HasPrefix(token, "xoxb-") || strings.HasPrefix(token, "xoxe.xoxp-") || strings.HasPrefix(token, "xoxe.xoxb-")
	isBotToken := strings.HasPrefix(token, "xoxb-") || strings.HasPrefix(token, "xoxe.xoxb-")

	return &MCPSlackClient{
		slackClient:  slackClient,
		edgeClient:   edgeClient,
		authResponse: authResponse,
		authProvider: authProvider,
		isEnterprise: isEnterprise,
		isOAuth:      isOAuth,
		isBotToken:   isBotToken,
		teamEndpoint: authResp.URL,
	}, nil
}

func (c *MCPSlackClient) AuthTest() (*slack.AuthTestResponse, error) {
	if os.Getenv("SLACK_MCP_XOXP_TOKEN") == "demo" || (os.Getenv("SLACK_MCP_XOXC_TOKEN") == "demo" && os.Getenv("SLACK_MCP_XOXD_TOKEN") == "demo") {
		return &slack.AuthTestResponse{
			URL:          "https://_.slack.com",
			Team:         "Demo Team",
			User:         "Username",
			TeamID:       "TEAM123456",
			UserID:       "U1234567890",
			EnterpriseID: "",
			BotID:        "",
		}, nil
	}

	if c.authResponse != nil {
		return c.authResponse, nil
	}

	return c.slackClient.AuthTest()
}

func (c *MCPSlackClient) AuthTestContext(ctx context.Context) (*slack.AuthTestResponse, error) {
	return c.slackClient.AuthTestContext(ctx)
}

func (c *MCPSlackClient) GetUsersContext(ctx context.Context, options ...slack.GetUsersOption) ([]slack.User, error) {
	return c.slackClient.GetUsersContext(ctx, options...)
}

func (c *MCPSlackClient) GetUsersInfo(users ...string) (*[]slack.User, error) {
	return c.slackClient.GetUsersInfo(users...)
}

func (c *MCPSlackClient) MarkConversationContext(ctx context.Context, channel, ts string) error {
	return c.slackClient.MarkConversationContext(ctx, channel, ts)
}

func (c *MCPSlackClient) LeaveConversationContext(ctx context.Context, channelID string) (bool, error) {
	if c.isEnterprise && !c.isOAuth {
		// Enterprise Grid + session tokens: use edge API which goes through
		// the webclient endpoint and bypasses enterprise_is_restricted.
		notInChannel, err := c.edgeClient.LeaveConversation(ctx, channelID)
		if err == nil {
			return notInChannel, nil
		}
		// Fall back to standard API if edge fails.
	}
	return c.slackClient.LeaveConversationContext(ctx, channelID)
}

func (c *MCPSlackClient) JoinConversationContext(ctx context.Context, channelID string) (*slack.Channel, string, []string, error) {
	return c.slackClient.JoinConversationContext(ctx, channelID)
}

func (c *MCPSlackClient) GetConversationsContext(ctx context.Context, params *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	// Please see https://github.com/korotovsky/slack-mcp-server/issues/73
	// It seems that `conversations.list` works with `xoxp` tokens within Enterprise Grid setups
	// and if `xoxc`/`xoxd` defined we fallback to edge client.
	// In non Enterprise Grid setups we always use `conversations.list` api as it accepts both token types wtf.
	if c.isEnterprise {
		if c.isOAuth {
			return c.slackClient.GetConversationsContext(ctx, params)
		}

		// Enterprise + non-OAuth: try edge API first (for DMs, MPIMs, etc.),
		// then supplement with standard API. The edge API may only return
		// partial results (e.g., DMs succeed but SearchChannels fails on
		// restricted teams), so we always merge both sources.
		//
		// The edge API returns all results in one shot (no pagination),
		// while the standard API paginates. We fully paginate the standard
		// API here and return a merged, deduplicated result set with an
		// empty cursor so the caller doesn't need to re-paginate.
		if !c.edgeFailed {
			edgeChannels, _, edgeErr := c.edgeClient.GetConversationsContext(ctx, nil)
			if edgeErr != nil {
				c.edgeFailed = true
				return c.slackClient.GetConversationsContext(ctx, params)
			}

			// Collect edge results into a map for deduplication.
			seen := make(map[string]struct{}, len(edgeChannels))
			var channels []slack.Channel
			for _, ec := range edgeChannels {
				if params != nil && params.ExcludeArchived && ec.IsArchived {
					continue
				}
				seen[ec.ID] = struct{}{}
				channels = append(channels, slack.Channel{
					IsGeneral: ec.IsGeneral,
					IsMember:  ec.IsMember,
					GroupConversation: slack.GroupConversation{
						Conversation: slack.Conversation{
							ID:                 ec.ID,
							IsIM:               ec.IsIM,
							IsMpIM:             ec.IsMpIM,
							IsPrivate:          ec.IsPrivate,
							Created:            slack.JSONTime(ec.Created.Time().UnixMilli()),
							Unlinked:           ec.Unlinked,
							NameNormalized:     ec.NameNormalized,
							IsShared:           ec.IsShared,
							IsExtShared:        ec.IsExtShared,
							IsOrgShared:        ec.IsOrgShared,
							IsPendingExtShared: ec.IsPendingExtShared,
							NumMembers:         ec.NumMembers,
						},
						Name:       ec.Name,
						IsArchived: ec.IsArchived,
						Members:    ec.Members,
						Topic: slack.Topic{
							Value: ec.Topic.Value,
						},
						Purpose: slack.Purpose{
							Value: ec.Purpose.Value,
						},
					},
				})
			}

			// Supplement with ALL pages from the standard API to fill gaps
			// the edge API missed (e.g., public/private channels on
			// restricted teams where SearchChannels returns an error).
			stdParams := &slack.GetConversationsParameters{
				Limit:           999,
				ExcludeArchived: true,
			}
			if params != nil {
				stdParams.Types = params.Types
			}
			for {
				stdChannels, nextCur, stdErr := c.slackClient.GetConversationsContext(ctx, stdParams)
				if stdErr != nil {
					break // standard API failed; keep what edge gave us
				}
				for _, sc := range stdChannels {
					if _, ok := seen[sc.ID]; !ok {
						seen[sc.ID] = struct{}{}
						channels = append(channels, sc)
					}
				}
				if nextCur == "" {
					break
				}
				stdParams.Cursor = nextCur
			}

			return channels, "", nil
		}

		// Edge API previously failed — use standard API directly.
		return c.slackClient.GetConversationsContext(ctx, params)
	}

	return c.slackClient.GetConversationsContext(ctx, params)
}

func (c *MCPSlackClient) GetConversationsForUserContext(ctx context.Context, params *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error) {
	return c.slackClient.GetConversationsForUserContext(ctx, params)
}

// GetUsersInConversationContext returns a single page of a channel's member
// roster plus the next-page cursor. It follows the same edge-first-then-standard
// dispatch as GetConversationsContext: for Enterprise Grid + session tokens it
// uses the edge client (which returns the full member list in one shot via
// UsersList, hence an empty next cursor), falling back to the standard client on
// error. The standard-client path returns exactly one conversations.members page
// and its cursor. Pagination is driven by the caller (fetchAndStoreChannelMembers)
// so each page can be individually rate-limited and retried, resuming from the
// current cursor on a 429 rather than restarting the whole gather.
func (c *MCPSlackClient) GetUsersInConversationContext(ctx context.Context, params *slack.GetUsersInConversationParameters) ([]string, string, error) {
	if c.isEnterprise && !c.isOAuth && !c.edgeFailed {
		// Enterprise Grid + session tokens: use edge API, which returns the full
		// member list in one shot (no pagination needed). The edge client uses
		// the rusq/slack fork's param type; only ChannelID is consumed.
		ids, _, err := c.edgeClient.GetUsersInConversationContext(ctx, &edgeslack.GetUsersInConversationParameters{
			ChannelID: params.ChannelID,
		})
		if err == nil {
			return ids, "", nil
		}
		c.edgeFailed = true
		// Fall back to standard API below.
	}

	// Standard client: return a single conversations.members page and its
	// next-cursor. The caller loops on the cursor, so no internal pagination here.
	return c.slackClient.GetUsersInConversationContext(ctx, params)
}

func (c *MCPSlackClient) GetConversationHistoryContext(ctx context.Context, params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	return c.slackClient.GetConversationHistoryContext(ctx, params)
}

func (c *MCPSlackClient) GetConversationRepliesContext(ctx context.Context, params *slack.GetConversationRepliesParameters) (msgs []slack.Message, hasMore bool, nextCursor string, err error) {
	return c.slackClient.GetConversationRepliesContext(ctx, params)
}

func (c *MCPSlackClient) SearchContext(ctx context.Context, query string, params slack.SearchParameters) (*slack.SearchMessages, *slack.SearchFiles, error) {
	return c.slackClient.SearchContext(ctx, query, params)
}

func (c *MCPSlackClient) PostMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error) {
	return c.slackClient.PostMessageContext(ctx, channelID, options...)
}

func (c *MCPSlackClient) AddReactionContext(ctx context.Context, name string, item slack.ItemRef) error {
	return c.slackClient.AddReactionContext(ctx, name, item)
}

func (c *MCPSlackClient) RemoveReactionContext(ctx context.Context, name string, item slack.ItemRef) error {
	return c.slackClient.RemoveReactionContext(ctx, name, item)
}

func (c *MCPSlackClient) GetFileInfoContext(ctx context.Context, fileID string, count, page int) (*slack.File, []slack.Comment, *slack.Paging, error) {
	return c.slackClient.GetFileInfoContext(ctx, fileID, count, page)
}

func (c *MCPSlackClient) GetFileContext(ctx context.Context, downloadURL string, writer io.Writer) error {
	return c.slackClient.GetFileContext(ctx, downloadURL, writer)
}

func (c *MCPSlackClient) GetConversationInfoContext(ctx context.Context, input *slack.GetConversationInfoInput) (*slack.Channel, error) {
	return c.slackClient.GetConversationInfoContext(ctx, input)
}

func (c *MCPSlackClient) ClientUserBoot(ctx context.Context) (*edge.ClientUserBootResponse, error) {
	return c.edgeClient.ClientUserBoot(ctx)
}

func (c *MCPSlackClient) UsersSearch(ctx context.Context, query string, count int) ([]slack.User, error) {
	return c.edgeClient.UsersSearch(ctx, query, count)
}

func (c *MCPSlackClient) DraftsCreate(ctx context.Context, channelID, threadTs string, blocks json.RawMessage) (string, error) {
	return c.edgeClient.DraftsCreate(ctx, channelID, threadTs, blocks)
}

func (c *MCPSlackClient) DraftsList(ctx context.Context, limit int) ([]edge.Draft, error) {
	return c.edgeClient.DraftsList(ctx, limit)
}

func (c *MCPSlackClient) DraftsUpdate(ctx context.Context, draftID, clientMsgID, lastUpdatedTS, channelID, threadTs string, blocks json.RawMessage) error {
	return c.edgeClient.DraftsUpdate(ctx, draftID, clientMsgID, lastUpdatedTS, channelID, threadTs, blocks)
}

func (c *MCPSlackClient) ClientCounts(ctx context.Context) (edge.ClientCountsResponse, error) {
	return c.edgeClient.ClientCounts(ctx)
}

func (c *MCPSlackClient) GetMutedChannels(ctx context.Context) (map[string]bool, error) {
	return c.edgeClient.GetMutedChannels(ctx)
}

func (c *MCPSlackClient) SavedList(ctx context.Context, filter string, limit int, cursor string) (edge.SavedListResponse, error) {
	return c.edgeClient.SavedList(ctx, filter, limit, cursor)
}

func (c *MCPSlackClient) SavedUpdate(ctx context.Context, itemType, itemID, ts, mark string, dateDue int64) error {
	return c.edgeClient.SavedUpdate(ctx, itemType, itemID, ts, mark, dateDue)
}

func (c *MCPSlackClient) SavedClearCompleted(ctx context.Context) error {
	return c.edgeClient.SavedClearCompleted(ctx)
}

func (c *MCPSlackClient) GetUserGroupsContext(ctx context.Context, options ...slack.GetUserGroupsOption) ([]slack.UserGroup, error) {
	return c.slackClient.GetUserGroupsContext(ctx, options...)
}

func (c *MCPSlackClient) GetUserGroupMembersContext(ctx context.Context, userGroup string, options ...slack.GetUserGroupMembersOption) ([]string, error) {
	return c.slackClient.GetUserGroupMembersContext(ctx, userGroup, options...)
}

func (c *MCPSlackClient) CreateUserGroupContext(ctx context.Context, userGroup slack.UserGroup, options ...slack.CreateUserGroupOption) (slack.UserGroup, error) {
	return c.slackClient.CreateUserGroupContext(ctx, userGroup, options...)
}

func (c *MCPSlackClient) UpdateUserGroupContext(ctx context.Context, userGroupID string, options ...slack.UpdateUserGroupsOption) (slack.UserGroup, error) {
	return c.slackClient.UpdateUserGroupContext(ctx, userGroupID, options...)
}

func (c *MCPSlackClient) UpdateUserGroupMembersContext(ctx context.Context, userGroup string, members string, options ...slack.UpdateUserGroupMembersOption) (slack.UserGroup, error) {
	return c.slackClient.UpdateUserGroupMembersContext(ctx, userGroup, members, options...)
}

func (c *MCPSlackClient) IsEnterprise() bool {
	return c.isEnterprise
}

func (c *MCPSlackClient) AuthResponse() *slack.AuthTestResponse {
	return c.authResponse
}

func (c *MCPSlackClient) IsBotToken() bool {
	return c.isBotToken
}

func (c *MCPSlackClient) IsOAuth() bool {
	return c.isOAuth
}

func (c *MCPSlackClient) Raw() struct {
	Slack *slack.Client
	Edge  *edge.Client
} {
	return struct {
		Slack *slack.Client
		Edge  *edge.Client
	}{
		Slack: c.slackClient,
		Edge:  c.edgeClient,
	}
}

func New(transport string, logger *zap.Logger) *ApiProvider {
	var (
		authProvider auth.ValueAuth
		err          error
	)

	// Read all environment variables
	xoxpToken := os.Getenv("SLACK_MCP_XOXP_TOKEN")
	xoxbToken := os.Getenv("SLACK_MCP_XOXB_TOKEN")
	xoxcToken := os.Getenv("SLACK_MCP_XOXC_TOKEN")
	xoxdToken := os.Getenv("SLACK_MCP_XOXD_TOKEN")

	// Warn if both user and bot tokens are set
	if xoxpToken != "" && xoxbToken != "" {
		logger.Warn(
			"Both SLACK_MCP_XOXP_TOKEN and SLACK_MCP_XOXB_TOKEN are set. "+
				"Using User token (xoxp) for full features. "+
				"Bot token will be ignored.",
			zap.String("context", "console"),
		)
	}

	// Priority 1: XOXP token (User OAuth)
	if xoxpToken != "" {
		authProvider, err = auth.NewValueAuth(xoxpToken, "")
		if err != nil {
			logger.Fatal("Failed to create auth provider with XOXP token", zap.Error(err))
		}

		return newWithXOXP(transport, authProvider, logger)
	}

	// Priority 2: XOXB token (Bot)
	if xoxbToken != "" {
		authProvider, err = auth.NewValueAuth(xoxbToken, "")
		if err != nil {
			logger.Fatal("Failed to create auth provider with XOXB token", zap.Error(err))
		}

		logger.Info("Using Bot token authentication",
			zap.String("context", "console"),
			zap.String("token_type", "xoxb"),
		)

		return newWithXOXB(transport, authProvider, logger)
	}

	// Priority 3: XOXC/XOXD tokens (session-based)
	if xoxcToken == "" || xoxdToken == "" {
		logger.Fatal("Authentication required: Either SLACK_MCP_XOXP_TOKEN, SLACK_MCP_XOXB_TOKEN, or both SLACK_MCP_XOXC_TOKEN and SLACK_MCP_XOXD_TOKEN must be provided")
	}

	authProvider, err = auth.NewValueAuth(xoxcToken, xoxdToken)
	if err != nil {
		logger.Fatal("Failed to create auth provider with XOXC/XOXD tokens", zap.Error(err))
	}

	return newWithXOXC(transport, authProvider, logger)
}

func newWithXOXP(transport string, authProvider auth.ValueAuth, logger *zap.Logger) *ApiProvider {
	var (
		client *MCPSlackClient
		err    error
	)

	teamID, err := validateAuthAndGetTeamID(authProvider, logger)
	if err != nil {
		logger.Fatal("Authentication failed - check your Slack tokens", zap.Error(err))
	}

	usersCache := os.Getenv("SLACK_MCP_USERS_CACHE")
	if usersCache == "" {
		usersCache = getCachePathWithTeamID(teamID, "users_cache.json")
	}

	channelsCache := os.Getenv("SLACK_MCP_CHANNELS_CACHE")
	if channelsCache == "" {
		channelsCache = getCachePathWithTeamID(teamID, "channels_cache_v2.json")
	}

	channelMembersCache := os.Getenv("SLACK_MCP_CHANNEL_MEMBERS_CACHE")
	if channelMembersCache == "" {
		channelMembersCache = getCachePathWithTeamID(teamID, "channel_members_cache.json")
	}

	if os.Getenv("SLACK_MCP_XOXP_TOKEN") == "demo" || (os.Getenv("SLACK_MCP_XOXC_TOKEN") == "demo" && os.Getenv("SLACK_MCP_XOXD_TOKEN") == "demo") {
		logger.Info("Demo credentials are set, skip.")
	} else {
		client, err = NewMCPSlackClient(authProvider, logger)
		if err != nil {
			logger.Fatal("Failed to create MCP Slack client", zap.Error(err))
		}
	}

	ap := &ApiProvider{
		transport: transport,
		client:    client,
		logger:    logger,

		rateLimiter:        limiter.Tier2.Limiter(),
		cacheTTL:           getCacheTTL(),
		minRefreshInterval: getMinRefreshInterval(),

		usersCachePath:    usersCache,
		channelsCachePath: channelsCache,
		membersCachePath:  channelMembersCache,

		lastForcedMembersRefresh: make(map[string]time.Time),
	}
	// Initialize with empty snapshots
	ap.usersSnapshot.Store(&UsersCache{
		Users:    make(map[string]slack.User),
		UsersInv: make(map[string]string),
	})
	ap.channelsSnapshot.Store(&ChannelsCache{
		Channels:    make(map[string]Channel),
		ChannelsInv: make(map[string]string),
	})
	ap.membersSnapshot.Store(&ChannelMembersCache{
		Channels: map[string]ChannelMemberRoster{},
	})
	return ap
}

func newWithXOXB(transport string, authProvider auth.ValueAuth, logger *zap.Logger) *ApiProvider {
	// Bot tokens do not support demo mode, but otherwise share the same
	// initialization logic as user OAuth tokens.
	return newWithXOXP(transport, authProvider, logger)
}

func newWithXOXC(transport string, authProvider auth.ValueAuth, logger *zap.Logger) *ApiProvider {
	var (
		client *MCPSlackClient
		err    error
	)

	teamID, err := validateAuthAndGetTeamID(authProvider, logger)
	if err != nil {
		logger.Fatal("Authentication failed - check your Slack tokens", zap.Error(err))
	}

	usersCache := os.Getenv("SLACK_MCP_USERS_CACHE")
	if usersCache == "" {
		usersCache = getCachePathWithTeamID(teamID, "users_cache.json")
	}

	channelsCache := os.Getenv("SLACK_MCP_CHANNELS_CACHE")
	if channelsCache == "" {
		channelsCache = getCachePathWithTeamID(teamID, "channels_cache_v2.json")
	}

	channelMembersCache := os.Getenv("SLACK_MCP_CHANNEL_MEMBERS_CACHE")
	if channelMembersCache == "" {
		channelMembersCache = getCachePathWithTeamID(teamID, "channel_members_cache.json")
	}

	if os.Getenv("SLACK_MCP_XOXP_TOKEN") == "demo" || (os.Getenv("SLACK_MCP_XOXC_TOKEN") == "demo" && os.Getenv("SLACK_MCP_XOXD_TOKEN") == "demo") {
		logger.Info("Demo credentials are set, skip.")
	} else {
		client, err = NewMCPSlackClient(authProvider, logger)
		if err != nil {
			logger.Fatal("Failed to create MCP Slack client", zap.Error(err))
		}
	}

	ap := &ApiProvider{
		transport: transport,
		client:    client,
		logger:    logger,

		rateLimiter:        limiter.Tier2.Limiter(),
		cacheTTL:           getCacheTTL(),
		minRefreshInterval: getMinRefreshInterval(),

		usersCachePath:    usersCache,
		channelsCachePath: channelsCache,
		membersCachePath:  channelMembersCache,

		lastForcedMembersRefresh: make(map[string]time.Time),
	}
	// Initialize with empty snapshots
	ap.usersSnapshot.Store(&UsersCache{
		Users:    make(map[string]slack.User),
		UsersInv: make(map[string]string),
	})
	ap.channelsSnapshot.Store(&ChannelsCache{
		Channels:    make(map[string]Channel),
		ChannelsInv: make(map[string]string),
	})
	ap.membersSnapshot.Store(&ChannelMembersCache{
		Channels: map[string]ChannelMemberRoster{},
	})
	return ap
}

func (ap *ApiProvider) RefreshUsers(ctx context.Context) error {
	return ap.refreshUsersInternal(ctx, false)
}

// ForceRefreshUsers bypasses the cache and fetches fresh user data from Slack API.
// Rate limited by SLACK_MCP_MIN_REFRESH_INTERVAL (default 30s) to prevent API abuse.
// Returns ErrRefreshRateLimited if refresh is skipped due to rate limiting.
func (ap *ApiProvider) ForceRefreshUsers(ctx context.Context) error {
	if ap.minRefreshInterval > 0 {
		// Use single lock scope for check-and-update to prevent TOCTOU race
		ap.usersMu.Lock()
		sinceLast := time.Since(ap.lastForcedUsersRefresh)
		if sinceLast < ap.minRefreshInterval {
			ap.usersMu.Unlock()
			ap.logger.Debug("Skipping forced users refresh, within rate limit",
				zap.Duration("since_last", sinceLast),
				zap.Duration("min_interval", ap.minRefreshInterval))
			return ErrRefreshRateLimited
		}
		// Update timestamp before refresh to prevent concurrent forced refreshes
		ap.lastForcedUsersRefresh = time.Now()
		ap.usersMu.Unlock()
	}

	ap.logger.Info("Force refreshing users cache")
	return ap.refreshUsersInternal(ctx, true)
}

// PatchUser fetches a single user by ID from the Slack API and adds them to
// the in-memory users snapshot. This is much cheaper than a full cache rebuild
// for a single cache miss (O(1) API call vs O(all users)).
// Disk persistence is skipped — the next full refresh will persist the entry.
func (ap *ApiProvider) PatchUser(ctx context.Context, userID string) (*slack.User, error) {
	usersInfo, err := ap.client.GetUsersInfo(userID)
	if err != nil {
		ap.logger.Warn("Failed to fetch user for cache patch", zap.String("user_id", userID), zap.Error(err))
		return nil, err
	}
	if usersInfo == nil || len(*usersInfo) == 0 {
		ap.logger.Debug("User not found via API", zap.String("user_id", userID))
		return nil, errors.New("user not found")
	}

	user := (*usersInfo)[0]
	current := ap.usersSnapshot.Load()

	newSnapshot := &UsersCache{
		Users:    make(map[string]slack.User, len(current.Users)+1),
		UsersInv: make(map[string]string, len(current.UsersInv)+1),
	}
	for k, v := range current.Users {
		newSnapshot.Users[k] = v
	}
	for k, v := range current.UsersInv {
		newSnapshot.UsersInv[k] = v
	}
	newSnapshot.Users[user.ID] = user
	newSnapshot.UsersInv[user.Name] = user.ID

	ap.usersSnapshot.Store(newSnapshot)
	ap.logger.Debug("Patched user into cache",
		zap.String("user_id", user.ID),
		zap.String("user_name", user.Name))

	return &user, nil
}

func (ap *ApiProvider) refreshUsersInternal(ctx context.Context, force bool) error {
	ap.usersMu.Lock()

	// Check if we should use cache (not forced, cache exists)
	if !force {
		if data, err := os.ReadFile(ap.usersCachePath); err == nil {
			var cachedUsers []slack.User
			if err := json.Unmarshal(data, &cachedUsers); err != nil {
				ap.logger.Warn("Failed to unmarshal users cache, will refetch",
					zap.String("cache_file", ap.usersCachePath),
					zap.Error(err))
			} else if len(cachedUsers) == 0 {
				ap.logger.Warn("Users cache is empty or null, will refetch",
					zap.String("cache_file", ap.usersCachePath))
			} else {
				// Build snapshot from cache
				newSnapshot := &UsersCache{
					Users:    make(map[string]slack.User, len(cachedUsers)),
					UsersInv: make(map[string]string, len(cachedUsers)),
				}
				for _, u := range cachedUsers {
					newSnapshot.Users[u.ID] = u
					newSnapshot.UsersInv[u.Name] = u.ID
				}
				ap.usersSnapshot.Store(newSnapshot)
				ap.usersReady.Store(true)

				// Check cache TTL using file modification time
				cacheExpired := false
				if ap.cacheTTL > 0 {
					if fileInfo, err := os.Stat(ap.usersCachePath); err == nil {
						cacheAge := time.Since(fileInfo.ModTime())
						if cacheAge > ap.cacheTTL {
							cacheExpired = true
							ap.logger.Info("Serving stale users cache, background refresh starting",
								zap.Duration("cache_age", cacheAge),
								zap.Duration("ttl", ap.cacheTTL),
								zap.Int("count", len(cachedUsers)),
								zap.String("cache_file", ap.usersCachePath))
						}
					}
				}

				if !cacheExpired {
					ap.logger.Info("Loaded users from cache",
						zap.Int("count", len(cachedUsers)),
						zap.String("cache_file", ap.usersCachePath))
					ap.usersMu.Unlock()
					return nil
				}

				// Cache is expired: release lock, spawn background refresh, return immediately
				ap.usersMu.Unlock()
				ap.spawnBackgroundUsersRefresh()
				return nil
			}
		}
	}

	// No usable cache: fetch fresh data synchronously (first run or force)
	ap.usersMu.Unlock()
	return ap.fetchAndStoreUsers(ctx)
}

// spawnBackgroundUsersRefresh starts a background goroutine to fetch fresh user data.
// Uses refreshingUsers flag to prevent concurrent background refreshes.
func (ap *ApiProvider) spawnBackgroundUsersRefresh() {
	if !ap.refreshingUsers.CompareAndSwap(false, true) {
		ap.logger.Debug("Skipping background users refresh, already in progress")
		return
	}
	go func() {
		defer ap.refreshingUsers.Store(false)
		if err := ap.fetchAndStoreUsers(context.Background()); err != nil {
			ap.logger.Warn("Background users refresh failed, continuing with stale data",
				zap.Error(err))
		}
	}()
}

// fetchAndStoreUsers fetches all users from the Slack API and updates the snapshot and cache file.
// Serialized by fetchUsersMu to prevent concurrent fetches from racing on snapshot/file writes.
func (ap *ApiProvider) fetchAndStoreUsers(ctx context.Context) error {
	ap.fetchUsersMu.Lock()
	defer ap.fetchUsersMu.Unlock()

	var (
		list        []slack.User
		optionLimit = slack.GetUsersOptionLimit(1000)
	)

	users, err := ap.client.GetUsersContext(ctx,
		optionLimit,
	)
	if err != nil {
		ap.logger.Error("Failed to fetch users", zap.Error(err))
		return err
	}

	if len(users) == 0 {
		if ap.usersReady.Load() {
			ap.logger.Warn("API returned zero users, keeping existing cache")
			return nil
		}
		return errors.New("API returned zero users and no existing cache is available")
	}

	list = append(list, users...)

	// Build new snapshot
	newSnapshot := &UsersCache{
		Users:    make(map[string]slack.User),
		UsersInv: make(map[string]string),
	}
	for _, user := range users {
		newSnapshot.Users[user.ID] = user
		newSnapshot.UsersInv[user.Name] = user.ID
	}
	// Store intermediate snapshot so GetSlackConnect can read current users
	ap.usersSnapshot.Store(newSnapshot)

	connectUsers, err := ap.GetSlackConnect(ctx)
	if err != nil {
		ap.logger.Error("Failed to fetch users from Slack Connect", zap.Error(err))
		return err
	}
	list = append(list, connectUsers...)

	// Add Slack Connect users to a new snapshot (since maps are shared)
	if len(connectUsers) > 0 {
		finalSnapshot := &UsersCache{
			Users:    make(map[string]slack.User, len(newSnapshot.Users)+len(connectUsers)),
			UsersInv: make(map[string]string, len(newSnapshot.UsersInv)+len(connectUsers)),
		}
		for k, v := range newSnapshot.Users {
			finalSnapshot.Users[k] = v
		}
		for k, v := range newSnapshot.UsersInv {
			finalSnapshot.UsersInv[k] = v
		}
		for _, user := range connectUsers {
			finalSnapshot.Users[user.ID] = user
			finalSnapshot.UsersInv[user.Name] = user.ID
		}
		ap.usersSnapshot.Store(finalSnapshot)
	}

	if data, err := json.MarshalIndent(list, "", "  "); err != nil {
		ap.logger.Error("Failed to marshal users for cache", zap.Error(err))
	} else {
		// Atomic write: temp file + rename to prevent partial/corrupt files
		if err := atomicWriteFile(ap.usersCachePath, data, 0600); err != nil {
			ap.logger.Error("Failed to write cache file",
				zap.String("cache_file", ap.usersCachePath),
				zap.Error(err))
		} else {
			ap.logger.Info("Wrote users to cache",
				zap.Int("count", len(list)),
				zap.String("cache_file", ap.usersCachePath))
		}
	}

	ap.usersReady.Store(true)

	return nil
}

func (ap *ApiProvider) RefreshChannels(ctx context.Context) error {
	return ap.refreshChannelsInternal(ctx, false)
}

// ForceRefreshChannels bypasses the cache and fetches fresh channel data from Slack API.
// Use this when a channel lookup fails to attempt recovery with fresh data.
// Rate limited by SLACK_MCP_MIN_REFRESH_INTERVAL (default 30s) to prevent API abuse.
// Returns ErrRefreshRateLimited if refresh is skipped due to rate limiting.
func (ap *ApiProvider) ForceRefreshChannels(ctx context.Context) error {
	if ap.minRefreshInterval > 0 {
		// Use single lock scope for check-and-update to prevent TOCTOU race
		ap.channelsMu.Lock()
		sinceLast := time.Since(ap.lastForcedChannelsRefresh)
		if sinceLast < ap.minRefreshInterval {
			ap.channelsMu.Unlock()
			ap.logger.Debug("Skipping forced channels refresh, within rate limit",
				zap.Duration("since_last", sinceLast),
				zap.Duration("min_interval", ap.minRefreshInterval))
			return ErrRefreshRateLimited
		}
		// Update timestamp before refresh to prevent concurrent forced refreshes
		ap.lastForcedChannelsRefresh = time.Now()
		ap.channelsMu.Unlock()
	}

	ap.logger.Info("Force refreshing channels cache")
	return ap.refreshChannelsInternal(ctx, true)
}

func (ap *ApiProvider) refreshChannelsInternal(ctx context.Context, force bool) error {
	ap.channelsMu.Lock()

	// Check if we should use cache (not forced, cache exists)
	if !force {
		if data, err := os.ReadFile(ap.channelsCachePath); err == nil {
			var cachedChannels []Channel
			if err := json.Unmarshal(data, &cachedChannels); err != nil {
				ap.logger.Warn("Failed to unmarshal channels cache, will refetch",
					zap.String("cache_file", ap.channelsCachePath),
					zap.Error(err))
			} else if len(cachedChannels) == 0 {
				ap.logger.Warn("Channels cache is empty or null, will refetch",
					zap.String("cache_file", ap.channelsCachePath))
			} else {
				// Re-map channels with current users cache to ensure DM names are populated
				usersMap := ap.ProvideUsersMap().Users
				newSnapshot := &ChannelsCache{
					Channels:    make(map[string]Channel, len(cachedChannels)),
					ChannelsInv: make(map[string]string, len(cachedChannels)),
				}
				for _, c := range cachedChannels {
					if c.IsIM {
						remappedChannel := mapChannel(
							c.ID, "", "", c.Topic, c.Purpose,
							c.User, c.Members, c.MemberCount,
							c.IsIM, c.IsMpIM, c.IsPrivate, c.IsMember, c.IsExtShared,
							usersMap,
						)
						newSnapshot.Channels[c.ID] = remappedChannel
						newSnapshot.ChannelsInv[remappedChannel.Name] = c.ID
					} else {
						newSnapshot.Channels[c.ID] = c
						newSnapshot.ChannelsInv[c.Name] = c.ID
					}
				}
				ap.channelsSnapshot.Store(newSnapshot)
				ap.channelsReady.Store(true)

				// Check cache TTL using file modification time
				cacheExpired := false
				if ap.cacheTTL > 0 {
					if fileInfo, err := os.Stat(ap.channelsCachePath); err == nil {
						cacheAge := time.Since(fileInfo.ModTime())
						if cacheAge > ap.cacheTTL {
							cacheExpired = true
							ap.logger.Info("Serving stale channels cache, background refresh starting",
								zap.Duration("cache_age", cacheAge),
								zap.Duration("ttl", ap.cacheTTL),
								zap.Int("count", len(cachedChannels)),
								zap.String("cache_file", ap.channelsCachePath))
						}
					}
				}

				if !cacheExpired {
					ap.logger.Info("Loaded channels from cache and re-mapped DM names",
						zap.Int("count", len(cachedChannels)),
						zap.String("cache_file", ap.channelsCachePath))
					ap.channelsMu.Unlock()
					return nil
				}

				// Cache is expired: release lock, spawn background refresh, return immediately
				ap.channelsMu.Unlock()
				ap.spawnBackgroundChannelsRefresh()
				return nil
			}
		}
	}

	// No usable cache: fetch fresh data synchronously (first run or force)
	ap.channelsMu.Unlock()
	return ap.fetchAndStoreChannels(ctx)
}

// mergeChannelCounts returns a new snapshot with unread info (HasUnreads,
// LastRead, MentionCount) from counts merged into snapshot's channels.
// The input snapshot is not mutated.
func mergeChannelCounts(snapshot *ChannelsCache, counts map[string]edge.ChannelSnapshot) *ChannelsCache {
	merged := &ChannelsCache{
		Channels:    make(map[string]Channel, len(snapshot.Channels)),
		ChannelsInv: make(map[string]string, len(snapshot.ChannelsInv)),
	}
	for id, ch := range snapshot.Channels {
		if snap, ok := counts[id]; ok {
			ch.HasUnreads = snap.HasUnreads
			ch.LastRead = snap.LastRead.SlackString()
			ch.MentionCount = snap.MentionCount
		}
		merged.Channels[id] = ch
	}
	for name, id := range snapshot.ChannelsInv {
		merged.ChannelsInv[name] = id
	}
	return merged
}

// channelsCacheExpired reports whether the channels cache file is older than
// the configured TTL. A missing file counts as expired; TTL 0 disables expiry.
func (ap *ApiProvider) channelsCacheExpired() bool {
	if ap.cacheTTL <= 0 {
		return false
	}
	fi, err := os.Stat(ap.channelsCachePath)
	if err != nil {
		return true
	}
	return time.Since(fi.ModTime()) > ap.cacheTTL
}

// RefreshChannelCounts refreshes only the unread counters (HasUnreads,
// LastRead, MentionCount) of the current channels snapshot using a single
// client.counts edge call, instead of re-listing every channel. The full
// channel list is refreshed in the background only when the cache file has
// passed its TTL. Returns ErrCountsUnavailable when counts cannot be fetched
// (e.g. no edge client), so callers can fall back to ForceRefreshChannels.
func (ap *ApiProvider) RefreshChannelCounts(ctx context.Context) error {
	if ap.channelsSnapshot.Load() == nil {
		return ErrCountsUnavailable
	}

	counts := ap.fetchChannelCounts(ctx)
	if counts == nil {
		return ErrCountsUnavailable
	}

	// Re-load under lock so a concurrent full refresh is not clobbered with
	// a merge based on an older snapshot.
	ap.channelsMu.Lock()
	snapshot := ap.channelsSnapshot.Load()
	if snapshot != nil {
		ap.channelsSnapshot.Store(mergeChannelCounts(snapshot, counts))
	}
	ap.channelsMu.Unlock()
	ap.logger.Info("Merged fresh unread counts into channels snapshot",
		zap.Int("counts", len(counts)))

	if ap.channelsCacheExpired() {
		ap.spawnBackgroundChannelsRefresh()
	}
	return nil
}

// spawnBackgroundChannelsRefresh starts a background goroutine to fetch fresh channel data.
func (ap *ApiProvider) spawnBackgroundChannelsRefresh() {
	if !ap.refreshingChannels.CompareAndSwap(false, true) {
		ap.logger.Debug("Skipping background channels refresh, already in progress")
		return
	}
	go func() {
		defer ap.refreshingChannels.Store(false)
		if err := ap.fetchAndStoreChannels(context.Background()); err != nil {
			ap.logger.Warn("Background channels refresh failed, continuing with stale data",
				zap.Error(err))
		}
	}()
}

// fetchAndStoreChannels fetches all channels from the Slack API and updates the snapshot and cache file.
// Serialized by fetchChannelsMu to prevent concurrent fetches from racing on snapshot/file writes.
func (ap *ApiProvider) fetchAndStoreChannels(ctx context.Context) error {
	ap.fetchChannelsMu.Lock()
	defer ap.fetchChannelsMu.Unlock()

	channels, err := ap.GetChannels(ctx, AllChanTypes)

	if err != nil {
		if ap.channelsReady.Load() {
			ap.logger.Warn("Channel refresh failed, keeping existing cache", zap.Error(err))
			return nil
		}
		return fmt.Errorf("channel refresh failed and no existing cache is available: %w", err)
	}

	if len(channels) == 0 {
		if ap.channelsReady.Load() {
			ap.logger.Warn("API returned zero channels, keeping existing cache")
			return nil
		}
		return errors.New("API returned zero channels and no existing cache is available")
	}

	if data, err := json.MarshalIndent(channels, "", "  "); err != nil {
		ap.logger.Error("Failed to marshal channels for cache", zap.Error(err))
	} else {
		// Atomic write: temp file + rename to prevent partial/corrupt files
		if err := atomicWriteFile(ap.channelsCachePath, data, 0600); err != nil {
			ap.logger.Error("Failed to write cache file",
				zap.String("cache_file", ap.channelsCachePath),
				zap.Error(err))
		} else {
			ap.logger.Info("Wrote channels to cache",
				zap.Int("count", len(channels)),
				zap.String("cache_file", ap.channelsCachePath))
		}
	}

	ap.channelsReady.Store(true)

	return nil
}

// RefreshChannelMembers loads the per-channel member-roster cache from disk on
// boot. Unlike RefreshUsers/RefreshChannels it makes no Slack call — rosters are
// fetched lazily per channel on first read. A missing (or unreadable/corrupt)
// cache file is not an error: the empty snapshot seeded by the constructor is
// kept so the server still boots.
func (ap *ApiProvider) RefreshChannelMembers(ctx context.Context) error {
	data, err := os.ReadFile(ap.membersCachePath)
	if err != nil {
		ap.logger.Info("No channel members cache to load, starting empty",
			zap.String("cache_file", ap.membersCachePath))
		return nil
	}

	var cached ChannelMembersCache
	if err := json.Unmarshal(data, &cached); err != nil {
		ap.logger.Warn("Failed to unmarshal channel members cache, starting empty",
			zap.String("cache_file", ap.membersCachePath),
			zap.Error(err))
		return nil
	}
	if cached.Channels == nil {
		cached.Channels = map[string]ChannelMemberRoster{}
	}
	ap.membersSnapshot.Store(&cached)
	ap.logger.Info("Loaded channel members from cache",
		zap.Int("channels", len(cached.Channels)),
		zap.String("cache_file", ap.membersCachePath))
	return nil
}

// GetChannelMembers returns the complete member roster for a single channel.
//   - Snapshot hit within the per-entry TTL (or TTL disabled) → serve from cache,
//     no Slack call.
//   - Snapshot hit but the entry has aged past the TTL → serve the stale IDs and
//     spawn a background refresh for this channel (serve-stale).
//   - Entry absent and a gather for this channel is already in flight → return
//     ErrChannelMembersNotReady so the caller waits instead of duplicating work.
//   - Entry absent → fetch synchronously so the first caller gets the roster.
func (ap *ApiProvider) GetChannelMembers(ctx context.Context, channelID string) ([]string, error) {
	snapshot := ap.membersSnapshot.Load()
	if roster, ok := snapshot.Channels[channelID]; ok {
		// TTL <= 0 disables expiry (cache forever), matching users/channels.
		if ap.cacheTTL <= 0 || time.Since(roster.FetchedAt) <= ap.cacheTTL {
			return roster.MemberIDs, nil
		}

		// Expired: serve stale IDs and refresh in the background.
		ap.logger.Info("Serving stale channel members, background refresh starting",
			zap.String("channel_id", channelID),
			zap.Duration("age", time.Since(roster.FetchedAt)),
			zap.Duration("ttl", ap.cacheTTL))
		ap.spawnBackgroundMembersRefresh(channelID)
		return roster.MemberIDs, nil
	}

	// Entry absent. Atomically claim the per-channel in-flight guard: if a
	// gather for this channel is already running, tell the caller to wait
	// rather than launching a duplicate synchronous fetch. This mirrors the
	// LoadOrStore guard in spawnBackgroundMembersRefresh and makes the
	// "still preparing" path reachable for concurrent first-time callers
	// (the loser of the race gets ErrChannelMembersNotReady). ForceRefresh
	// deliberately bypasses this guard (it uses its own per-channel throttle).
	if _, loaded := ap.refreshingMembers.LoadOrStore(channelID, struct{}{}); loaded {
		return nil, ErrChannelMembersNotReady
	}
	defer ap.refreshingMembers.Delete(channelID)

	// First run for this channel: fetch synchronously so the caller receives
	// the complete roster immediately.
	return ap.fetchAndStoreChannelMembers(ctx, channelID)
}

// ForceRefreshChannelMembers bypasses the cache and re-fetches a channel's
// members from Slack. Throttled per channel by SLACK_MCP_MIN_REFRESH_INTERVAL
// (default 30s); returns ErrRefreshRateLimited if a forced refresh for this
// channel happened within the window.
func (ap *ApiProvider) ForceRefreshChannelMembers(ctx context.Context, channelID string) ([]string, error) {
	if ap.minRefreshInterval > 0 {
		// Single lock scope for check-and-update to prevent a TOCTOU race.
		ap.membersMu.Lock()
		sinceLast := time.Since(ap.lastForcedMembersRefresh[channelID])
		if sinceLast < ap.minRefreshInterval {
			ap.membersMu.Unlock()
			ap.logger.Debug("Skipping forced channel members refresh, within rate limit",
				zap.String("channel_id", channelID),
				zap.Duration("since_last", sinceLast),
				zap.Duration("min_interval", ap.minRefreshInterval))
			return nil, ErrRefreshRateLimited
		}
		// Record the timestamp before refreshing to block concurrent forced refreshes.
		ap.lastForcedMembersRefresh[channelID] = time.Now()
		ap.membersMu.Unlock()
	}

	ap.logger.Info("Force refreshing channel members cache",
		zap.String("channel_id", channelID))
	return ap.fetchAndStoreChannelMembers(ctx, channelID)
}

// spawnBackgroundMembersRefresh starts a background goroutine to re-fetch a
// single channel's roster. refreshingMembers.LoadOrStore acts as the per-channel
// CAS guard so at most one background gather runs per channel at a time.
func (ap *ApiProvider) spawnBackgroundMembersRefresh(channelID string) {
	if _, loaded := ap.refreshingMembers.LoadOrStore(channelID, struct{}{}); loaded {
		ap.logger.Debug("Skipping background channel members refresh, already in progress",
			zap.String("channel_id", channelID))
		return
	}
	go func() {
		defer ap.refreshingMembers.Delete(channelID)
		if _, err := ap.fetchAndStoreChannelMembers(context.Background(), channelID); err != nil {
			ap.logger.Warn("Background channel members refresh failed, continuing with stale data",
				zap.String("channel_id", channelID),
				zap.Error(err))
		}
	}()
}

// channelMembersPage is one conversations.members page returned through the
// rate-limited retry wrapper: the member IDs plus the next-page cursor.
type channelMembersPage struct {
	ids    []string
	cursor string
}

// fetchAndStoreChannelMembers fetches a channel's complete member roster from
// Slack, copy-on-writes a new ChannelMembersCache with this channel's entry
// replaced, atomically persists the whole map, and returns the fetched IDs.
// Each conversations.members page is routed through the shared rate limiter and
// 429 retry individually, resuming from the current cursor on a retryable error
// rather than restarting the gather (the edge path returns the whole roster in a
// single page). An empty roster is valid (an empty channel legitimately has no
// members) and is stored as-is. Serialized by fetchMembersMu so concurrent
// fetches don't race on the snapshot swap / file write.
func (ap *ApiProvider) fetchAndStoreChannelMembers(ctx context.Context, channelID string) ([]string, error) {
	retryAfter := func(err error) time.Duration {
		var rle *slack.RateLimitedError
		if errors.As(err, &rle) {
			return rle.RetryAfter
		}
		return 0
	}

	// Drive pagination here so every page passes through the shared limiter +
	// 429 retry. On a retryable error CallWithRetry re-runs the current page
	// (the cursor is unchanged), so a late-page 429 resumes instead of
	// restarting from page 1.
	var ids []string
	cursor := ""
	for {
		page, err := limiter.CallWithRetry(ctx, ap.rateLimiter, 2, retryAfter,
			func() (channelMembersPage, error) {
				members, next, err := ap.client.GetUsersInConversationContext(ctx, &slack.GetUsersInConversationParameters{
					ChannelID: channelID,
					Limit:     1000,
					Cursor:    cursor,
				})
				return channelMembersPage{ids: members, cursor: next}, err
			})
		if err != nil {
			ap.logger.Error("Failed to fetch channel members",
				zap.String("channel_id", channelID),
				zap.Error(err))
			return nil, err
		}
		ids = append(ids, page.ids...)
		if page.cursor == "" {
			break
		}
		cursor = page.cursor
	}

	roster := ChannelMemberRoster{
		MemberIDs: ids,
		FetchedAt: time.Now(),
	}

	ap.fetchMembersMu.Lock()
	defer ap.fetchMembersMu.Unlock()

	// Copy-on-write: build a fresh map from the current snapshot and replace this
	// channel's entry. Never mutate the immutable snapshot — readers hold it lock-free.
	current := ap.membersSnapshot.Load()
	newCache := &ChannelMembersCache{
		Channels: make(map[string]ChannelMemberRoster, len(current.Channels)+1),
	}
	for k, v := range current.Channels {
		newCache.Channels[k] = v
	}
	newCache.Channels[channelID] = roster
	ap.membersSnapshot.Store(newCache)

	if data, err := json.MarshalIndent(newCache, "", "  "); err != nil {
		ap.logger.Error("Failed to marshal channel members for cache", zap.Error(err))
	} else if err := atomicWriteFile(ap.membersCachePath, data, 0600); err != nil {
		ap.logger.Error("Failed to write channel members cache file",
			zap.String("cache_file", ap.membersCachePath),
			zap.Error(err))
	} else {
		ap.logger.Info("Wrote channel members to cache",
			zap.String("channel_id", channelID),
			zap.Int("count", len(ids)),
			zap.String("cache_file", ap.membersCachePath))
	}

	return ids, nil
}

func (ap *ApiProvider) GetSlackConnect(ctx context.Context) ([]slack.User, error) {
	boot, err := ap.client.ClientUserBoot(ctx)
	if err != nil {
		ap.logger.Error("Failed to fetch client user boot", zap.Error(err))
		return nil, err
	}

	usersSnapshot := ap.usersSnapshot.Load()
	var collectedIDs []string
	for _, im := range boot.IMs {
		if !im.IsShared && !im.IsExtShared {
			continue
		}

		_, ok := usersSnapshot.Users[im.User]
		if !ok {
			collectedIDs = append(collectedIDs, im.User)
		}
	}

	res := make([]slack.User, 0, len(collectedIDs))
	if len(collectedIDs) > 0 {
		usersInfo, err := ap.client.GetUsersInfo(strings.Join(collectedIDs, ","))
		if err != nil {
			ap.logger.Error("Failed to fetch users info for shared IMs", zap.Error(err))
			return nil, err
		}

		for _, u := range *usersInfo {
			res = append(res, u)
		}
	}

	return res, nil
}

func (ap *ApiProvider) getChannelsMultiType(ctx context.Context, channelTypes []string) ([]Channel, error) {
	params := &slack.GetConversationsParameters{
		Types:           channelTypes,
		Limit:           999,
		ExcludeArchived: true,
	}

	var (
		channels []slack.Channel
		chans    []Channel

		nextcur string
		err     error
	)

	for {
		if err := ap.rateLimiter.Wait(ctx); err != nil {
			ap.logger.Error("Rate limiter wait failed", zap.Error(err))
			return nil, err
		}

		channels, nextcur, err = ap.client.GetConversationsContext(ctx, params)
		ap.logger.Debug("Fetched channels",
			zap.Strings("channelTypes", channelTypes),
			zap.Int("count", len(channels)),
		)
		if err != nil {
			ap.logger.Error("Failed to fetch channels", zap.Error(err))
			return nil, err
		}

		for _, channel := range channels {
			ch := mapChannel(
				channel.ID,
				channel.Name,
				channel.NameNormalized,
				channel.Topic.Value,
				channel.Purpose.Value,
				channel.User,
				channel.Members,
				channel.NumMembers,
				channel.IsIM,
				channel.IsMpIM,
				channel.IsPrivate,
				channel.IsMember,
				channel.IsExtShared,
				ap.ProvideUsersMap().Users,
			)
			chans = append(chans, ch)
		}

		if nextcur == "" {
			break
		}

		params.Cursor = nextcur
	}
	return chans, nil
}

func (ap *ApiProvider) GetChannels(ctx context.Context, channelTypes []string) ([]Channel, error) {
	if len(channelTypes) == 0 {
		channelTypes = AllChanTypes
	}

	// Fetch all channel types in a single paginated call. The standard
	// conversations.list API supports multiple types per request, and the edge
	// API (Enterprise Grid + non-OAuth) returns all types regardless. This
	// avoids making 4 separate API round-trips (one per type).
	chans, err := ap.getChannelsMultiType(ctx, AllChanTypes)
	if err != nil {
		// Do not overwrite the existing snapshot with a partial/empty list.
		return nil, err
	}

	// Merge unread info from ClientCounts if edge client is available
	if counts := ap.fetchChannelCounts(ctx); counts != nil {
		for i, ch := range chans {
			if snap, ok := counts[ch.ID]; ok {
				chans[i].HasUnreads = snap.HasUnreads
				chans[i].LastRead = snap.LastRead.SlackString()
				chans[i].MentionCount = snap.MentionCount
			}
		}
	}

	// Build new snapshot with all fetched channels
	newSnapshot := &ChannelsCache{
		Channels:    make(map[string]Channel, len(chans)),
		ChannelsInv: make(map[string]string, len(chans)),
	}
	for _, ch := range chans {
		newSnapshot.Channels[ch.ID] = ch
		newSnapshot.ChannelsInv[ch.Name] = ch.ID
	}
	ap.channelsSnapshot.Store(newSnapshot)

	// Filter by requested channel types
	var res []Channel
	for _, t := range channelTypes {
		for _, channel := range newSnapshot.Channels {
			if t == "public_channel" && !channel.IsPrivate && !channel.IsIM && !channel.IsMpIM {
				res = append(res, channel)
			}
			if t == "private_channel" && channel.IsPrivate && !channel.IsIM && !channel.IsMpIM {
				res = append(res, channel)
			}
			if t == "im" && channel.IsIM {
				res = append(res, channel)
			}
			if t == "mpim" && channel.IsMpIM {
				res = append(res, channel)
			}
		}
	}

	return res, nil
}

func (ap *ApiProvider) ProvideUsersMap() *UsersCache {
	// Atomic load - no lock needed, snapshot is immutable
	return ap.usersSnapshot.Load()
}

func (ap *ApiProvider) ProvideChannelsMaps() *ChannelsCache {
	// Atomic load - no lock needed, snapshot is immutable
	return ap.channelsSnapshot.Load()
}

func (ap *ApiProvider) IsReady() (bool, error) {
	if !ap.usersReady.Load() {
		return false, ErrUsersNotReady
	}
	if !ap.channelsReady.Load() {
		return false, ErrChannelsNotReady
	}
	return true, nil
}

// SkipCache marks both users and channels caches as ready without loading
// any data. Lookups by #channel-name or @username will not work; callers
// must use channel/user IDs instead.
func (ap *ApiProvider) SkipCache() {
	ap.usersReady.Store(true)
	ap.channelsReady.Store(true)
}

func (ap *ApiProvider) ServerTransport() string {
	return ap.transport
}

func (ap *ApiProvider) Slack() SlackAPI {
	return ap.client
}

func (ap *ApiProvider) IsBotToken() bool {
	client, ok := ap.client.(*MCPSlackClient)
	return ok && client != nil && client.IsBotToken()
}

func (ap *ApiProvider) IsOAuth() bool {
	client, ok := ap.client.(*MCPSlackClient)
	return ok && client != nil && client.IsOAuth()
}

// slackUserIDPattern matches Slack user IDs (e.g., U07VCEPP4N5, W0123456789).
var slackUserIDPattern = regexp.MustCompile(`^[UW][A-Z0-9]{2,}$`)

// SearchUsers searches for users by name, email, or display name.
// If the query matches a Slack user ID pattern (e.g., U07VCEPP4N5), it looks up the user
// directly via the users.info API instead of searching.
// For OAuth tokens (xoxp/xoxb), it searches the local users cache using regex matching.
// For browser tokens (xoxc/xoxd), it uses the edge API's UsersSearch method.
func (ap *ApiProvider) SearchUsers(ctx context.Context, query string, limit int) ([]slack.User, error) {
	if slackUserIDPattern.MatchString(query) {
		users, err := ap.client.GetUsersInfo(query)
		if err != nil {
			return nil, err
		}
		if users != nil {
			return *users, nil
		}
		return nil, nil
	}

	if ap.IsOAuth() {
		return ap.searchUsersInCache(query, limit)
	}

	return ap.client.UsersSearch(ctx, query, limit)
}

// searchUsersInCache performs a case-insensitive regex search on cached users.
// Matches against username, real name, display name, and email.
func (ap *ApiProvider) searchUsersInCache(query string, limit int) ([]slack.User, error) {
	if !ap.usersReady.Load() {
		return nil, ErrUsersNotReady
	}

	pattern, err := regexp.Compile("(?i)" + regexp.QuoteMeta(query))
	if err != nil {
		return nil, err
	}

	usersCache := ap.usersSnapshot.Load()
	var results []slack.User
	for _, user := range usersCache.Users {
		if user.Deleted {
			continue
		}

		if pattern.MatchString(user.Name) ||
			pattern.MatchString(user.RealName) ||
			pattern.MatchString(user.Profile.DisplayName) ||
			pattern.MatchString(user.Profile.Email) {
			results = append(results, user)

			if len(results) >= limit {
				break
			}
		}
	}

	return results, nil
}

// fetchChannelCounts returns unread counts per channel via the edge client.
// Returns nil if edge client is not available (e.g. standard bot tokens).
func (ap *ApiProvider) fetchChannelCounts(ctx context.Context) map[string]edge.ChannelSnapshot {
	client, ok := ap.client.(*MCPSlackClient)
	if !ok || client == nil {
		return nil
	}
	raw := client.Raw()
	if raw.Edge == nil {
		return nil
	}
	cr, err := raw.Edge.ClientCounts(ctx)
	if err != nil {
		ap.logger.Warn("Failed to fetch client counts for unread info", zap.Error(err))
		return nil
	}
	counts := make(map[string]edge.ChannelSnapshot)
	for _, ch := range cr.Channels {
		counts[ch.ID] = ch
	}
	for _, ch := range cr.MPIMs {
		counts[ch.ID] = ch
	}
	for _, ch := range cr.IMs {
		counts[ch.ID] = ch
	}
	return counts
}

func mapChannel(
	id, name, nameNormalized, topic, purpose, user string,
	members []string,
	numMembers int,
	isIM, isMpIM, isPrivate, isMember, isExtShared bool,
	usersMap map[string]slack.User,
) Channel {
	channelName := name
	finalPurpose := purpose
	finalTopic := topic
	finalMemberCount := numMembers

	var userID string
	if isIM {
		finalMemberCount = 2
		userID = user // Store the user ID for later re-mapping

		// If user field is empty but we have members, try to extract from members
		if userID == "" && len(members) > 0 {
			// For IM channels, members should contain the other user's ID
			// Try each member to find a valid user in the users map
			for _, memberID := range members {
				if _, ok := usersMap[memberID]; ok {
					userID = memberID
					break
				}
			}
		}

		if u, ok := usersMap[userID]; ok {
			channelName = "@" + u.Name
			finalPurpose = "DM with " + u.RealName
		} else if userID != "" {
			channelName = "@" + userID
			finalPurpose = "DM with " + userID
		} else {
			channelName = "@"
			finalPurpose = "DM with "
		}
		finalTopic = ""
	} else if isMpIM {
		if len(members) > 0 {
			finalMemberCount = len(members)
			var userNames []string
			for _, uid := range members {
				if u, ok := usersMap[uid]; ok {
					userNames = append(userNames, u.RealName)
				} else {
					userNames = append(userNames, uid)
				}
			}
			channelName = "@" + nameNormalized
			finalPurpose = "Group DM with " + strings.Join(userNames, ", ")
			finalTopic = ""
		}
	} else {
		channelName = "#" + nameNormalized
	}

	return Channel{
		ID:          id,
		Name:        channelName,
		Topic:       finalTopic,
		Purpose:     finalPurpose,
		MemberCount: finalMemberCount,
		IsIM:        isIM,
		IsMpIM:      isMpIM,
		IsPrivate:   isPrivate,
		IsMember:    isMember,
		IsExtShared: isExtShared,
		User:        userID,
		Members:     members,
	}
}

// MapChannelFromSlack converts a slack.Channel to our internal Channel type.
func MapChannelFromSlack(c slack.Channel, usersMap map[string]slack.User) Channel {
	return mapChannel(
		c.ID, c.Name, c.NameNormalized,
		c.Topic.Value, c.Purpose.Value,
		c.User, c.Members, c.NumMembers,
		c.IsIM, c.IsMpIM, c.IsPrivate, c.IsMember, c.IsExtShared,
		usersMap,
	)
}
