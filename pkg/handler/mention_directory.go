package handler

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/korotovsky/slack-mcp-server/pkg/limiter"
	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

// mentionNamer resolves a Slack reference to the display form the draft
// readback shows: "@dana" for a user, "#general" for a channel, "@eng" for a
// user group.
//
// Name never returns an error. The readback is a display aid, not a source of
// truth, and a lookup failure must degrade to the canonical literal rather than
// fail the tool call or — worse — silently drop the reference. "" means "not
// known"; the caller falls back to mentionRef.canonicalLiteral().
type mentionNamer interface {
	Name(ctx context.Context, ref mentionRef) string
}

// The two verification outcomes AC 2.4 requires be told apart. They are
// sentinels rather than message text because the split drives what the calling
// assistant should do next, and that decision must not depend on parsing prose:
//
//   - errRefNotFound — the workspace was consulted and has no such user,
//     channel or group. Self-correctable: fix the ID and retry.
//   - errRefUnverifiable — the workspace could not be consulted at all (a
//     missing scope, a 429 that outlived its retries, a transport failure).
//     The reference may well be perfectly valid, so the assistant must NOT be
//     told to "correct" it.
//
// Reporting an unverifiable reference as not-found would tell the assistant to
// rewrite a reference that is actually fine — which is why, for instance, a
// private channel the token cannot see maps to unverifiable and never to
// not-found.
var (
	errRefNotFound     = errors.New("reference does not exist in this workspace")
	errRefUnverifiable = errors.New("reference could not be verified")
)

// errNoSlackClient is the underlying cause when the provider has no usable
// Slack client at all (demo mode). It is an unverifiable cause, never a
// not-found one: nothing was consulted.
var errNoSlackClient = errors.New("no Slack client is configured")

// refError is what mentionVerifier.Verify returns: a class (one of the two
// sentinels above, reachable through errors.Is) plus the human detail that
// goes inside the parentheses of the message validateMentions builds.
type refError struct {
	class  error
	detail string
}

func (e *refError) Error() string { return e.detail + ": " + e.class.Error() }
func (e *refError) Unwrap() error { return e.class }

func refNotFound(detail string) error {
	return &refError{class: errRefNotFound, detail: detail}
}

func refUnverifiable(format string, args ...any) error {
	return &refError{class: errRefUnverifiable, detail: fmt.Sprintf(format, args...)}
}

// refDetail extracts the cause phrase from a Verify error.
func refDetail(err error) string {
	var re *refError
	if errors.As(err, &re) {
		return re.detail
	}
	return err.Error()
}

// mentionVerifier answers whether a reference names something real in the
// workspace. It is split from mentionNamer because the two obligations differ:
// naming is a display aid that degrades to "" on failure, while verification
// gates a write and must therefore report *why* it could not answer.
type mentionVerifier interface {
	Verify(ctx context.Context, ref mentionRef) error
}

// mentionResolver is what the handlers hold: one object serving both the
// validation gate and the readback, so a single usergroups.list fetch covers
// both.
type mentionResolver interface {
	mentionVerifier
	mentionNamer
}

// slackErrorCode returns Slack's machine-readable error code
// ("channel_not_found", "missing_scope", …), or "" when the error is not a
// Slack API response.
func slackErrorCode(err error) string {
	var sre slack.SlackErrorResponse
	if errors.As(err, &sre) {
		return sre.Err
	}
	return ""
}

// validateMentions verifies every live mention in a converted block and returns
// the first failure, phrased for the calling assistant.
//
// It is called from the handlers, never from the converter: markdownToRichTextBlock
// stays pure (no ctx, no provider, no network), which keeps every conversion
// unit test network-free and preserves the markdownToRichTextBlockFn seam.
//
// The two message forms are deliberately, textually distinct — see the sentinel
// comment above. Both name the offending reference by its canonical literal and
// both state that nothing was written, because the assistant's next move
// depends on knowing the destination is untouched.
func validateMentions(ctx context.Context, rtb *slack.RichTextBlock, v mentionVerifier) error {
	if rtb == nil || v == nil {
		return nil
	}
	for _, ref := range collectMentionRefs(rtb) {
		// A malformed reference is refused on the grammar alone — no lookup can
		// help, because the code cannot name what the syntax claims. It carries
		// the not-found class: it is self-correctable, exactly like a well-formed
		// ID that names nobody, and the assistant should fix it and retry.
		if ref.Kind.malformed() {
			return fmt.Errorf("reference %s is not a valid %s reference: %s; nothing was written — correct the reference and retry",
				ref.canonicalLiteral(), malformedRefNoun(ref.Kind), ref.malformedReason())
		}

		err := v.Verify(ctx, ref)
		if err == nil {
			continue
		}
		literal := ref.canonicalLiteral()
		if errors.Is(err, errRefNotFound) {
			return fmt.Errorf("reference %s does not exist in this workspace (%s); nothing was written — correct the reference and retry", literal, refDetail(err))
		}
		return fmt.Errorf("reference %s could not be verified — the workspace could not be consulted (%s). This is \"could not check\", not \"does not exist\"; nothing was written. Retry, or remove the reference", literal, refDetail(err))
	}
	return nil
}

// malformedRefNoun names the thing the reference's syntax claimed to point at,
// in the words the functional spec uses ("person reference", "channel
// reference", "user-group reference").
func malformedRefNoun(k mentionKind) string {
	switch k {
	case mentionBadChannel:
		return "channel"
	case mentionBadUserGroup:
		return "user-group"
	default:
		return "person"
	}
}

// The limiters the mention directory routes its live lookups through.
//
// They are package-level, and that is the whole point: tier.Limiter()
// constructs a *new* rate.Limiter on every call, so building one inside a
// per-reference lookup would give each reference its own full burst and
// throttle nothing at all. One limiter per Slack method, shared by every
// directory in the process, is what makes a draft mentioning ten uncached
// channels throttle instead of earning a 429 on the write path — and it makes
// validation and the readback of the same draft draw from the same budget
// rather than doubling up.
//
// The throttle lives here rather than inside provider.PatchUser /
// GetConversationInfoContext because those are shared with the message render
// path, where a limiter wait would add seconds of latency to a history page
// with several uncached users. Only the mention gate needs the burst control.
var (
	// users.info — Slack rates it at Tier 4; Tier3 is deliberately conservative
	// for a write-path burst.
	mentionUserInfoLimiter = limiter.Tier3.Limiter()
	// conversations.info, shared by verifyChannel and channelName.
	mentionChannelInfoLimiter = limiter.Tier3.Limiter()
	// usergroups.list — at most one fetch per directory, but shared so
	// concurrent tool calls cannot burst past the tier either.
	mentionUserGroupsLimiter = limiter.Tier2.Limiter()
)

// mentionDirectory names Slack references, cache-first with a live fallback.
//
// One instance per tool call. It memoises every lookup — positive and negative
// alike — for the call's duration, so a message mentioning the same person
// three times costs at most one users.info, and the usergroups.list fetch is
// issued at most once. It is deliberately not longer-lived than the call: names
// resolve live, so caching them across calls would serve a renamed user's old
// handle indefinitely.
//
// The user-group list is fetched lazily, only when a <!subteam^…> reference is
// actually named. A message that references no group makes zero usergroups.list
// calls — the whole reason user groups are consulted live instead of being
// persisted alongside the users and channels caches.
type mentionDirectory struct {
	ap     *provider.ApiProvider
	logger *zap.Logger

	// mu guards every field below and is held across the live lookups too, so
	// two concurrent references to the same unknown ID cost one API call rather
	// than two. A directory serves a single tool call, so the contention this
	// costs is nil.
	mu sync.Mutex
	// names memoises resolved display names by canonical literal. A stored ""
	// is a remembered miss: it is never retried within the call.
	names map[string]string
	// verified memoises verification outcomes by canonical literal. Negative
	// results are stored too — a message that mentions the same bad ID three
	// times costs one lookup, not three.
	verified map[string]error
	// groups maps user-group ID to display handle, populated by the single
	// usergroups.list fetch. nil after a failed fetch.
	groups map[string]string
	// groupsFetched records that the fetch was attempted, so a failure is not
	// retried once per referenced group.
	groupsFetched bool
	// groupsErr is why the fetch failed, kept so the verification error can
	// name the cause (a missing scope, most usefully).
	groupsErr error
}

func newMentionDirectory(ap *provider.ApiProvider, logger *zap.Logger) *mentionDirectory {
	return &mentionDirectory{
		ap:       ap,
		logger:   logger,
		names:    make(map[string]string),
		verified: make(map[string]error),
	}
}

// Name resolves one reference to its display form, or "" when it cannot be
// determined. It never errors: see mentionNamer.
func (d *mentionDirectory) Name(ctx context.Context, ref mentionRef) string {
	switch ref.Kind {
	case mentionUser, mentionChannel, mentionUserGroup:
	default:
		// Broadcasts have no directory entry — canonicalLiteral already reads
		// "@here" / "@channel" / "@everyone". Returning early keeps them free of
		// any lookup at all.
		return ""
	}
	if d == nil || d.ap == nil || ref.ID == "" {
		return ""
	}

	key := ref.canonicalLiteral()

	d.mu.Lock()
	defer d.mu.Unlock()
	if name, ok := d.names[key]; ok {
		return name
	}

	var name string
	switch ref.Kind {
	case mentionUser:
		name = d.userName(ctx, ref.ID)
	case mentionChannel:
		name = d.channelName(ctx, ref.ID)
	case mentionUserGroup:
		name = d.userGroupName(ctx, ref.ID)
	}
	d.names[key] = name
	return name
}

// Verify reports whether a reference names something real in the workspace,
// cache-first with a live fallback, memoising the outcome — positive and
// negative alike — for the directory's lifetime.
//
// Broadcasts are never verified: the converter turns them into inert text, so
// there is nothing to resolve and nothing that could notify anyone.
func (d *mentionDirectory) Verify(ctx context.Context, ref mentionRef) error {
	switch ref.Kind {
	case mentionUser, mentionChannel, mentionUserGroup:
	default:
		return nil
	}
	if ref.ID == "" {
		// Unreachable through the grammar (every non-broadcast class captures an
		// ID), but a missing ID is unverifiable, never proof of absence.
		return refUnverifiable("the reference carries no ID")
	}
	if d == nil || d.ap == nil {
		return refUnverifiable("%v", errNoSlackClient)
	}

	key := ref.canonicalLiteral()

	d.mu.Lock()
	defer d.mu.Unlock()
	if err, ok := d.verified[key]; ok {
		return err
	}

	var err error
	switch ref.Kind {
	case mentionUser:
		err = d.verifyUser(ctx, ref.ID)
	case mentionChannel:
		err = d.verifyChannel(ctx, ref.ID)
	case mentionUserGroup:
		err = d.verifyUserGroup(ctx, ref.ID)
	}
	d.verified[key] = err
	return err
}

// verifyUser resolves a user ID against the users snapshot, falling back to the
// targeted users.info patch. provider.ErrUserNotFound is the only outcome that
// proves absence; everything else — transport, invalid_auth, a 429 that
// outlived its retries — is "could not check".
func (d *mentionDirectory) verifyUser(ctx context.Context, id string) error {
	if users := d.ap.ProvideUsersMap(); users != nil {
		if _, ok := users.Users[id]; ok {
			return nil
		}
	}
	if !d.slackReachable() {
		return refUnverifiable("users.info could not be called: %v", errNoSlackClient)
	}
	_, err := limiter.CallWithRetry(ctx, mentionUserInfoLimiter, 2, slackRetryAfter, func() (*slack.User, error) {
		return d.ap.PatchUser(ctx, id)
	})
	if err != nil {
		if errors.Is(err, provider.ErrUserNotFound) {
			return refNotFound("no such user")
		}
		return refUnverifiable("users.info failed: %v", err)
	}
	return nil
}

// verifyChannel resolves a channel ID against the channels snapshot, falling
// back to conversations.info.
//
// Only channel_not_found proves absence. missing_scope and not_in_channel
// deliberately map to could-not-verify: a private channel this token cannot see
// is not proof that it does not exist, and reporting it as not-found would tell
// the assistant to "correct" a reference that is perfectly valid.
func (d *mentionDirectory) verifyChannel(ctx context.Context, id string) error {
	if channels := d.ap.ProvideChannelsMaps(); channels != nil {
		if _, ok := channels.Channels[id]; ok {
			return nil
		}
	}
	if !d.slackReachable() {
		return refUnverifiable("conversations.info could not be called: %v", errNoSlackClient)
	}

	info, err := limiter.CallWithRetry(ctx, mentionChannelInfoLimiter, 2, slackRetryAfter, func() (*slack.Channel, error) {
		return d.ap.Slack().GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{
			ChannelID: id,
		})
	})
	if err != nil {
		switch slackErrorCode(err) {
		case "channel_not_found":
			return refNotFound("no such channel")
		case "missing_scope":
			return refUnverifiable("conversations.info failed: missing_scope — the token lacks the scope needed to read this conversation (channels:read / groups:read / im:read), which is not proof the channel is absent")
		case "not_in_channel":
			return refUnverifiable("conversations.info failed: not_in_channel — the token cannot see this conversation, which is not proof the channel is absent")
		}
		return refUnverifiable("conversations.info failed: %v", err)
	}
	if info == nil {
		return refUnverifiable("conversations.info returned no channel")
	}
	return nil
}

// verifyUserGroup resolves a group ID against the per-call memoised
// usergroups.list. A fetched list with no matching entry is the only proof of
// absence; a failed fetch — missing_scope, paid_only, not_allowed_token_type, a
// surviving 429 — is "could not check", and the message names the scope when
// that is the cause, because it is the one failure the operator can fix.
func (d *mentionDirectory) verifyUserGroup(ctx context.Context, id string) error {
	groups, err := d.userGroups(ctx)
	if err != nil {
		switch slackErrorCode(err) {
		case "missing_scope":
			return refUnverifiable("usergroups.list failed: missing_scope — the token is missing the usergroups:read scope")
		case "paid_only":
			return refUnverifiable("usergroups.list failed: paid_only — user groups are unavailable on this workspace's plan")
		case "not_allowed_token_type":
			return refUnverifiable("usergroups.list failed: not_allowed_token_type — this token type cannot list user groups")
		}
		return refUnverifiable("usergroups.list failed: %v", err)
	}
	if _, ok := groups[id]; !ok {
		return refNotFound("no such user group")
	}
	return nil
}

// slackReachable reports whether the provider actually has a Slack client to
// call. In demo mode ApiProvider holds a typed-nil *MCPSlackClient, so
// ap.Slack() is a non-nil interface over which every call would panic; this is
// the same nil-client check ApiProvider.IsOAuth and IsBotToken use. Without it
// the readback — a display aid — could crash the tool call, which is precisely
// the failure mode "Name never errors" exists to prevent.
func (d *mentionDirectory) slackReachable() bool {
	api := d.ap.Slack()
	if api == nil {
		return false
	}
	client, ok := api.(*provider.MCPSlackClient)
	return !ok || client != nil
}

// userName resolves a user ID from the users snapshot, falling back to a
// targeted users.info patch on a miss — the same O(1) fetch userResolver uses
// for message rendering, rather than a full cache rebuild.
func (d *mentionDirectory) userName(ctx context.Context, id string) string {
	if users := d.ap.ProvideUsersMap(); users != nil {
		if u, ok := users.Users[id]; ok {
			return userMentionName(u)
		}
	}
	if !d.slackReachable() {
		return ""
	}
	patched, err := limiter.CallWithRetry(ctx, mentionUserInfoLimiter, 2, slackRetryAfter, func() (*slack.User, error) {
		return d.ap.PatchUser(ctx, id)
	})
	if err != nil || patched == nil {
		d.logger.Debug("Draft readback could not name user",
			zap.String("user_id", id), zap.Error(err))
		return ""
	}
	return userMentionName(*patched)
}

// userMentionName formats a user's Slack handle. Name (the @handle), not
// RealName: it is what the rest of this server uses for user handles, and what
// Slack itself renders in a mention.
func userMentionName(u slack.User) string {
	if u.Name == "" {
		return ""
	}
	return "@" + u.Name
}

// channelName resolves a channel ID from the channels snapshot, falling back to
// conversations.info on a miss. Cached channel names already carry their "#"
// (or "@" for a DM) prefix, and MapChannelFromSlack gives the fallback the same
// treatment, so the two paths cannot disagree about how a channel is displayed.
func (d *mentionDirectory) channelName(ctx context.Context, id string) string {
	if channels := d.ap.ProvideChannelsMaps(); channels != nil {
		if c, ok := channels.Channels[id]; ok {
			return displayableChannelName(c.Name)
		}
	}

	if !d.slackReachable() {
		return ""
	}

	info, err := limiter.CallWithRetry(ctx, mentionChannelInfoLimiter, 2, slackRetryAfter, func() (*slack.Channel, error) {
		return d.ap.Slack().GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{
			ChannelID: id,
		})
	})
	if err != nil || info == nil {
		d.logger.Debug("Draft readback could not name channel",
			zap.String("channel_id", id), zap.Error(err))
		return ""
	}

	// MapChannelFromSlack needs the users snapshot to name a DM after its
	// counterpart; a miss there only costs the "@handle" form, never an error.
	var usersMap map[string]slack.User
	if users := d.ap.ProvideUsersMap(); users != nil {
		usersMap = users.Users
	}
	return displayableChannelName(provider.MapChannelFromSlack(*info, usersMap).Name)
}

// displayableChannelName rejects a name that is nothing but its prefix, which
// is what mapChannel produces for a channel with no usable name. Showing a bare
// "#" would be less informative than the canonical literal it falls back to.
func displayableChannelName(name string) string {
	if name == "" || name == "#" || name == "@" {
		return ""
	}
	return name
}

// userGroupName resolves a user-group ID against the memoised list.
func (d *mentionDirectory) userGroupName(ctx context.Context, id string) string {
	groups, err := d.userGroups(ctx)
	if err != nil {
		return ""
	}
	return groups[id]
}

// userGroups returns the workspace's user groups keyed by ID, fetching them
// once per directory. usergroups.list is the only enumerating read slack-go
// exposes (there is no usergroups.info), so one list call serves every group
// referenced by the draft. Disabled groups are included: a draft can legitimately
// mention a group that was archived after it was written, and naming it is
// strictly better than showing the raw code.
//
// Returns a non-nil error when the list could not be fetched, which is
// categorically distinct from a fetched list that has no entry for the ID: the
// former is "could not check", the latter "does not exist", and the mention
// gate must never confuse the two. Every group is keyed, including one with no
// usable handle — its name is "" (the readback falls back to the canonical
// literal), but it must still count as present for verification.
func (d *mentionDirectory) userGroups(ctx context.Context) (map[string]string, error) {
	if d.groupsFetched {
		return d.groups, d.groupsErr
	}
	d.groupsFetched = true
	if !d.slackReachable() {
		d.groupsErr = errNoSlackClient
		return nil, d.groupsErr
	}

	groups, err := limiter.CallWithRetry(ctx, mentionUserGroupsLimiter, 2, slackRetryAfter, func() ([]slack.UserGroup, error) {
		return d.ap.Slack().GetUserGroupsContext(ctx,
			slack.GetUserGroupsOptionIncludeDisabled(true),
			slack.GetUserGroupsOptionIncludeUsers(false),
			slack.GetUserGroupsOptionIncludeCount(false),
		)
	})
	if err != nil {
		d.logger.Debug("Could not list user groups", zap.Error(err))
		d.groupsErr = err
		return nil, err
	}

	byID := make(map[string]string, len(groups))
	for _, g := range groups {
		byID[g.ID] = userGroupMentionName(g)
	}
	d.groups = byID
	return byID, nil
}

// userGroupMentionName formats a group's handle — "@eng" — which is what Slack
// renders in a group mention. Name is a readable title ("Engineering") and is
// used only when the group has no handle at all.
func userGroupMentionName(g slack.UserGroup) string {
	switch {
	case g.Handle != "":
		return "@" + g.Handle
	case g.Name != "":
		return "@" + g.Name
	default:
		return ""
	}
}
