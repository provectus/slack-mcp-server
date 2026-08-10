package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/slack-go/slack"
)

// draftBlocksUnrenderable is returned by renderDraftBlocksText when the blocks
// cannot be decoded or contain nothing renderable. It deliberately points the
// reader at blocks_json, the single authoritative representation.
const draftBlocksUnrenderable = "(draft content could not be rendered as text; consult blocks_json for the authoritative content)"

// normalizeDraftBlocks canonicalises a Slack blocks array so that content can
// be compared byte-for-byte and restored losslessly. Slack does not echo draft
// blocks back identically: it injects a server-generated block_id into each
// block and reorders JSON keys. Normalisation removes exactly those two
// artifacts — every block_id key is deleted (recursively, for future-proofing;
// today Slack injects it only at the top block level) and re-marshalling with
// encoding/json sorts map keys into canonical order. Everything else is
// preserved verbatim, including element types this server's markdown converter
// never emits (user/channel mentions, emoji, unknown future types) — that
// preservation is what makes restoring a human-authored draft lossless.
func normalizeDraftBlocks(raw json.RawMessage) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Keep number literals exactly as written (json.Number re-marshals the
	// original text), so normalisation never rewrites e.g. 2.50 as 2.5.
	dec.UseNumber()

	var blocks []any
	if err := dec.Decode(&blocks); err != nil {
		return nil, fmt.Errorf("draft blocks are not a JSON array: %w", err)
	}
	if blocks == nil {
		return nil, fmt.Errorf("draft blocks are not a JSON array: got null")
	}
	if dec.More() {
		return nil, fmt.Errorf("draft blocks contain trailing data after the array")
	}

	for _, b := range blocks {
		stripBlockIDs(b)
	}

	out, err := json.Marshal(blocks)
	if err != nil {
		return nil, fmt.Errorf("re-marshal normalised draft blocks: %w", err)
	}
	return out, nil
}

// parseVerbatimDraftBlocks validates and normalises caller-supplied verbatim
// block JSON (content_type application/json — the lossless restore path). The
// drafts composer accepts only rich_text blocks and silently drops anything
// else, so every element must declare "type": "rich_text"; anything else is
// rejected with an explicit error rather than left for Slack to lose.
func parseVerbatimDraftBlocks(raw json.RawMessage) (json.RawMessage, error) {
	normalized, err := normalizeDraftBlocks(raw)
	if err != nil {
		return nil, fmt.Errorf("text must be a JSON array of rich_text blocks (e.g. a blocks_json previously returned by this tool): %w", err)
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(normalized, &blocks); err != nil {
		return nil, fmt.Errorf("text must be a JSON array of rich_text block objects: %w", err)
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("text must contain at least one rich_text block; an empty array would produce an empty draft")
	}
	for i, b := range blocks {
		if b.Type != "rich_text" {
			return nil, fmt.Errorf("block %d has type %q; the drafts composer accepts only rich_text blocks and Slack would silently drop anything else", i, b.Type)
		}
	}
	return normalized, nil
}

// stripBlockIDs recursively deletes every "block_id" key from a decoded JSON
// value in place.
func stripBlockIDs(v any) {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "block_id")
		for _, val := range t {
			stripBlockIDs(val)
		}
	case []any:
		for _, val := range t {
			stripBlockIDs(val)
		}
	}
}

// renderDraftBlocksText renders a Slack blocks array into best-effort plain
// text with structural hints: blank lines between sections (paragraphs), "-"/"N." list
// markers, "> " quote prefixes and ``` fences around preformatted code.
//
// DISPLAY ONLY. This rendering is lossy by design (styling and unknown
// elements are flattened or dropped) and must never be used as a restore or
// comparison source — blocks_json is the authoritative content. On decode
// failure it degrades to a placeholder directing the reader there.
//
// Mentions make that doubly true. Names are resolved live against the
// workspace, so this output is NOT STABLE ACROSS CALLS: rename a user and two
// readbacks of the very same unmodified draft differ, and a lookup that fails
// once falls back to the canonical literal while the next succeeds with a name.
// Comparing two renderings therefore proves nothing about whether the draft
// changed, and re-submitting one would replace a live mention with the text of
// a name. The drafts provenance comparison uses blocks_json and must keep doing
// so.
func renderDraftBlocksText(ctx context.Context, raw json.RawMessage, namer mentionNamer) string {
	var blocks slack.Blocks
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return draftBlocksUnrenderable
	}

	var parts []string
	for _, b := range blocks.BlockSet {
		rtb, ok := b.(*slack.RichTextBlock)
		if !ok {
			continue
		}
		for _, el := range rtb.Elements {
			if s := renderRichTextElement(ctx, el, namer); s != "" {
				parts = append(parts, s)
			}
		}
	}
	if len(parts) == 0 {
		return draftBlocksUnrenderable
	}
	// Blank line between top-level elements: Slack renders separate sections
	// as separate paragraphs.
	return strings.Join(parts, "\n\n")
}

// renderRichTextElement renders one top-level rich_text element (section,
// list, quote or preformatted) into display text. Unknown element types
// render as empty — best effort, never an error.
func renderRichTextElement(ctx context.Context, el slack.RichTextElement, namer mentionNamer) string {
	switch e := el.(type) {
	case *slack.RichTextSection:
		return renderRichTextSectionElements(ctx, e.Elements, namer)

	case *slack.RichTextList:
		indent := strings.Repeat("  ", e.Indent)
		var lines []string
		for i, item := range e.Elements {
			marker := "-"
			if e.Style == slack.RTEListOrdered {
				marker = strconv.Itoa(i+1) + "."
			}
			body := ""
			if s, ok := item.(*slack.RichTextSection); ok {
				body = renderRichTextSectionElements(ctx, s.Elements, namer)
			}
			lines = append(lines, indent+marker+" "+body)
		}
		return strings.Join(lines, "\n")

	case *slack.RichTextQuote:
		var quoted []string
		for _, line := range strings.Split(renderRichTextSectionElements(ctx, e.Elements, namer), "\n") {
			quoted = append(quoted, "> "+line)
		}
		return strings.Join(quoted, "\n")

	case *slack.RichTextPreformatted:
		return "```\n" + strings.TrimRight(renderRichTextSectionElements(ctx, e.Elements, namer), "\n") + "\n```"

	default:
		return ""
	}
}

// renderRichTextSectionElements flattens section elements into display text,
// covering the element types Slack drafts carry in practice. Unknown element
// types are skipped — best effort, never an error.
func renderRichTextSectionElements(ctx context.Context, elems []slack.RichTextSectionElement, namer mentionNamer) string {
	var sb strings.Builder
	for _, e := range elems {
		switch el := e.(type) {
		case *slack.RichTextSectionTextElement:
			sb.WriteString(el.Text)
		case *slack.RichTextSectionLinkElement:
			if el.Text != "" && el.Text != el.URL {
				sb.WriteString(el.Text + " (" + el.URL + ")")
			} else {
				sb.WriteString(el.URL)
			}
		case *slack.RichTextSectionUserElement:
			sb.WriteString(renderMention(ctx, namer, mentionRef{Kind: mentionUser, ID: el.UserID}))
		case *slack.RichTextSectionChannelElement:
			sb.WriteString(renderMention(ctx, namer, mentionRef{Kind: mentionChannel, ID: el.ChannelID}))
		case *slack.RichTextSectionUserGroupElement:
			sb.WriteString(renderMention(ctx, namer, mentionRef{Kind: mentionUserGroup, ID: el.UsergroupID}))
		case *slack.RichTextSectionEmojiElement:
			sb.WriteString(":" + el.Name + ":")
		case *slack.RichTextSectionBroadcastElement:
			// Never constructed by this server's converter (a broadcast becomes
			// inert text), but a human-authored draft can carry one. It has no
			// directory entry, so it is rendered without consulting the namer:
			// canonicalLiteral already reads "@here" / "@channel" / "@everyone".
			sb.WriteString(mentionRef{Kind: mentionBroadcast, Range: el.Range}.canonicalLiteral())
		}
	}
	return sb.String()
}

// renderMention renders one mention by name, falling back to the canonical
// literal when the name cannot be determined — never to nothing. A reference
// the reader cannot see at all is worse than one shown as a raw code: the
// readback exists so the assistant can tell the user who it tagged, and a
// silently omitted mention would make a wrong tag invisible.
func renderMention(ctx context.Context, namer mentionNamer, ref mentionRef) string {
	if namer != nil {
		if name := namer.Name(ctx, ref); name != "" {
			return name
		}
	}
	return ref.canonicalLiteral()
}
