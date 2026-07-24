package handler

import (
	"bytes"
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
// DISPLAY ONLY. This rendering is lossy by design (styling, mention identity
// and unknown elements are flattened or dropped) and must never be used as a
// restore or comparison source — blocks_json is the authoritative content. On
// decode failure it degrades to a placeholder directing the reader there.
func renderDraftBlocksText(raw json.RawMessage) string {
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
			if s := renderRichTextElement(el); s != "" {
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
func renderRichTextElement(el slack.RichTextElement) string {
	switch e := el.(type) {
	case *slack.RichTextSection:
		return renderRichTextSectionElements(e.Elements)

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
				body = renderRichTextSectionElements(s.Elements)
			}
			lines = append(lines, indent+marker+" "+body)
		}
		return strings.Join(lines, "\n")

	case *slack.RichTextQuote:
		var quoted []string
		for _, line := range strings.Split(renderRichTextSectionElements(e.Elements), "\n") {
			quoted = append(quoted, "> "+line)
		}
		return strings.Join(quoted, "\n")

	case *slack.RichTextPreformatted:
		return "```\n" + strings.TrimRight(renderRichTextSectionElements(e.Elements), "\n") + "\n```"

	default:
		return ""
	}
}

// renderRichTextSectionElements flattens section elements into display text,
// covering the element types Slack drafts carry in practice. Unknown element
// types are skipped — best effort, never an error.
func renderRichTextSectionElements(elems []slack.RichTextSectionElement) string {
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
			sb.WriteString("<@" + el.UserID + ">")
		case *slack.RichTextSectionChannelElement:
			sb.WriteString("<#" + el.ChannelID + ">")
		case *slack.RichTextSectionEmojiElement:
			sb.WriteString(":" + el.Name + ":")
		case *slack.RichTextSectionBroadcastElement:
			sb.WriteString("@" + el.Range)
		}
	}
	return sb.String()
}
