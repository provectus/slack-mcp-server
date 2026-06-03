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
