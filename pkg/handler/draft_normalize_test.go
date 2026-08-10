package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// marshalDraftBlocks marshals a converter-produced rich_text block into the
// JSON array shape the drafts API sends, i.e. what the handler would submit.
func marshalDraftBlocks(t *testing.T, block *slack.RichTextBlock) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal([]slack.Block{block})
	require.NoError(t, err)
	return raw
}

// simulateSlackEcho reproduces the two artifacts Slack adds when echoing draft
// blocks back from drafts.list (live finding, technical-considerations §2.3):
// a server-generated block_id injected into each top-level block, and JSON
// object keys in a different order. Key reordering is simulated by
// re-serialising every object with its keys reverse-sorted — the opposite of
// the canonical ascending order encoding/json produces.
func simulateSlackEcho(t *testing.T, sent json.RawMessage) json.RawMessage {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(sent)))
	dec.UseNumber()
	var blocks []any
	require.NoError(t, dec.Decode(&blocks))

	for i, b := range blocks {
		obj, ok := b.(map[string]any)
		require.True(t, ok, "top-level block %d is not an object", i)
		obj["block_id"] = fmt.Sprintf("Blk%02d", i)
	}

	var sb strings.Builder
	writeReversedKeys(t, &sb, blocks)
	return json.RawMessage(sb.String())
}

// writeReversedKeys serialises a decoded JSON value with every object's keys
// in reverse-sorted order, producing valid JSON with non-canonical key order.
func writeReversedKeys(t *testing.T, sb *strings.Builder, v any) {
	t.Helper()
	switch tv := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(tv))
		for k := range tv {
			keys = append(keys, k)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(keys)))
		sb.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				sb.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			require.NoError(t, err)
			sb.Write(kb)
			sb.WriteByte(':')
			writeReversedKeys(t, sb, tv[k])
		}
		sb.WriteByte('}')
	case []any:
		sb.WriteByte('[')
		for i, el := range tv {
			if i > 0 {
				sb.WriteByte(',')
			}
			writeReversedKeys(t, sb, el)
		}
		sb.WriteByte(']')
	default:
		b, err := json.Marshal(tv) // json.Number re-marshals its literal
		require.NoError(t, err)
		sb.Write(b)
	}
}

// TestUnitNormalizeDraftBlocksRoundTrip covers every formatting kind
// markdownToRichTextBlock can emit: normalising what we sent and normalising
// the simulated Slack echo (injected block_id + shuffled key order) must be
// byte-identical, and normalisation must be idempotent. This equality is what
// the calling agent uses to decide a draft is its own untouched work, so a
// regression here either breaks consent (too aggressive) or causes needless
// re-asking (too conservative).
func TestUnitNormalizeDraftBlocksRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		markdown string
	}{
		{"bold", "some **bold** words"},
		{"italic", "some *italic* words"},
		{"bold italic", "***both styles*** here"},
		{"inline code", "run `make test` before reporting"},
		{"fenced code", "```\nfunc main() {\n\tprintln(1)\n}\n```"},
		{"ordered list", "1. first item\n2. second item"},
		{"unordered list", "- alpha\n- beta"},
		{"nested list", "- top level\n  - nested item\n- back at top"},
		{"quote", "> quoted wisdom\n> second line"},
		{"link", "[the label](https://example.com/path)"},
		{"autolink", "see https://example.com/auto for details"},
		{"heading", "# Section Title\n\nbody paragraph"},
		{"mixed document", "# Title\n\nIntro with **bold** and `code`.\n\n- item one\n- item two\n\n> a quote\n\n```\ncode block\n```"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			block, err := markdownToRichTextBlock(tt.markdown)
			require.NoError(t, err)
			sent := marshalDraftBlocks(t, block)

			normSent, err := normalizeDraftBlocks(sent)
			require.NoError(t, err)
			normEcho, err := normalizeDraftBlocks(simulateSlackEcho(t, sent))
			require.NoError(t, err)

			assert.Equal(t, string(normSent), string(normEcho),
				"normalised sent blocks must match normalised Slack echo")
			assert.NotContains(t, string(normEcho), "block_id")

			again, err := normalizeDraftBlocks(normSent)
			require.NoError(t, err)
			assert.Equal(t, string(normSent), string(again), "normalisation must be idempotent")
		})
	}
}

// TestUnitNormalizeDraftBlocksLiveGroundTruth pins normalisation to the exact
// sent/echoed pair captured from live Slack during the investigation.
func TestUnitNormalizeDraftBlocksLiveGroundTruth(t *testing.T) {
	sent := json.RawMessage(`[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"content probe: exact round trip test"}]}]}]`)
	got := json.RawMessage(`[{"block_id":"fzjvI","elements":[{"elements":[{"text":"content probe: exact round trip test","type":"text"}],"type":"rich_text_section"}],"type":"rich_text"}]`)

	normSent, err := normalizeDraftBlocks(sent)
	require.NoError(t, err)
	normGot, err := normalizeDraftBlocks(got)
	require.NoError(t, err)
	assert.Equal(t, string(normSent), string(normGot))
}

// TestUnitNormalizeDraftBlocksPreservesForeignElements guards the property that
// makes restoring a human-authored draft lossless: element types this
// server's converter never emits — user mentions, channel mentions, emoji,
// unknown future types — survive normalisation with only block_id (at any
// depth) and key order removed. Everything else, including unknown nested
// fields and exact number literals, is preserved verbatim.
func TestUnitNormalizeDraftBlocksPreservesForeignElements(t *testing.T) {
	input := json.RawMessage(`[{"block_id":"AbC12","type":"rich_text","elements":[{"block_id":"nested1","type":"rich_text_section","elements":[{"type":"user","user_id":"U0123456"},{"type":"text","text":" said "},{"type":"channel","channel_id":"C0999"},{"type":"emoji","name":"tada","unicode":"1f389"},{"type":"future_widget","payload":{"z":1,"a":"keep","flags":[true,null,2.50]},"block_id":"deep"}]}]}]`)

	norm, err := normalizeDraftBlocks(input)
	require.NoError(t, err)

	// block_id is removed at every depth, nothing more.
	assert.NotContains(t, string(norm), "block_id")

	// Deep equality against the input with block_ids stripped proves every
	// other field survived. Both sides decode with UseNumber so the exact
	// number literal (2.50, not 2.5) is part of the comparison.
	decode := func(raw json.RawMessage) []any {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		var v []any
		require.NoError(t, dec.Decode(&v))
		return v
	}
	want := decode(input)
	for _, b := range want {
		stripBlockIDs(b)
	}
	assert.Equal(t, want, decode(norm))
	assert.Contains(t, string(norm), "2.50", "number literals must be preserved verbatim")
	assert.Contains(t, string(norm), "future_widget")

	// The echo simulation (more block_ids, shuffled keys) still normalises to
	// the same bytes, and normalisation stays idempotent.
	normEcho, err := normalizeDraftBlocks(simulateSlackEcho(t, input))
	require.NoError(t, err)
	assert.Equal(t, string(norm), string(normEcho))

	again, err := normalizeDraftBlocks(norm)
	require.NoError(t, err)
	assert.Equal(t, string(norm), string(again))
}

// TestUnitNormalizeDraftBlocksMalformed asserts malformed input yields an error,
// never a panic and never fabricated output.
func TestUnitNormalizeDraftBlocksMalformed(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"not json", "definitely not json"},
		{"empty input", ""},
		{"object not array", `{"type":"rich_text"}`},
		{"null", "null"},
		{"string", `"[]"`},
		{"trailing data", "[] []"},
		{"truncated array", `[{"type":"rich_text"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := normalizeDraftBlocks(json.RawMessage(tt.raw))
			assert.Error(t, err)
			assert.Nil(t, out)
		})
	}
}

// TestUnitRenderDraftBlocksTextStructure checks the display rendering carries the
// structural hints: paragraph separation, list markers, quote prefixes and
// code fences. The rendering is display-only; these assertions are about
// readability, not fidelity.
func TestUnitRenderDraftBlocksTextStructure(t *testing.T) {
	markdown := "First paragraph with **bold**.\n\n- alpha\n- beta\n\n1. one\n2. two\n\n> a quote\n\n```\ncode line\n```\n\nSee [docs](https://example.com/docs) and https://example.com/auto"
	block, err := markdownToRichTextBlock(markdown)
	require.NoError(t, err)
	raw := marshalDraftBlocks(t, block)

	text := renderDraftBlocksText(context.Background(), raw, nil)

	assert.Contains(t, text, "First paragraph with bold.")
	assert.Contains(t, text, "First paragraph with bold.\n\n- alpha",
		"top-level elements must be separated by a blank line — Slack renders them as separate paragraphs")
	assert.Contains(t, text, "- alpha")
	assert.Contains(t, text, "- beta")
	assert.Contains(t, text, "1. one")
	assert.Contains(t, text, "2. two")
	assert.Contains(t, text, "> a quote")
	assert.Contains(t, text, "```\ncode line\n```")
	assert.Contains(t, text, "docs (https://example.com/docs)")
	assert.Contains(t, text, "https://example.com/auto")
}

// TestUnitRenderDraftBlocksTextMentions checks best-effort rendering of element
// types the markdown converter never emits but human drafts contain. With no
// namer at all every mention degrades to its canonical literal — visible, never
// omitted.
func TestUnitRenderDraftBlocksTextMentions(t *testing.T) {
	raw := json.RawMessage(`[{"type":"rich_text","block_id":"x","elements":[{"type":"rich_text_section","elements":[{"type":"user","user_id":"U0123456"},{"type":"text","text":" ping "},{"type":"channel","channel_id":"C0999"},{"type":"emoji","name":"tada"},{"type":"broadcast","range":"here"}]}]}]`)

	text := renderDraftBlocksText(context.Background(), raw, nil)

	assert.Contains(t, text, "<@U0123456>")
	assert.Contains(t, text, " ping ")
	assert.Contains(t, text, "<#C0999>")
	assert.Contains(t, text, ":tada:")
	assert.Contains(t, text, "@here")
}

// stubMentionNamer resolves mentions from a fixed table and records every
// lookup it is asked for, so tests can assert both the rendered name and that
// nothing was looked up for a reference class that must never be resolved.
type stubMentionNamer struct {
	names   map[string]string // canonical literal -> display name
	lookups []string
}

func (s *stubMentionNamer) Name(_ context.Context, ref mentionRef) string {
	lit := ref.canonicalLiteral()
	s.lookups = append(s.lookups, lit)
	return s.names[lit]
}

// TestUnitRenderDraftBlocksTextNamesMentions is the AC 2.5 readback: a draft's
// user, channel and user-group mentions are shown by name, so the assistant can
// tell the user who it tagged instead of reciting raw codes.
func TestUnitRenderDraftBlocksTextNamesMentions(t *testing.T) {
	namer := &stubMentionNamer{names: map[string]string{
		"<@U0123456>":         "@dana",
		"<#C0999>":            "#general",
		"<!subteam^S0123ABC>": "@eng",
	}}
	raw := json.RawMessage(`[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"user","user_id":"U0123456"},{"type":"text","text":" please post in "},{"type":"channel","channel_id":"C0999"},{"type":"text","text":" and cc "},{"type":"usergroup","usergroup_id":"S0123ABC"}]}]}]`)

	text := renderDraftBlocksText(context.Background(), raw, namer)

	assert.Equal(t, "@dana please post in #general and cc @eng", text)
	assert.NotContains(t, text, "U0123456")
	assert.NotContains(t, text, "C0999")
	assert.NotContains(t, text, "S0123ABC")
	assert.Equal(t, []string{"<@U0123456>", "<#C0999>", "<!subteam^S0123ABC>"}, namer.lookups)
}

// TestUnitRenderDraftBlocksTextNamesMentionsInStructures checks mentions resolve
// wherever a draft can carry them, not just in a bare paragraph: inside a list
// item, a quote and a preformatted block.
func TestUnitRenderDraftBlocksTextNamesMentionsInStructures(t *testing.T) {
	namer := &stubMentionNamer{names: map[string]string{
		"<@U0123456>": "@dana",
		"<#C0999>":    "#general",
	}}
	raw := json.RawMessage(`[{"type":"rich_text","elements":[` +
		`{"type":"rich_text_list","style":"bullet","indent":0,"elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"ask "},{"type":"user","user_id":"U0123456"}]}]},` +
		`{"type":"rich_text_quote","elements":[{"type":"text","text":"see "},{"type":"channel","channel_id":"C0999"}]}` +
		`]}]`)

	text := renderDraftBlocksText(context.Background(), raw, namer)

	assert.Contains(t, text, "- ask @dana")
	assert.Contains(t, text, "> see #general")
}

// TestUnitRenderDraftBlocksTextUnknownMentionFallsBack is the other half of AC
// 2.5: a mention the directory cannot name must still be visible. Falling back
// to the canonical literal keeps a wrongly-tagged reference reviewable; dropping
// it would make the mistake invisible to both assistant and user.
func TestUnitRenderDraftBlocksTextUnknownMentionFallsBack(t *testing.T) {
	// Empty table: every lookup misses, as a deactivated user or a workspace
	// that could not be consulted would.
	namer := &stubMentionNamer{names: map[string]string{}}
	raw := json.RawMessage(`[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"cc "},{"type":"user","user_id":"U0BOGUS1"},{"type":"text","text":" "},{"type":"channel","channel_id":"C0BOGUS1"},{"type":"text","text":" "},{"type":"usergroup","usergroup_id":"S0BOGUS1"}]}]}]`)

	text := renderDraftBlocksText(context.Background(), raw, namer)

	assert.Equal(t, "cc <@U0BOGUS1> <#C0BOGUS1> <!subteam^S0BOGUS1>", text)
}

// TestUnitRenderDraftBlocksTextBroadcastUnchanged pins the readback of a
// human-authored draft's broadcast element. The converter never constructs one
// (a broadcast becomes inert text), but a draft the user typed by hand can carry
// it, and reading such a draft back before deciding whether to overwrite it is
// exactly the provenance-check flow the draft tool documents. It renders as
// "@here" without consulting the namer at all — a broadcast has no directory
// entry, and resolving one must never cost a lookup.
func TestUnitRenderDraftBlocksTextBroadcastUnchanged(t *testing.T) {
	namer := &stubMentionNamer{names: map[string]string{}}
	raw := json.RawMessage(`[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"broadcast","range":"here"},{"type":"text","text":" standup in 5"}]},{"type":"rich_text_section","elements":[{"type":"broadcast","range":"channel"},{"type":"text","text":" and "},{"type":"broadcast","range":"everyone"}]}]}]`)

	got := renderDraftBlocksText(context.Background(), raw, namer)

	assert.Equal(t, "@here standup in 5\n\n@channel and @everyone", got)
	assert.Empty(t, namer.lookups, "a broadcast must never be looked up in the mention directory")
}

// TestUnitNormalizeDraftBlocksGolden pins normalizeDraftBlocks to exact bytes.
//
// The readback now resolves mention names live, which makes its text unstable
// across calls by design. normalizeDraftBlocks is the opposite: it backs the
// lossless-restore contract, where displaced.blocks_json promises the
// overwritten draft can be put back exactly as it was, and the provenance
// comparison that decides whether a draft is the agent's own untouched work.
// Neither survives a change in these bytes. Mention-bearing blocks are included
// precisely because this feature touches mentions everywhere else — the
// authoritative blocks path must not move with them.
func TestUnitNormalizeDraftBlocksGolden(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "plain section",
			raw:  `[{"block_id":"B1","type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"hello"}]}]}]`,
			want: `[{"elements":[{"elements":[{"text":"hello","type":"text"}],"type":"rich_text_section"}],"type":"rich_text"}]`,
		},
		{
			name: "mentions preserved as elements, never as names",
			raw:  `[{"block_id":"B1","type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"user","user_id":"U0123456"},{"type":"text","text":" and "},{"type":"channel","channel_id":"C0999"},{"type":"text","text":" and "},{"type":"usergroup","usergroup_id":"S0123ABC"},{"type":"broadcast","range":"here"}]}]}]`,
			want: `[{"elements":[{"elements":[{"type":"user","user_id":"U0123456"},{"text":" and ","type":"text"},{"channel_id":"C0999","type":"channel"},{"text":" and ","type":"text"},{"type":"usergroup","usergroup_id":"S0123ABC"},{"range":"here","type":"broadcast"}],"type":"rich_text_section"}],"type":"rich_text"}]`,
		},
		{
			name: "styled run keeps its style object",
			raw:  `[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"bold","style":{"bold":true}}]}]}]`,
			want: `[{"elements":[{"elements":[{"style":{"bold":true},"text":"bold","type":"text"}],"type":"rich_text_section"}],"type":"rich_text"}]`,
		},
		{
			name: "list keeps style and indent",
			raw:  `[{"type":"rich_text","elements":[{"type":"rich_text_list","style":"bullet","indent":0,"elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"a"}]}]}]}]`,
			want: `[{"elements":[{"elements":[{"elements":[{"text":"a","type":"text"}],"type":"rich_text_section"}],"indent":0,"style":"bullet","type":"rich_text_list"}],"type":"rich_text"}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeDraftBlocks(json.RawMessage(tt.raw))
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got),
				"normalizeDraftBlocks output must not move: displaced.blocks_json promises an exact restore")
		})
	}
}

// TestUnitRenderDraftBlocksTextDegrades asserts the renderer never panics and
// falls back to the blocks_json placeholder when it cannot produce text.
func TestUnitRenderDraftBlocksTextDegrades(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"not json", "garbage"},
		{"empty input", ""},
		{"null", "null"},
		{"empty array", "[]"},
		{"unknown rich_text element only", `[{"type":"rich_text","elements":[{"type":"future_widget","payload":{"a":1}}]}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := renderDraftBlocksText(context.Background(), json.RawMessage(tt.raw), nil)
			assert.Equal(t, draftBlocksUnrenderable, text)
			assert.Contains(t, text, "blocks_json")
		})
	}
}
