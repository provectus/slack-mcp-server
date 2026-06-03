package edge

import (
	"context"
	"encoding/json"
	"runtime/trace"

	"github.com/google/uuid"
)

// drafts.* API (undocumented web-client endpoint)

type draftDestination struct {
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts,omitempty"`
}

// buildDraftDestinations builds the JSON `destinations` payload for a single
// target channel, optionally scoped to a thread.
func buildDraftDestinations(channelID, threadTs string) (string, error) {
	b, err := json.Marshal([]draftDestination{{ChannelID: channelID, ThreadTS: threadTs}})
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
		BaseRequest:     BaseRequest{Token: cl.token},
		ClientMsgID:     uuid.NewString(),
		Blocks:          string(blocks),
		Destinations:    dest,
		FileIDs:         "[]",
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
	return r.draftID(), nil
}
