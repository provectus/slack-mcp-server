package handler

import (
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

// flattenRichText collects all plain text contained in a rich_text block,
// recursing through sections, lists, quotes and preformatted elements. Mention
// elements carry no text of their own, so each contributes its canonical
// literal; without these cases the helper silently under-reports.
func flattenRichText(b *slack.RichTextBlock) string {
	var sb strings.Builder
	var sectionText = func(elems []slack.RichTextSectionElement) {
		for _, e := range elems {
			switch el := e.(type) {
			case *slack.RichTextSectionTextElement:
				sb.WriteString(el.Text)
			case *slack.RichTextSectionLinkElement:
				sb.WriteString(el.Text)
			case *slack.RichTextSectionUserElement:
				sb.WriteString(mentionRef{Kind: mentionUser, ID: el.UserID}.canonicalLiteral())
			case *slack.RichTextSectionChannelElement:
				sb.WriteString(mentionRef{Kind: mentionChannel, ID: el.ChannelID}.canonicalLiteral())
			case *slack.RichTextSectionUserGroupElement:
				sb.WriteString(mentionRef{Kind: mentionUserGroup, ID: el.UsergroupID}.canonicalLiteral())
			case *slack.RichTextSectionBroadcastElement:
				sb.WriteString(mentionRef{Kind: mentionBroadcast, Range: el.Range}.canonicalLiteral())
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
func TestUnitMarkdownToRichTextBlock_NoEmptyTextElements(t *testing.T) {
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

func TestUnitMarkdownToRichTextBlock_PreservesAllContent(t *testing.T) {
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

// sectionText concatenates the text of a single rich_text_section's elements,
// rendering mention elements as their canonical literal.
func sectionText(s *slack.RichTextSection) string {
	var sb strings.Builder
	for _, e := range s.Elements {
		switch el := e.(type) {
		case *slack.RichTextSectionTextElement:
			sb.WriteString(el.Text)
		case *slack.RichTextSectionLinkElement:
			sb.WriteString(el.Text)
		case *slack.RichTextSectionUserElement:
			sb.WriteString(mentionRef{Kind: mentionUser, ID: el.UserID}.canonicalLiteral())
		case *slack.RichTextSectionChannelElement:
			sb.WriteString(mentionRef{Kind: mentionChannel, ID: el.ChannelID}.canonicalLiteral())
		case *slack.RichTextSectionUserGroupElement:
			sb.WriteString(mentionRef{Kind: mentionUserGroup, ID: el.UsergroupID}.canonicalLiteral())
		case *slack.RichTextSectionBroadcastElement:
			sb.WriteString(mentionRef{Kind: mentionBroadcast, Range: el.Range}.canonicalLiteral())
		}
	}
	return sb.String()
}

// Consecutive paragraphs merge into a single section joined by a blank line, so
// the draft composer (which spaces top-level elements itself) renders exactly
// one empty line between them instead of doubling it.
func TestUnitMarkdownToRichTextBlock_MergesParagraphs(t *testing.T) {
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

func TestUnitMarkdownToRichTextBlock_ParagraphThenList(t *testing.T) {
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

func TestUnitMarkdownToRichTextBlock_ListThenParagraph(t *testing.T) {
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

func TestUnitMarkdownToRichTextBlock_NestedAndLooseLists(t *testing.T) {
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

func TestUnitDraftContentLoss_PassesForFullInput(t *testing.T) {
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

func TestUnitDraftContentLoss_DetectsMissingWord(t *testing.T) {
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

// A link whose label is itself formatted (bold/code) must keep its label text,
// otherwise draftContentLoss sees the words as dropped and the handler refuses
// the whole draft.
func TestUnitMarkdownToRichTextBlock_FormattedLinkLabelNoContentLoss(t *testing.T) {
	for _, input := range []string{
		"See [**bold label**](https://example.com) here.",
		"Run [the `code` cmd](https://example.com/x) now.",
	} {
		t.Run(input, func(t *testing.T) {
			rtb, err := markdownToRichTextBlock(input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if missing := draftContentLoss(input, rtb); len(missing) > 0 {
				t.Fatalf("formatted link label dropped content: %v", missing)
			}
		})
	}
}

// A list written with "•" — the form the assistant uses most — must become a
// real rich_text_list, and the blank line that separates the intro from the
// list in the source must not survive as a "\n\n" text run: flushInline emits
// the intro as its own section and the list becomes its own top-level element.
func TestUnitMarkdownToRichTextBlock_BulletCharIntroListOutro(t *testing.T) {
	input := "Intro:\n\n• a\n• b\n\nOutro."

	rtb, err := markdownToRichTextBlock(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rtb.Elements) != 3 {
		t.Fatalf("expected [section, list, section], got %d elements: %#v", len(rtb.Elements), rtb.Elements)
	}

	intro, ok := rtb.Elements[0].(*slack.RichTextSection)
	if !ok {
		t.Fatalf("expected element 0 to be a rich_text_section, got %T", rtb.Elements[0])
	}
	if got := sectionText(intro); got != "Intro:" {
		t.Fatalf("expected intro section text %q, got %q", "Intro:", got)
	}
	// The stray empty line above the list is the visible half of the defect.
	for _, e := range intro.Elements {
		if te, ok := e.(*slack.RichTextSectionTextElement); ok && strings.Contains(te.Text, "\n\n") {
			t.Fatalf("intro section still carries a %q text run: %#v", "\n\n", intro.Elements)
		}
	}

	list, ok := rtb.Elements[1].(*slack.RichTextList)
	if !ok {
		t.Fatalf("expected element 1 to be a rich_text_list, got %T", rtb.Elements[1])
	}
	if list.Style != slack.RTEListBullet {
		t.Errorf("expected bullet style, got %q", list.Style)
	}
	if list.Indent != 0 {
		t.Errorf("expected indent 0, got %d", list.Indent)
	}
	if len(list.Elements) != 2 {
		t.Fatalf("expected 2 list items, got %d", len(list.Elements))
	}
	for i, want := range []string{"a", "b"} {
		item, ok := list.Elements[i].(*slack.RichTextSection)
		if !ok {
			t.Fatalf("expected list item %d to be a rich_text_section, got %T", i, list.Elements[i])
		}
		if got := sectionText(item); got != want {
			t.Errorf("list item %d: got %q, want %q", i, got, want)
		}
	}

	outro, ok := rtb.Elements[2].(*slack.RichTextSection)
	if !ok {
		t.Fatalf("expected element 2 to be a rich_text_section, got %T", rtb.Elements[2])
	}
	if got := sectionText(outro); got != "Outro." {
		t.Errorf("expected outro section text %q, got %q", "Outro.", got)
	}
}

// Every recognised bullet character produces the same native list.
func TestUnitMarkdownToRichTextBlock_AllBulletMarkersBecomeLists(t *testing.T) {
	for _, marker := range []string{"•", "◦", "‣", "⁃", "·"} {
		t.Run(marker, func(t *testing.T) {
			input := "Intro:\n\n" + marker + " alpha\n" + marker + " beta\n"
			rtb, err := markdownToRichTextBlock(input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			list := firstRichTextList(rtb)
			if list == nil {
				t.Fatalf("expected a rich_text_list for marker %q, got %#v", marker, rtb.Elements)
			}
			if list.Style != slack.RTEListBullet {
				t.Errorf("expected bullet style, got %q", list.Style)
			}
			if len(list.Elements) != 2 {
				t.Fatalf("expected 2 list items, got %d", len(list.Elements))
			}
			// The marker itself must not leak into the visible item text.
			for i, want := range []string{"alpha", "beta"} {
				item, ok := list.Elements[i].(*slack.RichTextSection)
				if !ok {
					t.Fatalf("expected list item %d to be a rich_text_section, got %T", i, list.Elements[i])
				}
				if got := sectionText(item); got != want {
					t.Errorf("list item %d: got %q, want %q", i, got, want)
				}
			}
		})
	}
}

// The forms that already worked must keep working.
func TestUnitMarkdownToRichTextBlock_ExistingListFormsUnchanged(t *testing.T) {
	cases := []struct {
		marker string
		input  string
		style  slack.RichTextListElementType
	}{
		{"-", "Intro:\n\n- alpha\n- beta\n", slack.RTEListBullet},
		{"*", "Intro:\n\n* alpha\n* beta\n", slack.RTEListBullet},
		{"+", "Intro:\n\n+ alpha\n+ beta\n", slack.RTEListBullet},
		{"1.", "Intro:\n\n1. alpha\n2. beta\n", slack.RTEListOrdered},
	}
	for _, tc := range cases {
		t.Run(tc.marker, func(t *testing.T) {
			rtb, err := markdownToRichTextBlock(tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			list := firstRichTextList(rtb)
			if list == nil {
				t.Fatalf("expected a rich_text_list, got %#v", rtb.Elements)
			}
			if list.Style != tc.style {
				t.Errorf("expected style %q, got %q", tc.style, list.Style)
			}
			if len(list.Elements) != 2 {
				t.Fatalf("expected 2 list items, got %d", len(list.Elements))
			}
			if missing := draftContentLoss(tc.input, rtb); len(missing) > 0 {
				t.Fatalf("content dropped: %v", missing)
			}
		})
	}
}

// The bullet rewrite must be token-neutral: tokenizeWords treats every marker as
// a separator, so the loss check sees the same multiset on both sides. A false
// positive here is a hard refusal to create the draft, which is worse than the
// formatting defect being fixed.
func TestUnitDraftContentLoss_BulletMarkersAreTokenNeutral(t *testing.T) {
	for _, marker := range []string{"•", "◦", "‣", "⁃", "·", "-", "*", "+"} {
		t.Run(marker, func(t *testing.T) {
			input := "Intro to the list:\n\n" +
				marker + " first **bold** item\n" +
				marker + " second `code` item\n" +
				marker + " third [label](https://example.com/x) item\n\n" +
				"Outro."
			rtb, err := markdownToRichTextBlock(input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if missing := draftContentLoss(input, rtb); len(missing) > 0 {
				t.Fatalf("marker %q produced spurious content loss: %v", marker, missing)
			}
			if got := countEmptyTextElements(rtb); got != 0 {
				t.Fatalf("expected no empty text elements, got %d", got)
			}
		})
	}
}

// Regression: CommonMark defines an ordered marker as digits followed by "." or
// ")", and goldmark implements both, so "1)" already yields an ordered list. No
// code may rewrite "1)" to "1." — that would strip the digit from the visible
// text on the got side while the want side still tokenised it, manufacturing a
// false "conversion dropped content" refusal.
func TestUnitMarkdownToRichTextBlock_ParenOrderedListAlreadyWorks(t *testing.T) {
	input := "Intro:\n1) a\n2) b\n"

	src, err := preprocessDraftSource(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src.Text != input {
		t.Fatalf("preprocessDraftSource rewrote the %q form: got %q", "1)", src.Text)
	}

	rtb, err := markdownToRichTextBlock(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	list := firstRichTextList(rtb)
	if list == nil {
		t.Fatalf("expected a rich_text_list, got %#v", rtb.Elements)
	}
	if list.Style != slack.RTEListOrdered {
		t.Fatalf("expected style %q, got %q", slack.RTEListOrdered, list.Style)
	}
	if len(list.Elements) != 2 {
		t.Fatalf("expected 2 list items, got %d", len(list.Elements))
	}
	for i, want := range []string{"a", "b"} {
		item, ok := list.Elements[i].(*slack.RichTextSection)
		if !ok {
			t.Fatalf("expected list item %d to be a rich_text_section, got %T", i, list.Elements[i])
		}
		if got := sectionText(item); got != want {
			t.Errorf("list item %d: got %q, want %q", i, got, want)
		}
	}
	if missing := draftContentLoss(input, rtb); len(missing) > 0 {
		t.Fatalf("expected no content loss, got %v", missing)
	}
}

// A bullet character inside fenced or indented code is content, not a marker.
func TestUnitMarkdownToRichTextBlock_BulletsInCodeStayVerbatim(t *testing.T) {
	input := "Intro:\n\n```\n• not a list\n```\n\n• a real list item\n"
	rtb, err := markdownToRichTextBlock(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var pre *slack.RichTextPreformatted
	for _, el := range rtb.Elements {
		if p, ok := el.(*slack.RichTextPreformatted); ok {
			pre = p
		}
	}
	if pre == nil {
		t.Fatalf("expected a rich_text_preformatted element, got %#v", rtb.Elements)
	}
	var code strings.Builder
	for _, e := range pre.Elements {
		if te, ok := e.(*slack.RichTextSectionTextElement); ok {
			code.WriteString(te.Text)
		}
	}
	if !strings.Contains(code.String(), "• not a list") {
		t.Errorf("fenced code lost its bullet character: %q", code.String())
	}

	if list := firstRichTextList(rtb); list == nil {
		t.Errorf("expected the bullet line outside the fence to become a list, got %#v", rtb.Elements)
	}
}

// firstRichTextList returns the first rich_text_list element in a block, or nil.
func firstRichTextList(b *slack.RichTextBlock) *slack.RichTextList {
	for _, el := range b.Elements {
		if l, ok := el.(*slack.RichTextList); ok {
			return l
		}
	}
	return nil
}

func TestUnitMarkdownToRichTextBlock_BoldAndLinkStyles(t *testing.T) {
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

// allSectionElements returns every rich_text section element in a block, across
// sections, list items, quotes and preformatted blocks.
func allSectionElements(b *slack.RichTextBlock) []slack.RichTextSectionElement {
	var out []slack.RichTextSectionElement
	for _, el := range b.Elements {
		switch e := el.(type) {
		case *slack.RichTextSection:
			out = append(out, e.Elements...)
		case *slack.RichTextQuote:
			out = append(out, e.Elements...)
		case *slack.RichTextPreformatted:
			out = append(out, e.Elements...)
		case *slack.RichTextList:
			for _, item := range e.Elements {
				if s, ok := item.(*slack.RichTextSection); ok {
					out = append(out, s.Elements...)
				}
			}
		}
	}
	return out
}

// assertNoPlaceholders is the global tripwire for the placeholder design: the
// private-use rune the converter uses internally must never reach a draft, in a
// text run, a code span, a link label or a link URL.
func assertNoPlaceholders(t *testing.T, b *slack.RichTextBlock) {
	t.Helper()
	const ph = string(draftPlaceholderRune)
	for _, e := range allSectionElements(b) {
		switch el := e.(type) {
		case *slack.RichTextSectionTextElement:
			if strings.Contains(el.Text, ph) {
				t.Fatalf("placeholder rune leaked into a text element: %q", el.Text)
			}
		case *slack.RichTextSectionLinkElement:
			if strings.Contains(el.Text, ph) || strings.Contains(el.URL, ph) {
				t.Fatalf("placeholder rune leaked into a link element: %q / %q", el.Text, el.URL)
			}
		}
	}
}

// firstUserElement returns the first user element in a block, or nil.
func firstUserElement(b *slack.RichTextBlock) *slack.RichTextSectionUserElement {
	for _, e := range allSectionElements(b) {
		if u, ok := e.(*slack.RichTextSectionUserElement); ok {
			return u
		}
	}
	return nil
}

// A reference resolves wherever it appears: in a list item, a blockquote and a
// heading, not just in a bare paragraph. goldmark splits a raw "<@U…>" at a
// context-dependent position, which is exactly why the reference is lifted out
// before the parse.
func TestUnitMarkdownToRichTextBlock_MentionsInEveryBlockPosition(t *testing.T) {
	input := "Ping <@U0AAAAA1> in the intro.\n\n" +
		"# Heading with <@U0BBBBB2>\n\n" +
		"- item with <@U0CCCCC3> and <#C0DDDDD4>\n" +
		"- plain item\n\n" +
		"> quoted <!subteam^S0EEEEE5|@eng> here\n"

	rtb, err := markdownToRichTextBlock(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertNoPlaceholders(t, rtb)

	var users, channels, groups int
	for _, e := range allSectionElements(rtb) {
		switch e.(type) {
		case *slack.RichTextSectionUserElement:
			users++
		case *slack.RichTextSectionChannelElement:
			channels++
		case *slack.RichTextSectionUserGroupElement:
			groups++
		}
	}
	if users != 3 {
		t.Errorf("expected 3 user elements (paragraph, heading, list item), got %d", users)
	}
	if channels != 1 {
		t.Errorf("expected 1 channel element in the list item, got %d", channels)
	}
	if groups != 1 {
		t.Errorf("expected 1 usergroup element in the blockquote, got %d", groups)
	}

	// The usergroup element carries only the ID: slack-go v0.19.0 has no field
	// for a display name, and the "|@eng" label is deliberately dropped.
	for _, e := range allSectionElements(rtb) {
		if g, ok := e.(*slack.RichTextSectionUserGroupElement); ok && g.UsergroupID != "S0EEEEE5" {
			t.Errorf("usergroup id: got %q, want %q", g.UsergroupID, "S0EEEEE5")
		}
	}

	// The list item's surrounding words and spacing must be unchanged.
	list := firstRichTextList(rtb)
	if list == nil {
		t.Fatalf("expected a rich_text_list, got %#v", rtb.Elements)
	}
	item, ok := list.Elements[0].(*slack.RichTextSection)
	if !ok {
		t.Fatalf("expected a rich_text_section list item, got %T", list.Elements[0])
	}
	if got, want := sectionText(item), "item with <@U0CCCCC3> and <#C0DDDDD4>"; got != want {
		t.Errorf("list item text: got %q, want %q", got, want)
	}

	if missing := draftContentLoss(input, rtb); len(missing) > 0 {
		t.Fatalf("mentions produced spurious content loss: %v", missing)
	}
}

// A reference in a backtick code span is something the user wrote to be read
// literally: it expands back to the source literal and must NOT become an
// element. A fenced block is never even scanned, so it survives verbatim.
func TestUnitMarkdownToRichTextBlock_MentionsInCodeStayLiteral(t *testing.T) {
	input := "Use `<@U0AAAAA1>` to tag someone.\n\n" +
		"```\ncurl -d '<@U0AAAAA1>' https://example.com\n```\n"

	rtb, err := markdownToRichTextBlock(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertNoPlaceholders(t, rtb)

	if u := firstUserElement(rtb); u != nil {
		t.Fatalf("a mention inside code must not become a user element, got %+v", u)
	}

	var codeSpan string
	for _, e := range allSectionElements(rtb) {
		if te, ok := e.(*slack.RichTextSectionTextElement); ok && te.Style != nil && te.Style.Code {
			codeSpan = te.Text
		}
	}
	if codeSpan != "<@U0AAAAA1>" {
		t.Errorf("code span: got %q, want %q", codeSpan, "<@U0AAAAA1>")
	}

	var pre *slack.RichTextPreformatted
	for _, el := range rtb.Elements {
		if p, ok := el.(*slack.RichTextPreformatted); ok {
			pre = p
		}
	}
	if pre == nil {
		t.Fatalf("expected a rich_text_preformatted element, got %#v", rtb.Elements)
	}
	var code strings.Builder
	for _, e := range pre.Elements {
		if te, ok := e.(*slack.RichTextSectionTextElement); ok {
			code.WriteString(te.Text)
		}
	}
	if !strings.Contains(code.String(), "curl -d '<@U0AAAAA1>' https://example.com") {
		t.Errorf("fenced block did not survive verbatim: %q", code.String())
	}

	if missing := draftContentLoss(input, rtb); len(missing) > 0 {
		t.Fatalf("mentions in code produced spurious content loss: %v", missing)
	}
}

// The design's whole dependence on goldmark is now "a placeholder of U+E000 plus
// ASCII digits is not inline-special", so a placeholder is one indivisible text
// run in every position. This is the assertion that pins that down; if a future
// goldmark splits one, expansion fails and the private-use rune leaks.
func TestUnitMarkdownToRichTextBlock_PlaceholderSurvivesGoldmarkRoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantUser  bool
		wantTexts []string
	}{
		{"paragraph", "Ping <@U0AAAAA1> now.", true, []string{"Ping ", " now."}},
		{"heading", "# Ping <@U0AAAAA1> now", true, []string{"Ping ", " now"}},
		{"list item", "- Ping <@U0AAAAA1> now", true, []string{"Ping ", " now"}},
		{"blockquote", "> Ping <@U0AAAAA1> now", true, []string{"Ping ", " now"}},
		{"emphasis", "**Ping <@U0AAAAA1> now**", true, []string{"Ping ", " now"}},
		// A rich_text link carries a flat string label with no room for a nested
		// element, so the reference expands back to the literal instead.
		{"link label", "[Ping <@U0AAAAA1> now](https://example.com/x)", false, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rtb, err := markdownToRichTextBlock(tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertNoPlaceholders(t, rtb)

			user := firstUserElement(rtb)
			if tc.wantUser {
				if user == nil {
					t.Fatalf("expected a user element, got %#v", allSectionElements(rtb))
				}
				if user.UserID != "U0AAAAA1" {
					t.Errorf("user id: got %q, want %q", user.UserID, "U0AAAAA1")
				}
			} else {
				if user != nil {
					t.Fatalf("expected no user element, got %+v", user)
				}
				var label string
				for _, e := range allSectionElements(rtb) {
					if l, ok := e.(*slack.RichTextSectionLinkElement); ok {
						label = l.Text
					}
				}
				if label != "Ping <@U0AAAAA1> now" {
					t.Errorf("link label: got %q, want %q", label, "Ping <@U0AAAAA1> now")
				}
			}

			flat := flattenRichText(rtb)
			for _, want := range tc.wantTexts {
				if !strings.Contains(flat, want) {
					t.Errorf("surrounding text %q was mangled; full text %q", want, flat)
				}
			}
			if missing := draftContentLoss(tc.input, rtb); len(missing) > 0 {
				t.Fatalf("spurious content loss: %v", missing)
			}
			if got := countEmptyTextElements(rtb); got != 0 {
				t.Fatalf("expected no empty text elements, got %d", got)
			}
		})
	}
}

// The false-positive tripwire. draftContentLoss must stay balanced for every
// reference form, labelled ones included: a false positive here is a hard
// refusal to create the draft, which is worse than the bug being fixed.
func TestUnitDraftContentLoss_AllReferenceFormsAreBalanced(t *testing.T) {
	refs := []string{
		"<@U0AAAAA1>",
		"<@U0AAAAA1|dana>",
		"<@W0AAAAA1>",
		"<@W0AAAAA1|dana>",
		"<#C0AAAAA1>",
		"<#C0AAAAA1|general>",
		"<!subteam^S0AAAAA1>",
		"<!subteam^S0AAAAA1|@eng>",
		"<!here>",
		"<!channel>",
		"<!everyone>",
		// The malformed classes are emitted as inert text carrying the canonical
		// literal, and canonicalizeReferences rewrites the want side to the same
		// string — so the two halves have to stay balanced for these too. A false
		// positive here would replace a precise "that code is not a user ID"
		// refusal with a baffling "conversion dropped content" one.
		"<@D0BJE66FPU5>",
		"<@D0BJE66FPU5|dana>",
		"<#S0AAAAA1>",
		"<#S0AAAAA1|eng>",
		"<!subteam^X0AAAAA1>",
		"<!subteam^X0AAAAA1|@eng>",
	}

	for _, ref := range refs {
		t.Run(ref, func(t *testing.T) {
			input := "Heads up " + ref + " — the **deploy** is done.\n\n" +
				"- bullet mentioning " + ref + "\n" +
				"- bullet with `code` and [a label](https://example.com/x)\n\n" +
				"> quoted " + ref + "\n\n" +
				"Also `" + ref + "` shown literally, and:\n\n" +
				"```\nfenced " + ref + "\n```\n\n" +
				"Outro."

			rtb, err := markdownToRichTextBlock(input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if missing := draftContentLoss(input, rtb); len(missing) > 0 {
				t.Fatalf("reference %q produced spurious content loss: %v", ref, missing)
			}
			assertNoPlaceholders(t, rtb)
			if got := countEmptyTextElements(rtb); got != 0 {
				t.Fatalf("expected no empty text elements, got %d", got)
			}
		})
	}
}

// A reference standing alone in a paragraph must not leave an empty text run
// behind: Slack's drafts API rejects those with "invalid_message".
func TestUnitMarkdownToRichTextBlock_LoneMentionEmitsNoEmptyRun(t *testing.T) {
	rtb, err := markdownToRichTextBlock("<@U0AAAAA1>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := countEmptyTextElements(rtb); got != 0 {
		t.Fatalf("expected no empty text elements, got %d", got)
	}
	if u := firstUserElement(rtb); u == nil || u.UserID != "U0AAAAA1" {
		t.Fatalf("expected a lone user element, got %#v", allSectionElements(rtb))
	}
	assertNoPlaceholders(t, rtb)
}
