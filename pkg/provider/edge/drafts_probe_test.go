//go:build liveprobe

// Live probes for the undocumented drafts.* edge API. Build-tagged so they
// never compile or run in CI or `make test`.
//
// Run with:
//
//	go test -tags liveprobe -v -run TestLiveDrafts ./pkg/provider/edge/
//
// Requires SLACK_MCP_XOXC_TOKEN and SLACK_MCP_XOXD_TOKEN (drafts need a
// browser-session token; bot tokens cannot reach the edge API). Each probe
// creates REAL drafts in the caller's own DM-with-self and deletes exactly the
// drafts it created on exit.
//
// SAFETY RULES — non-negotiable:
//   - Probes target the DM-to-self only. NEVER point them at a shared channel.
//   - Cleanup deletes ONLY drafts tracked by the ids drafts.create returned to
//     this probe run. Never sweep "all active drafts" — the user's own drafts
//     live in the same list, and destroying one is the exact bug the safe-draft
//     feature exists to prevent.
//
// What each probe establishes (ground truths unit tests cannot pin):
//
//	TestLiveDraftsUpsertStability — repeated list→match→update rounds against
//	    one destination converge on exactly ONE draft: drafts.list?is_active=true
//	    keeps returning a just-updated draft, so the destination upsert never
//	    forks a duplicate at machine speed.
//	TestLiveDraftsThreadedDestination — thread_ts inside the destinations
//	    payload survives drafts.create and is echoed back verbatim by
//	    drafts.list, i.e. thread targeting works.
//	TestLiveDraftsBlockNormalization — the live pin for
//	    pkg/handler/draft_normalize.go: Slack echoes back the blocks we sent
//	    with a server-generated block_id injected and JSON keys reordered, and
//	    that is the ONLY difference. If this probe fails, normalisation no
//	    longer removes exactly Slack's artifacts.
//	TestLiveDraftsConflictOnStaleToken — drafts.update with a stale
//	    client_last_updated_ts fails with the draft_has_conflict API error
//	    (mapped to edge.ErrDraftConflict) and the draft survives intact.
package edge

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/rusq/slackdump/v3/auth"
	"github.com/slack-go/slack"
)

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// liveClient builds an edge client from the session-token env vars and
// resolves the caller's DM-with-self channel — the only destination probes are
// allowed to touch. Skips the test when the tokens are absent.
func liveClient(t *testing.T) (*Client, string) {
	t.Helper()
	xoxc, xoxd := os.Getenv("SLACK_MCP_XOXC_TOKEN"), os.Getenv("SLACK_MCP_XOXD_TOKEN")
	if xoxc == "" || xoxd == "" {
		t.Skip("SLACK_MCP_XOXC_TOKEN / SLACK_MCP_XOXD_TOKEN not set")
	}
	prov, err := auth.NewValueAuth(xoxc, xoxd)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	cl, err := New(context.Background(), prov)
	if err != nil {
		t.Fatalf("edge client: %v", err)
	}

	// Resolve the DM-with-self channel via the standard Web API.
	hc, err := prov.HTTPClient()
	if err != nil {
		t.Fatalf("http client: %v", err)
	}
	api := slack.New(xoxc, slack.OptionHTTPClient(hc))
	at, err := api.AuthTest()
	if err != nil {
		t.Fatalf("auth test: %v", err)
	}
	ch, _, _, err := api.OpenConversation(&slack.OpenConversationParameters{Users: []string{at.UserID}})
	if err != nil {
		t.Fatalf("open self-DM: %v", err)
	}
	t.Logf("self user=%s self-DM channel=%s", at.UserID, ch.ID)
	return cl, ch.ID
}

// draftsDelete is probe-only cleanup; the shipped server has no delete path.
// drafts.delete requires client_last_updated_ts (padded), the same
// optimistic-concurrency token drafts.update takes — without it the call fails
// with "missing required field: client_last_updated_ts".
func draftsDelete(ctx context.Context, cl *Client, draftID, lastUpdatedTS string) error {
	form := url.Values{}
	form.Set("token", cl.token)
	form.Set("draft_id", draftID)
	form.Set("client_last_updated_ts", padDraftTS(lastUpdatedTS))
	resp, err := cl.PostForm(ctx, "drafts.delete", form)
	if err != nil {
		return err
	}
	var r baseResponse
	if err := cl.ParseResponse(&r, resp); err != nil {
		return err
	}
	return r.validate("drafts.delete")
}

// cleanupDrafts deletes ONLY the drafts whose ids this probe run received from
// drafts.create (tracked in *created; pointer so deferred calls see late
// appends). It re-lists first because drafts.delete requires each draft's
// CURRENT last_updated_ts. Never widen this to sweep the active list.
func cleanupDrafts(ctx context.Context, t *testing.T, cl *Client, created *[]string) {
	t.Helper()
	final, err := cl.DraftsList(ctx, 100)
	if err != nil {
		t.Logf("CLEANUP: could not re-list drafts: %v — remove %v by hand in Slack", err, *created)
		return
	}
	for _, id := range *created {
		d, ok := find(final, id)
		if !ok {
			continue
		}
		if err := draftsDelete(ctx, cl, id, d.LastUpdatedTS); err != nil {
			t.Logf("CLEANUP FAILED for %s: %v — remove it by hand in Slack", id, err)
		} else {
			t.Logf("cleaned up %s", id)
		}
	}
}

// blocksFor wraps a plain string in a single rich_text block, the shape the
// drafts composer stores.
func blocksFor(s string) json.RawMessage {
	b, _ := json.Marshal([]slack.Block{&slack.RichTextBlock{
		Type: slack.MBTRichText,
		Elements: []slack.RichTextElement{
			&slack.RichTextSection{
				Type:     slack.RTESection,
				Elements: []slack.RichTextSectionElement{&slack.RichTextSectionTextElement{Type: slack.RTSEText, Text: s}},
			},
		},
	}})
	return b
}

func find(ds []Draft, id string) (Draft, bool) {
	for _, d := range ds {
		if d.ID == id {
			return d, true
		}
	}
	return Draft{}, false
}

// findForDest mirrors handler.findDraftForDestination exactly, so the probes
// exercise the same matching the shipped upsert does. (The handler package
// cannot be imported here without a cycle.)
func findForDest(ds []Draft, channel, threadTs string) (Draft, bool) {
	norm := func(ts string) string {
		i := -1
		for k, c := range ts {
			if c == '.' {
				i = k
				break
			}
		}
		if i < 0 {
			return ts
		}
		frac := ts[i+1:]
		for len(frac) > 0 && frac[len(frac)-1] == '0' {
			frac = frac[:len(frac)-1]
		}
		if frac == "" {
			return ts[:i]
		}
		return ts[:i+1] + frac
	}
	want := norm(threadTs)
	for _, d := range ds {
		if d.IsSent || d.IsDeleted || d.DateScheduled != 0 {
			continue
		}
		for _, dest := range d.Destinations {
			if dest.ChannelID == channel && norm(dest.ThreadTS) == want {
				return d, true
			}
		}
	}
	return Draft{}, false
}

func dump(t *testing.T, label string, ds []Draft) {
	t.Helper()
	t.Logf("--- %s: %d active draft(s)", label, len(ds))
	for _, d := range ds {
		dj, _ := json.Marshal(d.Destinations)
		t.Logf("    id=%s client_msg_id=%q last_updated_ts=%q sent=%v deleted=%v sched=%d dest=%s",
			d.ID, d.ClientMsgID, d.LastUpdatedTS, d.IsSent, d.IsDeleted, d.DateScheduled, dj)
	}
}

// selfDMHistory returns recent message timestamps in the given self-DM, used
// to pick a thread parent.
func selfDMHistory(ctx context.Context, dm string) ([]string, error) {
	xoxc, xoxd := os.Getenv("SLACK_MCP_XOXC_TOKEN"), os.Getenv("SLACK_MCP_XOXD_TOKEN")
	prov, err := auth.NewValueAuth(xoxc, xoxd)
	if err != nil {
		return nil, err
	}
	hc, err := prov.HTTPClient()
	if err != nil {
		return nil, err
	}
	api := slack.New(xoxc, slack.OptionHTTPClient(hc))
	h, err := api.GetConversationHistory(&slack.GetConversationHistoryParameters{ChannelID: dm, Limit: 5})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range h.Messages {
		out = append(out, m.Timestamp)
	}
	return out, nil
}

// canonical rewrites a decoded JSON value so map key order is irrelevant,
// letting two payloads be compared for semantic rather than textual equality
// (encoding/json marshals maps with sorted keys).
func canonical(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range t {
			out[k] = canonical(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = canonical(val)
		}
		return out
	default:
		return v
	}
}

// stripBlockIDs recursively removes every block_id key from a decoded JSON
// value — the one field Slack injects server-side into stored draft blocks.
func stripBlockIDs(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range t {
			if k == "block_id" {
				continue
			}
			out[k] = stripBlockIDs(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = stripBlockIDs(val)
		}
		return out
	default:
		return v
	}
}

// ---------------------------------------------------------------------------
// probes
// ---------------------------------------------------------------------------

// TestLiveDraftsUpsertStability: repeated create→list→match→update rounds
// against one destination, exactly as the handler's upsert does them, converge
// on exactly one draft — a just-updated draft stays visible to
// drafts.list?is_active=true and the destination match keeps finding it.
func TestLiveDraftsUpsertStability(t *testing.T) {
	ctx := context.Background()
	cl, dm := liveClient(t)

	var created []string
	defer cleanupDrafts(ctx, t, cl, &created)

	id0, err := cl.DraftsCreate(ctx, dm, "", blocksFor("upsert probe round 0"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created = append(created, id0)
	t.Logf("round 0 created %s", id0)

	for round := 1; round <= 4; round++ {
		ds, err := cl.DraftsList(ctx, 100)
		if err != nil {
			t.Fatalf("round %d list: %v", round, err)
		}
		existing, found := findForDest(ds, dm, "")
		if !found {
			dump(t, "state at miss", ds)
			t.Fatalf("FINDING: round %d — findDraftForDestination MISSED; the handler would create a duplicate here", round)
		}
		if existing.ID != id0 {
			t.Errorf("FINDING: round %d matched a DIFFERENT draft %s (expected %s)", round, existing.ID, id0)
		}
		if err := cl.DraftsUpdate(ctx, existing.ID, existing.ClientMsgID, existing.LastUpdatedTS, dm, "", blocksFor("upsert probe round "+strconv.Itoa(round))); err != nil {
			t.Fatalf("round %d update failed: %v", round, err)
		}
		t.Logf("round %d: matched %s, updated ok (last_updated=%s)", round, existing.ID, existing.LastUpdatedTS)
	}

	after, err := cl.DraftsList(ctx, 100)
	if err != nil {
		t.Fatalf("final list: %v", err)
	}
	n := 0
	for _, d := range after {
		if d.IsSent || d.IsDeleted || d.DateScheduled != 0 {
			continue
		}
		for _, dest := range d.Destinations {
			if dest.ChannelID == dm && dest.ThreadTS == "" {
				n++
			}
		}
	}
	if n != 1 {
		dump(t, "final state", after)
		t.Errorf("FINDING: %d active channel-level draft(s) at the destination after 5 writes, want exactly 1", n)
	}
}

// TestLiveDraftsThreadedDestination: thread_ts inside the destinations payload
// survives drafts.create and is echoed back verbatim by drafts.list.
func TestLiveDraftsThreadedDestination(t *testing.T) {
	ctx := context.Background()
	cl, dm := liveClient(t)

	hist, err := selfDMHistory(ctx, dm)
	if err != nil || len(hist) == 0 {
		t.Skipf("no messages in self-DM (err=%v) — post any message to yourself and re-run", err)
	}
	parent := hist[0]
	t.Logf("threading against parent ts=%s", parent)

	var created []string
	defer cleanupDrafts(ctx, t, cl, &created)

	id, err := cl.DraftsCreate(ctx, dm, parent, blocksFor("threaded destination probe"))
	if err != nil {
		t.Fatalf("threaded create: %v", err)
	}
	created = append(created, id)

	ds, err := cl.DraftsList(ctx, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	d, ok := find(ds, id)
	switch {
	case !ok:
		t.Errorf("FINDING: threaded draft %s not returned by drafts.list", id)
	case len(d.Destinations) == 0:
		t.Errorf("FINDING: threaded draft %s has NO destinations", id)
	case d.Destinations[0].ThreadTS != parent:
		t.Errorf("FINDING: thread_ts did not round-trip — sent %q, got %q", parent, d.Destinations[0].ThreadTS)
	default:
		t.Logf("thread_ts round-tripped intact: %q", d.Destinations[0].ThreadTS)
	}
}

// TestLiveDraftsBlockNormalization: the live pin for
// pkg/handler/draft_normalize.go. Slack echoes stored blocks back with a
// server-generated block_id injected and JSON keys reordered; after removing
// block_id and canonicalising key order, the echo must be byte-identical to
// what was sent — proving those two artifacts are the ONLY difference.
func TestLiveDraftsBlockNormalization(t *testing.T) {
	ctx := context.Background()
	cl, dm := liveClient(t)

	sent := blocksFor("block normalization ground-truth probe")
	t.Logf("SENT blocks: %s", string(sent))

	var created []string
	defer cleanupDrafts(ctx, t, cl, &created)

	id, err := cl.DraftsCreate(ctx, dm, "", sent)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created = append(created, id)

	ds, err := cl.DraftsList(ctx, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	d, ok := find(ds, id)
	if !ok {
		t.Fatalf("draft %s not found in drafts.list", id)
	}
	if len(d.Blocks) == 0 {
		t.Fatalf("FINDING: drafts.list did NOT return the draft body — the compare-then-consent design depends on it")
	}
	t.Logf("RETURNED blocks: %s", string(d.Blocks))

	var sentVal, gotVal any
	if err := json.Unmarshal(sent, &sentVal); err != nil {
		t.Fatalf("unmarshal sent: %v", err)
	}
	if err := json.Unmarshal(d.Blocks, &gotVal); err != nil {
		t.Fatalf("unmarshal returned: %v", err)
	}

	// encoding/json sorts map keys on marshal, so canonical() + Marshal yields
	// a canonical key order on both sides; stripBlockIDs removes the one field
	// Slack injects.
	ca, _ := json.Marshal(canonical(stripBlockIDs(sentVal)))
	cb, _ := json.Marshal(canonical(stripBlockIDs(gotVal)))
	if string(ca) != string(cb) {
		t.Errorf("FINDING: Slack MATERIALLY ALTERED the blocks beyond block_id injection and key order.\n  canonical sent=%s\n  canonical got =%s", ca, cb)
	} else {
		t.Logf("blocks identical after stripping block_id and canonicalising key order — normalisation ground truth holds")
	}
}

// TestLiveDraftsConflictOnStaleToken: drafts.update carrying a stale
// client_last_updated_ts is rejected with the draft_has_conflict API error
// (surfaced as edge.ErrDraftConflict) and the draft survives intact.
func TestLiveDraftsConflictOnStaleToken(t *testing.T) {
	ctx := context.Background()
	cl, dm := liveClient(t)

	var created []string
	defer cleanupDrafts(ctx, t, cl, &created)

	id, err := cl.DraftsCreate(ctx, dm, "", blocksFor("conflict probe v1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created = append(created, id)

	ds, err := cl.DraftsList(ctx, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	d, ok := find(ds, id)
	if !ok {
		t.Fatalf("draft %s not found after create", id)
	}

	stale := "1700000000.000000"
	err = cl.DraftsUpdate(ctx, id, d.ClientMsgID, stale, dm, "", blocksFor("conflict probe stale write"))
	if err == nil {
		t.Fatalf("FINDING: update with stale client_last_updated_ts SUCCEEDED — the concurrency token no longer guards writes")
	}
	if !errors.Is(err, ErrDraftConflict) {
		t.Errorf("FINDING: stale-token update failed with %v, want errors.Is(err, ErrDraftConflict) — the draft_has_conflict mapping no longer matches what Slack returns", err)
	} else {
		t.Logf("stale-token update rejected with ErrDraftConflict: %v", err)
	}

	after, err := cl.DraftsList(ctx, 100)
	if err != nil {
		t.Fatalf("re-list: %v", err)
	}
	if _, still := find(after, id); !still {
		t.Errorf("FINDING: the rejected update DROPPED draft %s from the active list — the draft did not survive the conflict", id)
	} else {
		t.Logf("draft %s survived the rejected write intact", id)
	}
}
