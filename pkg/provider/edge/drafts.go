package edge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/trace"
	"strings"

	"github.com/google/uuid"
)

// drafts.* API (undocumented web-client endpoint)

// DraftDestination is a draft's target channel, optionally scoped to a thread.
type DraftDestination struct {
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts,omitempty"`
}

// buildDraftDestinations builds the JSON `destinations` payload for a single
// target channel, optionally scoped to a thread.
func buildDraftDestinations(channelID, threadTs string) (string, error) {
	b, err := json.Marshal([]DraftDestination{{ChannelID: channelID, ThreadTS: threadTs}})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type draftsCreateForm struct {
	BaseRequest
	ClientMsgID    string `json:"client_msg_id"`
	Blocks         string `json:"blocks"`
	Destinations   string `json:"destinations"`
	FileIDs        string `json:"file_ids"`
	IsFromComposer bool   `json:"is_from_composer"`
	WebClientFields
}

type draftsCreateResponse struct {
	baseResponse
	DraftID string `json:"draft_id"`
	Draft   struct {
		ID string `json:"id"`
	} `json:"draft"`
}

func (r draftsCreateResponse) draftID() string {
	if r.DraftID != "" {
		return r.DraftID
	}
	return r.Draft.ID
}

// DraftsCreate creates a native Slack draft in channelID (optionally threaded
// under threadTs) with the given pre-rendered block kit JSON. Returns the new
// draft's identifier. Requires a session token (xoxc/xoxd); bot tokens cannot
// reach the edge API.
func (cl *Client) DraftsCreate(ctx context.Context, channelID, threadTs string, blocks json.RawMessage) (string, error) {
	ctx, task := trace.NewTask(ctx, "DraftsCreate")
	defer task.End()
	trace.Logf(ctx, "params", "channelID=%v threadTs=%v", channelID, threadTs)

	dest, err := buildDraftDestinations(channelID, threadTs)
	if err != nil {
		return "", err
	}

	form := draftsCreateForm{
		BaseRequest:  BaseRequest{Token: cl.token},
		ClientMsgID:  uuid.NewString(),
		Blocks:       string(blocks),
		Destinations: dest,
		FileIDs:      "[]",
		// Origin metadata only (does not control composer attachment); mirrors
		// what the web client sends on drafts.create.
		IsFromComposer:  false,
		WebClientFields: webclientReason(""),
	}

	resp, err := cl.PostForm(ctx, "drafts.create", values(form, true))
	if err != nil {
		return "", err
	}

	var r draftsCreateResponse
	if err := cl.ParseResponse(&r, resp); err != nil {
		return "", err
	}
	if err := r.validate("drafts.create"); err != nil {
		return "", err
	}
	id := r.draftID()
	if id == "" {
		// ok:true but no id in either known field indicates the response shape
		// has changed; surface it instead of returning an empty confirmation.
		return "", fmt.Errorf("drafts.create: draft created but response contained no draft id (unexpected payload shape)")
	}
	return id, nil
}

// Draft is a single entry from drafts.list. Only the fields needed to detect a
// duplicate draft for a destination and to drive drafts.update are decoded.
type Draft struct {
	ID            string             `json:"id"`
	ClientMsgID   string             `json:"client_msg_id"`
	LastUpdatedTS string             `json:"last_updated_ts"`
	DateScheduled int64              `json:"date_scheduled"`
	IsSent        bool               `json:"is_sent"`
	IsDeleted     bool               `json:"is_deleted"`
	Destinations  []DraftDestination `json:"destinations"`
}

type draftsListForm struct {
	BaseRequest
	Limit    int  `json:"limit"`
	IsActive bool `json:"is_active"`
	WebClientFields
}

type draftsListResponse struct {
	baseResponse
	Drafts  []Draft `json:"drafts"`
	HasMore bool    `json:"has_more"`
}

// DraftsList returns the caller's active drafts (up to limit). It requests
// is_active=true because drafts.list otherwise returns every draft including
// sent ones and the server caps the page at 100 — so without the filter an
// active draft can be buried past the cap and missed, causing the upsert to
// create a duplicate. Requires a session token (xoxc/xoxd); bot tokens cannot
// reach the edge API.
func (cl *Client) DraftsList(ctx context.Context, limit int) ([]Draft, error) {
	ctx, task := trace.NewTask(ctx, "DraftsList")
	defer task.End()
	trace.Logf(ctx, "params", "limit=%v", limit)

	form := draftsListForm{
		BaseRequest:     BaseRequest{Token: cl.token},
		Limit:           limit,
		IsActive:        true,
		WebClientFields: webclientReason(""),
	}

	resp, err := cl.PostForm(ctx, "drafts.list", values(form, true))
	if err != nil {
		return nil, err
	}

	var r draftsListResponse
	if err := cl.ParseResponse(&r, resp); err != nil {
		return nil, err
	}
	if err := r.validate("drafts.list"); err != nil {
		return nil, err
	}
	if r.HasMore {
		// We don't paginate (drafts are a small per-user set). Surface the rare
		// case where an existing draft sits beyond the limit, since the upsert
		// could then miss it and create a duplicate.
		slog.Default().Warn("drafts.list returned more drafts than the fetch limit; a draft beyond the limit may be missed", "limit", limit)
	}
	return r.Drafts, nil
}

// padDraftTS normalizes a Slack draft timestamp's fractional part to exactly 7
// digits, the form drafts.update expects in client_last_updated_ts. Shorter
// fractions are right-padded with zeros, longer ones truncated. Timestamps
// without a fractional part are returned unchanged.
func padDraftTS(ts string) string {
	i := strings.Index(ts, ".")
	if i < 0 {
		return ts
	}
	frac := ts[i+1:]
	switch {
	case len(frac) < 7:
		frac += strings.Repeat("0", 7-len(frac))
	case len(frac) > 7:
		frac = frac[:7]
	}
	return ts[:i+1] + frac
}

type draftsUpdateForm struct {
	BaseRequest
	DraftID             string `json:"draft_id"`
	ClientLastUpdatedTS string `json:"client_last_updated_ts"`
	ClientMsgID         string `json:"client_msg_id"`
	Blocks              string `json:"blocks"`
	Destinations        string `json:"destinations"`
	FileIDs             string `json:"file_ids"`
	IsFromComposer      string `json:"is_from_composer"`
	DateScheduled       string `json:"date_scheduled"`
	WebClientFields
}

// DraftsUpdate replaces the content of an existing draft (draftID) in place.
// lastUpdatedTS is the draft's last_updated_ts from DraftsList; it is sent as
// client_last_updated_ts (an optimistic-concurrency token the endpoint requires).
// clientMsgID should be the draft's existing client_msg_id; a fresh one is
// generated if empty. Requires a session token (xoxc/xoxd).
func (cl *Client) DraftsUpdate(ctx context.Context, draftID, clientMsgID, lastUpdatedTS, channelID, threadTs string, blocks json.RawMessage) error {
	ctx, task := trace.NewTask(ctx, "DraftsUpdate")
	defer task.End()
	trace.Logf(ctx, "params", "draftID=%v channelID=%v threadTs=%v", draftID, channelID, threadTs)

	dest, err := buildDraftDestinations(channelID, threadTs)
	if err != nil {
		return err
	}

	if clientMsgID == "" {
		clientMsgID = uuid.NewString()
	}

	form := draftsUpdateForm{
		BaseRequest:         BaseRequest{Token: cl.token},
		DraftID:             draftID,
		ClientLastUpdatedTS: padDraftTS(lastUpdatedTS),
		ClientMsgID:         clientMsgID,
		Blocks:              string(blocks),
		Destinations:        dest,
		FileIDs:             "[]",
		// Origin metadata only (does not control composer attachment); mirrors
		// what the web client sends on drafts.update.
		IsFromComposer:  "true",
		DateScheduled:   "0",
		WebClientFields: webclientReason(""),
	}

	resp, err := cl.PostForm(ctx, "drafts.update", values(form, true))
	if err != nil {
		return err
	}

	var r baseResponse
	if err := cl.ParseResponse(&r, resp); err != nil {
		return err
	}
	return r.validate("drafts.update")
}
