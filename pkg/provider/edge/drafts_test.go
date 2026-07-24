package edge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestBuildDraftDestinations(t *testing.T) {
	t.Run("channel only omits empty thread_ts", func(t *testing.T) {
		got, err := buildDraftDestinations("C123", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := `[{"channel_id":"C123"}]`
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("includes thread_ts when set", func(t *testing.T) {
		got, err := buildDraftDestinations("C123", "1700000000.000100")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := `[{"channel_id":"C123","thread_ts":"1700000000.000100"}]`
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

func TestDraftsCreateResponseDraftID(t *testing.T) {
	topLevel := draftsCreateResponse{DraftID: "Dr1"}

	nested := draftsCreateResponse{}
	nested.Draft.ID = "Dr2"

	both := draftsCreateResponse{DraftID: "Dr1"}
	both.Draft.ID = "Dr2"

	cases := []struct {
		name string
		resp draftsCreateResponse
		want string
	}{
		{"top-level draft_id", topLevel, "Dr1"},
		{"nested draft.id", nested, "Dr2"},
		{"top-level wins", both, "Dr1"},
		{"both empty", draftsCreateResponse{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.resp.draftID(); got != tc.want {
				t.Errorf("draftID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPadDraftTS(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already seven digits", "1713300000.1234567", "1713300000.1234567"},
		{"short fraction padded", "1700000000.0001", "1700000000.0001000"},
		{"long fraction truncated", "1700000000.123456789", "1700000000.1234567"},
		{"no fraction unchanged", "1700000000", "1700000000"},
		{"empty fraction padded", "1700000000.", "1700000000.0000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := padDraftTS(tc.in); got != tc.want {
				t.Errorf("padDraftTS(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDraftsUpdateFormValues(t *testing.T) {
	dest, err := buildDraftDestinations("C123", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	form := draftsUpdateForm{
		BaseRequest:         BaseRequest{Token: "xoxc-test"},
		DraftID:             "Dr1",
		ClientLastUpdatedTS: padDraftTS("1700000000.0001"),
		ClientMsgID:         "11111111-1111-1111-1111-111111111111",
		Blocks:              `[{"type":"rich_text"}]`,
		Destinations:        dest,
		FileIDs:             "[]",
		IsFromComposer:      "true",
		DateScheduled:       "0",
		WebClientFields:     webclientReason(""),
	}
	v := values(form, true)
	for _, key := range []string{"token", "draft_id", "client_last_updated_ts", "client_msg_id", "blocks", "destinations", "file_ids"} {
		if v.Get(key) == "" {
			t.Errorf("expected form key %q to be set, got empty (all: %v)", key, url.Values(v))
		}
	}
	if got := v.Get("draft_id"); got != "Dr1" {
		t.Errorf("draft_id = %q, want %q", got, "Dr1")
	}
	if got := v.Get("client_last_updated_ts"); got != "1700000000.0001000" {
		t.Errorf("client_last_updated_ts = %q, want padded 7 digits", got)
	}
	if got := v.Get("blocks"); got != `[{"type":"rich_text"}]` {
		t.Errorf("blocks not passed through verbatim: %q", got)
	}
}

func TestDraftsListFormValues(t *testing.T) {
	// drafts.list must request only active drafts: it otherwise returns sent
	// drafts too and the server caps the page at 100, so an active draft can be
	// buried past the cap and missed.
	form := draftsListForm{
		BaseRequest:     BaseRequest{Token: "xoxc-test"},
		Limit:           100,
		IsActive:        true,
		WebClientFields: webclientReason(""),
	}
	v := values(form, true)
	if got := v.Get("is_active"); got != "true" {
		t.Errorf("is_active = %q, want \"true\" (all: %v)", got, url.Values(v))
	}
	if got := v.Get("limit"); got != "100" {
		t.Errorf("limit = %q, want \"100\"", got)
	}
}

func TestDraftsListResponseParsing(t *testing.T) {
	body := `{"ok":true,"drafts":[
		{"id":"Dr1","client_msg_id":"cm1","last_updated_ts":"1700000000.000100","date_scheduled":0,"is_sent":false,"is_deleted":false,"destinations":[{"channel_id":"C123"}]},
		{"id":"Dr2","client_msg_id":"cm2","last_updated_ts":"1700000001.000200","is_sent":true,"destinations":[{"channel_id":"C123","thread_ts":"1700000000.000001"}]}
	],"has_more":false}`

	var r draftsListResponse
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := r.validate("drafts.list"); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(r.Drafts) != 2 {
		t.Fatalf("got %d drafts, want 2", len(r.Drafts))
	}
	d0 := r.Drafts[0]
	if d0.ID != "Dr1" || d0.ClientMsgID != "cm1" || d0.LastUpdatedTS != "1700000000.000100" {
		t.Errorf("draft[0] decoded wrong: %+v", d0)
	}
	if len(d0.Destinations) != 1 || d0.Destinations[0].ChannelID != "C123" || d0.Destinations[0].ThreadTS != "" {
		t.Errorf("draft[0] destinations decoded wrong: %+v", d0.Destinations)
	}
	d1 := r.Drafts[1]
	if !d1.IsSent || d1.Destinations[0].ThreadTS != "1700000000.000001" {
		t.Errorf("draft[1] decoded wrong: %+v", d1)
	}
}

func TestUnitDraftsListResponseDecodesBody(t *testing.T) {
	// Recorded drafts.list entry shape (trimmed to one draft): includes fields
	// the Draft struct deliberately leaves undecoded (is_from_composer,
	// date_created, file_ids, team_id, user_id) to prove decoding tolerates
	// them.
	blocks := `[{"block_id":"kjNyw","elements":[{"elements":[{"text":"hello","type":"text"}],"type":"rich_text_section"}],"type":"rich_text"}]`
	body := `{"ok":true,"drafts":[{"id":"Dr1","client_msg_id":"cm-1","blocks":` + blocks + `,"destinations":[{"broadcast":false,"channel_id":"D0A8P7UEQV8","thread_ts":"1784730669.577499","user_ids":["U0A8SRKAB0C"]}],"file_ids":[],"is_sent":false,"is_deleted":false,"is_from_composer":true,"date_created":1784730000,"date_scheduled":0,"last_updated_ts":"1784730669.1234567","last_updated_client":"Slack SSB Mac (Atom)","team_id":"T123","user_id":"U0A8SRKAB0C"}],"has_more":false}`

	var r draftsListResponse
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(r.Drafts) != 1 {
		t.Fatalf("got %d drafts, want 1", len(r.Drafts))
	}
	d := r.Drafts[0]
	if got := string(d.Blocks); got != blocks {
		t.Errorf("Blocks = %s, want %s", got, blocks)
	}
	if d.LastUpdatedClient != "Slack SSB Mac (Atom)" {
		t.Errorf("LastUpdatedClient = %q, want %q", d.LastUpdatedClient, "Slack SSB Mac (Atom)")
	}
}

// fakeHTTPClient satisfies the httpClient interface with a canned response.
type fakeHTTPClient struct {
	body string
}

func (f *fakeHTTPClient) Do(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     make(http.Header),
	}, nil
}

func TestUnitDraftsUpdateConflictError(t *testing.T) {
	newClient := func(body string) *Client {
		return &Client{
			cl:           &fakeHTTPClient{body: body},
			webclientAPI: "https://example.com/api/",
			token:        "xoxc-test",
		}
	}
	update := func(cl *Client) error {
		return cl.DraftsUpdate(context.Background(), "Dr1", "cm-1", "1700000000.0001", "C123", "", json.RawMessage(`[{"type":"rich_text"}]`))
	}

	t.Run("draft_has_conflict maps to ErrDraftConflict", func(t *testing.T) {
		err := update(newClient(`{"ok":false,"error":"draft_has_conflict"}`))
		if !errors.Is(err, ErrDraftConflict) {
			t.Fatalf("errors.Is(err, ErrDraftConflict) = false, err = %v", err)
		}
	})

	t.Run("decorated conflict string still maps to ErrDraftConflict", func(t *testing.T) {
		err := update(newClient(`{"ok":false,"error":"draft_has_conflict (edited elsewhere)"}`))
		if !errors.Is(err, ErrDraftConflict) {
			t.Fatalf("errors.Is(err, ErrDraftConflict) = false for decorated conflict string, err = %v", err)
		}
	})

	t.Run("unrelated error does not match", func(t *testing.T) {
		err := update(newClient(`{"ok":false,"error":"invalid_auth"}`))
		if err == nil {
			t.Fatal("expected an error")
		}
		if errors.Is(err, ErrDraftConflict) {
			t.Fatalf("errors.Is(err, ErrDraftConflict) = true for unrelated error %v", err)
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Err != "invalid_auth" {
			t.Errorf("unrelated error not preserved as *APIError: %v", err)
		}
	})

	t.Run("ok response returns nil", func(t *testing.T) {
		if err := update(newClient(`{"ok":true}`)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestDraftsCreateFormValues(t *testing.T) {
	form := draftsCreateForm{
		BaseRequest:     BaseRequest{Token: "xoxc-test"},
		ClientMsgID:     "11111111-1111-1111-1111-111111111111",
		Blocks:          `[{"type":"rich_text"}]`,
		Destinations:    `[{"channel_id":"C123"}]`,
		FileIDs:         "[]",
		IsFromComposer:  false,
		WebClientFields: webclientReason(""),
	}
	v := values(form, true)
	for _, key := range []string{"token", "client_msg_id", "blocks", "destinations", "file_ids"} {
		if v.Get(key) == "" {
			t.Errorf("expected form key %q to be set, got empty (all: %v)", key, url.Values(v))
		}
	}
	if v.Get("blocks") != `[{"type":"rich_text"}]` {
		t.Errorf("blocks not passed through verbatim: %q", v.Get("blocks"))
	}
	// destinations must be valid JSON
	var dest []map[string]any
	if err := json.Unmarshal([]byte(v.Get("destinations")), &dest); err != nil {
		t.Errorf("destinations not valid JSON: %v", err)
	}
}
