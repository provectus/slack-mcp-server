package handler

import (
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

// flattenRichText collects all plain text contained in a rich_text block,
// recursing through sections, lists, quotes and preformatted elements.
func flattenRichText(b *slack.RichTextBlock) string {
	var sb strings.Builder
	var sectionText = func(elems []slack.RichTextSectionElement) {
		for _, e := range elems {
			switch el := e.(type) {
			case *slack.RichTextSectionTextElement:
				sb.WriteString(el.Text)
			case *slack.RichTextSectionLinkElement:
				sb.WriteString(el.Text)
			}
		}
	}
	for _, el := range b.Elements {
		switch e := el.(type) {
		case *slack.RichTextSection:
			sectionText(e.Elements)
		case *slack.RichTextList:
			for _, item := range e.Elements {
				if s, ok := item.(*slack.RichTextSection); ok {
					sectionText(s.Elements)
					sb.WriteString("\n")
				}
			}
		case *slack.RichTextQuote:
			sectionText(e.Elements)
		}
	}
	return sb.String()
}

// countEmptyTextElements recurses through a rich_text block and counts text
// elements whose Text is "". Slack's drafts API rejects any such element with
// "invalid_message", so the converter must never emit them.
func countEmptyTextElements(b *slack.RichTextBlock) int {
	n := 0
	var sectionElems = func(elems []slack.RichTextSectionElement) {
		for _, e := range elems {
			if te, ok := e.(*slack.RichTextSectionTextElement); ok && te.Text == "" {
				n++
			}
		}
	}
	for _, el := range b.Elements {
		switch e := el.(type) {
		case *slack.RichTextSection:
			sectionElems(e.Elements)
		case *slack.RichTextQuote:
			sectionElems(e.Elements)
		case *slack.RichTextPreformatted:
			sectionElems(e.Elements)
		case *slack.RichTextList:
			for _, item := range e.Elements {
				if s, ok := item.(*slack.RichTextSection); ok {
					sectionElems(s.Elements)
				}
			}
		}
	}
	return n
}

// Regression: markdown whose inline spans abut a line break (e.g. an emphasis
// span immediately followed by a newline) used to emit empty text runs, which
// Slack's drafts.create/drafts.update reject with "invalid_message".
func TestMarkdownToRichTextBlock_NoEmptyTextElements(t *testing.T) {
	input := ":sparkles: *New: `conversations_draft_message` now upserts drafts*\n" +
		"Editing a draft actually updates it now instead of silently piling up duplicates.\n" +
		"• Lists existing drafts and replaces the one targeting the same *channel/thread* in place\n" +
		":point_right: Pull `feat/conversations-draft-upsert` to try it."

	rtb, err := markdownToRichTextBlock(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := countEmptyTextElements(rtb); got != 0 {
		t.Fatalf("expected no empty text elements, got %d", got)
	}
	// Content must still survive the empty-run filtering.
	if missing := draftContentLoss(input, rtb); len(missing) > 0 {
		t.Fatalf("content dropped after filtering empty runs: %v", missing)
	}
}

func TestMarkdownToRichTextBlock_PreservesAllContent(t *testing.T) {
	input := ":provectus: **AWOS v1.3.1 is out** — [release notes](https://example.com/x) — plugin unchanged.\n\n" +
		"1. **Testing & regression, first-class.** New QA slice. (#109)\n" +
		"2. **Screenshots you can actually reuse.** Saved to `docs/screenshots/`.\n\n" +
		":pray: Thanks to **Aleksandr Makarov**.\n\n" +
		"**Update:** `npx @provectusinc/awos`\n\n" +
		":point_right: **Please pull it and run a feature.**"

	rtb, err := markdownToRichTextBlock(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Must be a single rich_text block (the only type drafts.create accepts).
	if rtb.Type != slack.MBTRichText {
		t.Fatalf("expected rich_text block, got %q", rtb.Type)
	}

	flat := flattenRichText(rtb)

	// Every paragraph that ConvertMarkdownTextToBlocks dropped (as section blocks)
	// must now be present.
	for _, want := range []string{
		":provectus:", "AWOS v1.3.1 is out", "release notes", "plugin unchanged",
		"Testing & regression, first-class.", "Screenshots you can actually reuse.",
		":pray:", "Aleksandr Makarov", "Update:", "npx @provectusinc/awos",
		":point_right:", "Please pull it and run a feature.",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("flattened rich_text is missing %q\nfull text:\n%s", want, flat)
		}
	}

	// The numbered list must be preserved as a real rich_text_list, not flattened away.
	var hasList bool
	for _, el := range rtb.Elements {
		if _, ok := el.(*slack.RichTextList); ok {
			hasList = true
		}
	}
	if !hasList {
		t.Errorf("expected a rich_text_list element for the numbered list")
	}
}

// sectionText concatenates the text of a single rich_text_section's elements.
func sectionText(s *slack.RichTextSection) string {
	var sb strings.Builder
	for _, e := range s.Elements {
		switch el := e.(type) {
		case *slack.RichTextSectionTextElement:
			sb.WriteString(el.Text)
		case *slack.RichTextSectionLinkElement:
			sb.WriteString(el.Text)
		}
	}
	return sb.String()
}

// Consecutive paragraphs merge into a single section joined by a blank line, so
// the draft composer (which spaces top-level elements itself) renders exactly
// one empty line between them instead of doubling it.
func TestMarkdownToRichTextBlock_MergesParagraphs(t *testing.T) {
	rtb, err := markdownToRichTextBlock("First.\n\nSecond.\n\nThird.")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rtb.Elements) != 1 {
		t.Fatalf("expected paragraphs merged into one section, got %d elements", len(rtb.Elements))
	}
	sec, ok := rtb.Elements[0].(*slack.RichTextSection)
	if !ok {
		t.Fatalf("expected a rich_text_section, got %T", rtb.Elements[0])
	}
	if got := sectionText(sec); got != "First.\n\nSecond.\n\nThird." {
		t.Fatalf("expected paragraphs joined by blank lines, got %q", got)
	}
}

func TestMarkdownToRichTextBlock_ParagraphThenList(t *testing.T) {
	rtb, err := markdownToRichTextBlock("Intro paragraph.\n\n1. first item\n2. second item")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	listIdx := -1
	for i, el := range rtb.Elements {
		if _, ok := el.(*slack.RichTextList); ok {
			listIdx = i
		}
	}
	if listIdx <= 0 {
		t.Fatalf("expected a list preceded by the paragraph, list index=%d", listIdx)
	}
	// The element before the list is the paragraph's own section (no separator
	// section); the composer supplies spacing between top-level elements.
	prev, ok := rtb.Elements[listIdx-1].(*slack.RichTextSection)
	if !ok || sectionText(prev) != "Intro paragraph." {
		t.Fatalf("expected paragraph section before list, got %T %q", rtb.Elements[listIdx-1], sectionTextOrEmpty(rtb.Elements[listIdx-1]))
	}
}

func TestMarkdownToRichTextBlock_ListThenParagraph(t *testing.T) {
	rtb, err := markdownToRichTextBlock("1. one\n2. two\n\nAfter the list.")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	listIdx := -1
	for i, el := range rtb.Elements {
		if _, ok := el.(*slack.RichTextList); ok {
			listIdx = i
		}
	}
	if listIdx < 0 || listIdx+1 >= len(rtb.Elements) {
		t.Fatalf("expected a list followed by the paragraph, list index=%d", listIdx)
	}
	next, ok := rtb.Elements[listIdx+1].(*slack.RichTextSection)
	if !ok || sectionText(next) != "After the list." {
		t.Fatalf("expected paragraph section after list, got %T %q", rtb.Elements[listIdx+1], sectionTextOrEmpty(rtb.Elements[listIdx+1]))
	}
}

func sectionTextOrEmpty(el slack.RichTextElement) string {
	if s, ok := el.(*slack.RichTextSection); ok {
		return sectionText(s)
	}
	return ""
}

func TestMarkdownToRichTextBlock_NestedAndLooseLists(t *testing.T) {
	input := "- parent item\n  - nested child item\n\n1. first paragraph\n\n   second paragraph\n2. second item"
	rtb, err := markdownToRichTextBlock(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No content may be dropped (nested child + loose second paragraph).
	if missing := draftContentLoss(input, rtb); len(missing) > 0 {
		t.Fatalf("nested/loose list content was dropped: %v", missing)
	}
	// The nested bullet must produce an indented rich_text_list.
	var maxIndent int
	for _, el := range rtb.Elements {
		if l, ok := el.(*slack.RichTextList); ok && l.Indent > maxIndent {
			maxIndent = l.Indent
		}
	}
	if maxIndent < 1 {
		t.Errorf("expected a nested list with indent >= 1, got max indent %d", maxIndent)
	}
}

func TestDraftContentLoss_PassesForFullInput(t *testing.T) {
	input := ":provectus: **AWOS v1.3.1** — [release notes](https://example.com/x) — done.\n\n" +
		"1. **Testing** first-class. (#109)\n2. Screenshots to `docs/screenshots/`.\n\n" +
		":pray: Thanks **Aleksandr Makarov**."
	rtb, err := markdownToRichTextBlock(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if missing := draftContentLoss(input, rtb); len(missing) > 0 {
		t.Fatalf("expected no content loss, but these words were missing: %v", missing)
	}
}

func TestDraftContentLoss_DetectsMissingWord(t *testing.T) {
	// A rich_text block that is missing the word "gamma" from the input.
	rtb := &slack.RichTextBlock{
		Type: slack.MBTRichText,
		Elements: []slack.RichTextElement{
			&slack.RichTextSection{
				Type: slack.RTESection,
				Elements: []slack.RichTextSectionElement{
					&slack.RichTextSectionTextElement{Type: slack.RTSEText, Text: "alpha beta"},
				},
			},
		},
	}
	missing := draftContentLoss("alpha beta gamma", rtb)
	if len(missing) != 1 || missing[0] != "gamma" {
		t.Fatalf("expected [gamma], got %v", missing)
	}
}

func TestMarkdownToRichTextBlock_BoldAndLinkStyles(t *testing.T) {
	rtb, err := markdownToRichTextBlock("Hello **bold** and [link](https://example.com).")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var foundBold, foundLink bool
	var walk func(elems []slack.RichTextSectionElement)
	walk = func(elems []slack.RichTextSectionElement) {
		for _, e := range elems {
			switch el := e.(type) {
			case *slack.RichTextSectionTextElement:
				if el.Style != nil && el.Style.Bold && strings.Contains(el.Text, "bold") {
					foundBold = true
				}
			case *slack.RichTextSectionLinkElement:
				if el.URL == "https://example.com" {
					foundLink = true
				}
			}
		}
	}
	for _, el := range rtb.Elements {
		if s, ok := el.(*slack.RichTextSection); ok {
			walk(s.Elements)
		}
	}
	if !foundBold {
		t.Error("expected a bold-styled text element for **bold**")
	}
	if !foundLink {
		t.Error("expected a link element with the correct URL")
	}
}
