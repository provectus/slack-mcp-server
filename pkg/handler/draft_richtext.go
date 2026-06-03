package handler

import (
	"sort"
	"strings"
	"unicode"

	"github.com/slack-go/slack"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	gmtext "github.com/yuin/goldmark/text"
)

// draftMD is the markdown parser used to build rich_text blocks for drafts.
var draftMD = goldmark.New()

// plainRichTextBlock wraps text in a single rich_text block with one plain
// rich_text_section, used for text/plain drafts and as a markdown fallback.
func plainRichTextBlock(text string) *slack.RichTextBlock {
	return &slack.RichTextBlock{
		Type: slack.MBTRichText,
		Elements: []slack.RichTextElement{
			&slack.RichTextSection{
				Type: slack.RTESection,
				Elements: []slack.RichTextSectionElement{
					&slack.RichTextSectionTextElement{Type: slack.RTSEText, Text: text},
				},
			},
		},
	}
}

// markdownToRichTextBlock converts markdown into a single Slack rich_text block.
//
// Slack's undocumented drafts.create endpoint only accepts rich_text blocks; it
// silently drops section/header blocks. The shared ConvertMarkdownTextToBlocks
// helper emits paragraphs as section blocks, so it cannot be used for drafts.
// This converter emits paragraphs as rich_text_section elements, lists as
// rich_text_list, fenced code as rich_text_preformatted and blockquotes as
// rich_text_quote, all inside one rich_text block, preserving inline bold,
// italic, inline-code and link styling.
func markdownToRichTextBlock(markdown string) (*slack.RichTextBlock, error) {
	source := []byte(markdown)
	doc := draftMD.Parser().Parse(gmtext.NewReader(source))

	var elements []slack.RichTextElement
	// Whether the previously appended block is "self-breaking" (lists, quotes and
	// preformatted blocks render block-level and emit their own trailing newline).
	prevSelfBreaking := false

	for n := doc.FirstChild(); n != nil; n = n.NextSibling() {
		var els []slack.RichTextElement
		selfBreaking := false

		switch n.Kind() {
		case ast.KindParagraph:
			els = []slack.RichTextElement{&slack.RichTextSection{
				Type:     slack.RTESection,
				Elements: parseDraftInline(n, source, false),
			}}

		case ast.KindHeading:
			els = []slack.RichTextElement{&slack.RichTextSection{
				Type:     slack.RTESection,
				Elements: parseDraftInline(n, source, true),
			}}

		case ast.KindList:
			selfBreaking = true
			els = buildRichTextLists(n.(*ast.List), source, 0)

		case ast.KindFencedCodeBlock, ast.KindCodeBlock:
			selfBreaking = true
			els = []slack.RichTextElement{&slack.RichTextPreformatted{
				RichTextSection: slack.RichTextSection{
					Type: slack.RTEPreformatted,
					Elements: []slack.RichTextSectionElement{
						&slack.RichTextSectionTextElement{
							Type: slack.RTSEText,
							Text: codeBlockText(n, source),
						},
					},
				},
			}}

		case ast.KindBlockquote:
			selfBreaking = true
			els = []slack.RichTextElement{&slack.RichTextQuote{
				Type:     slack.RTEQuote,
				Elements: parseDraftInline(n, source, false),
			}}

		default:
			// Unknown top-level block: best-effort text extraction so content is
			// never silently dropped. draftContentLoss is the hard backstop.
			inline := parseDraftInline(n, source, false)
			if len(inline) == 0 {
				continue
			}
			els = []slack.RichTextElement{&slack.RichTextSection{Type: slack.RTESection, Elements: inline}}
		}

		if len(els) == 0 {
			continue
		}
		// Separate top-level blocks so they render as distinct paragraphs. A blank
		// line ("\n\n") is needed between inline sections, but after a self-breaking
		// block (list/quote/code), which already emits a trailing newline, a single
		// "\n" is enough to avoid a double blank line.
		if len(elements) > 0 {
			sep := "\n\n"
			if prevSelfBreaking {
				sep = "\n"
			}
			elements = append(elements, newlineSection(sep))
		}
		elements = append(elements, els...)
		prevSelfBreaking = selfBreaking
	}

	// Never return an empty block: fall back to the raw text as one section so
	// content is preserved even if the markdown produced no recognised nodes.
	if len(elements) == 0 {
		elements = append(elements, &slack.RichTextSection{
			Type: slack.RTESection,
			Elements: []slack.RichTextSectionElement{
				&slack.RichTextSectionTextElement{Type: slack.RTSEText, Text: markdown},
			},
		})
	}

	return &slack.RichTextBlock{
		Type:     slack.MBTRichText,
		Elements: elements,
	}, nil
}

// buildRichTextLists converts a markdown list (and any nested lists) into one
// or more rich_text_list elements. Nested lists become separate rich_text_list
// elements with an increased Indent, emitted in document order so no list
// content is dropped. Loose list items (multiple block children) are joined with
// newlines within the item's section.
func buildRichTextLists(list *ast.List, source []byte, indent int) []slack.RichTextElement {
	style := slack.RTEListBullet
	if list.IsOrdered() {
		style = slack.RTEListOrdered
	}

	var out []slack.RichTextElement
	var items []slack.RichTextElement
	flush := func() {
		if len(items) > 0 {
			out = append(out, &slack.RichTextList{
				Type:     slack.RTEList,
				Style:    style,
				Indent:   indent,
				Elements: items,
			})
			items = nil
		}
	}

	for li := list.FirstChild(); li != nil; li = li.NextSibling() {
		if li.Kind() != ast.KindListItem {
			continue
		}

		var inline []slack.RichTextSectionElement
		var nested []*ast.List
		for c := li.FirstChild(); c != nil; c = c.NextSibling() {
			if c.Kind() == ast.KindList {
				nested = append(nested, c.(*ast.List))
				continue
			}
			sub := parseDraftInline(c, source, false)
			if len(sub) == 0 {
				continue
			}
			if len(inline) > 0 {
				inline = append(inline, &slack.RichTextSectionTextElement{Type: slack.RTSEText, Text: "\n"})
			}
			inline = append(inline, sub...)
		}

		items = append(items, &slack.RichTextSection{Type: slack.RTESection, Elements: inline})

		// Emit nested lists right after their parent item, splitting this list so
		// nesting renders in document order.
		if len(nested) > 0 {
			flush()
			for _, nl := range nested {
				out = append(out, buildRichTextLists(nl, source, indent+1)...)
			}
		}
	}
	flush()
	return out
}

// newlineSection is a rich_text_section containing only newline(s), used to
// separate top-level blocks so they are not glued together in the composer.
func newlineSection(text string) *slack.RichTextSection {
	return &slack.RichTextSection{
		Type: slack.RTESection,
		Elements: []slack.RichTextSectionElement{
			&slack.RichTextSectionTextElement{Type: slack.RTSEText, Text: text},
		},
	}
}

// codeBlockText extracts the raw text of a (fenced) code block.
func codeBlockText(n ast.Node, source []byte) string {
	var text string
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		line := lines.At(i)
		text += string(line.Value(source))
	}
	return text
}

// parseDraftInline converts the inline children of a container node (paragraph,
// heading, list item, blockquote) into rich_text_section elements, preserving
// bold, italic, inline-code and link styling. When forceBold is true every text
// run is bolded (used to approximate markdown headings, which rich_text lacks).
func parseDraftInline(n ast.Node, source []byte, forceBold bool) []slack.RichTextSectionElement {
	var elements []slack.RichTextSectionElement

	var process func(node ast.Node, isBold, isItalic bool)
	process = func(node ast.Node, isBold, isItalic bool) {
		if node == nil {
			return
		}
		switch node.Kind() {
		case ast.KindText:
			textNode := node.(*ast.Text)
			text := string(textNode.Segment.Value(source))
			elements = append(elements, &slack.RichTextSectionTextElement{
				Type:  slack.RTSEText,
				Text:  text,
				Style: draftTextStyle(isBold || forceBold, isItalic),
			})
			if textNode.SoftLineBreak() || textNode.HardLineBreak() {
				elements = append(elements, &slack.RichTextSectionTextElement{
					Type: slack.RTSEText,
					Text: "\n",
				})
			}

		case ast.KindString:
			strNode := node.(*ast.String)
			elements = append(elements, &slack.RichTextSectionTextElement{
				Type:  slack.RTSEText,
				Text:  string(strNode.Value),
				Style: draftTextStyle(isBold || forceBold, isItalic),
			})

		case ast.KindEmphasis:
			emp := node.(*ast.Emphasis)
			newBold := isBold || emp.Level == 2
			newItalic := isItalic || emp.Level == 1
			for c := node.FirstChild(); c != nil; c = c.NextSibling() {
				process(c, newBold, newItalic)
			}

		case ast.KindLink:
			link := node.(*ast.Link)
			var text string
			for c := link.FirstChild(); c != nil; c = c.NextSibling() {
				if c.Kind() == ast.KindText {
					text += string(c.(*ast.Text).Segment.Value(source))
				}
			}
			elements = append(elements, &slack.RichTextSectionLinkElement{
				Type: slack.RTSELink,
				Text: text,
				URL:  string(link.Destination),
			})

		case ast.KindAutoLink:
			al := node.(*ast.AutoLink)
			url := string(al.URL(source))
			elements = append(elements, &slack.RichTextSectionLinkElement{
				Type: slack.RTSELink,
				Text: url,
				URL:  url,
			})

		case ast.KindCodeSpan:
			var text string
			for c := node.FirstChild(); c != nil; c = c.NextSibling() {
				if c.Kind() == ast.KindText {
					text += string(c.(*ast.Text).Segment.Value(source))
				}
			}
			elements = append(elements, &slack.RichTextSectionTextElement{
				Type:  slack.RTSEText,
				Text:  text,
				Style: &slack.RichTextSectionTextStyle{Code: true},
			})

		default:
			for c := node.FirstChild(); c != nil; c = c.NextSibling() {
				process(c, isBold, isItalic)
			}
		}
	}

	process(n, false, false)
	return elements
}

func draftTextStyle(isBold, isItalic bool) *slack.RichTextSectionTextStyle {
	if !isBold && !isItalic {
		return nil
	}
	return &slack.RichTextSectionTextStyle{
		Bold:   isBold,
		Italic: isItalic,
	}
}

// draftContentLoss reports any visible-text words from the markdown input that
// did not make it into the generated rich_text block. It is the backstop that
// lets the draft handler refuse to create a draft that would silently drop
// content, rather than producing a lossy draft. List markers and formatting
// syntax are ignored (they are not text); link labels and URLs are both checked.
func draftContentLoss(input string, rtb *slack.RichTextBlock) []string {
	want := tokenizeWords(collectMarkdownText(input))
	got := tokenizeWords(flattenRichTextContent(rtb))

	var missing []string
	for word, n := range want {
		if got[word] < n {
			missing = append(missing, word)
		}
	}
	sort.Strings(missing)
	return missing
}

// collectMarkdownText walks the markdown AST and returns every piece of visible
// text plus link/autolink URLs, ignoring list markers and emphasis/heading
// syntax (which are not text nodes).
func collectMarkdownText(input string) string {
	source := []byte(input)
	doc := draftMD.Parser().Parse(gmtext.NewReader(source))

	var sb strings.Builder
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.Kind() {
		case ast.KindText:
			seg := n.(*ast.Text).Segment
			sb.Write(seg.Value(source))
			sb.WriteByte(' ')
		case ast.KindString:
			sb.Write(n.(*ast.String).Value)
			sb.WriteByte(' ')
		case ast.KindAutoLink:
			sb.Write(n.(*ast.AutoLink).URL(source))
			sb.WriteByte(' ')
		case ast.KindLink:
			// Label text is collected via the child Text nodes; add the URL here.
			sb.WriteString(string(n.(*ast.Link).Destination))
			sb.WriteByte(' ')
		}
		return ast.WalkContinue, nil
	})
	return sb.String()
}

// flattenRichTextContent returns all visible text in a rich_text block, including
// link labels and their URLs, across sections, lists, quotes and preformatted.
func flattenRichTextContent(rtb *slack.RichTextBlock) string {
	var sb strings.Builder
	writeSection := func(elems []slack.RichTextSectionElement) {
		for _, e := range elems {
			switch el := e.(type) {
			case *slack.RichTextSectionTextElement:
				sb.WriteString(el.Text)
				sb.WriteByte(' ')
			case *slack.RichTextSectionLinkElement:
				sb.WriteString(el.Text)
				sb.WriteByte(' ')
				sb.WriteString(el.URL)
				sb.WriteByte(' ')
			}
		}
	}
	for _, el := range rtb.Elements {
		switch e := el.(type) {
		case *slack.RichTextSection:
			writeSection(e.Elements)
		case *slack.RichTextList:
			for _, item := range e.Elements {
				if s, ok := item.(*slack.RichTextSection); ok {
					writeSection(s.Elements)
				}
			}
		case *slack.RichTextQuote:
			writeSection(e.Elements)
		case *slack.RichTextPreformatted:
			writeSection(e.Elements)
		}
	}
	return sb.String()
}

// tokenizeWords splits text into a multiset of lowercase alphanumeric words.
func tokenizeWords(s string) map[string]int {
	counts := make(map[string]int)
	for _, field := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		counts[field]++
	}
	return counts
}
