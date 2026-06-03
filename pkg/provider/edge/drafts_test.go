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
