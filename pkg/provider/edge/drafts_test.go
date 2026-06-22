package edge

import (
	"encoding/json"
	"net/url"
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
