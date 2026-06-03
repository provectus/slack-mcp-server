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

func TestMarkdownToRichTextBlock_SeparatesParagraphAndList(t *testing.T) {
	rtb, err := markdownToRichTextBlock("Intro paragraph.\n\n1. first item\n2. second item")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Find the list and assert the element right before it is a newline separator,
	// so the paragraph does not glue onto the list.
	listIdx := -1
	for i, el := range rtb.Elements {
		if _, ok := el.(*slack.RichTextList); ok {
			listIdx = i
		}
	}
	if listIdx <= 0 {
		t.Fatalf("expected a list preceded by other elements, list index=%d", listIdx)
	}
	prev, ok := rtb.Elements[listIdx-1].(*slack.RichTextSection)
	if !ok || len(prev.Elements) != 1 {
		t.Fatalf("expected a separator section before the list, got %T", rtb.Elements[listIdx-1])
	}
	// Paragraph precedes the list (a non-self-breaking block), so a blank-line
	// separator ("\n\n") is needed to render an empty line before the list.
	if te, ok := prev.Elements[0].(*slack.RichTextSectionTextElement); !ok || te.Text != "\n\n" {
		t.Fatalf("expected blank-line separator before list, got %+v", prev.Elements[0])
	}
}

func TestMarkdownToRichTextBlock_SingleBreakAfterList(t *testing.T) {
	// list -> paragraph: the list already emits a trailing newline, so the
	// separator must be a single "\n" to avoid a double blank line.
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
		t.Fatalf("expected a list followed by more elements, list index=%d", listIdx)
	}
	sep, ok := rtb.Elements[listIdx+1].(*slack.RichTextSection)
	if !ok || len(sep.Elements) != 1 {
		t.Fatalf("expected a separator section after the list, got %T", rtb.Elements[listIdx+1])
	}
	if te, ok := sep.Elements[0].(*slack.RichTextSectionTextElement); !ok || te.Text != "\n" {
		t.Fatalf("expected single-newline separator after list, got %+v", sep.Elements[0])
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
