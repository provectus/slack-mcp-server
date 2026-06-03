package handler

import (
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

	for n := doc.FirstChild(); n != nil; n = n.NextSibling() {
		switch n.Kind() {
		case ast.KindParagraph:
			elements = appendBlankLine(elements)
			elements = append(elements, &slack.RichTextSection{
				Type:     slack.RTESection,
				Elements: parseDraftInline(n, source, false),
			})

		case ast.KindHeading:
			elements = appendBlankLine(elements)
			elements = append(elements, &slack.RichTextSection{
				Type:     slack.RTESection,
				Elements: parseDraftInline(n, source, true),
			})

		case ast.KindList:
			list := n.(*ast.List)
			var items []slack.RichTextElement
			for li := n.FirstChild(); li != nil; li = li.NextSibling() {
				if li.Kind() != ast.KindListItem {
					continue
				}
				items = append(items, &slack.RichTextSection{
					Type:     slack.RTESection,
					Elements: parseDraftInline(li.FirstChild(), source, false),
				})
			}
			style := slack.RTEListBullet
			if list.IsOrdered() {
				style = slack.RTEListOrdered
			}
			elements = append(elements, &slack.RichTextList{
				Type:     slack.RTEList,
				Style:    style,
				Elements: items,
			})

		case ast.KindFencedCodeBlock, ast.KindCodeBlock:
			elements = append(elements, &slack.RichTextPreformatted{
				RichTextSection: slack.RichTextSection{
					Type: slack.RTEPreformatted,
					Elements: []slack.RichTextSectionElement{
						&slack.RichTextSectionTextElement{
							Type: slack.RTSEText,
							Text: codeBlockText(n, source),
						},
					},
				},
			})

		case ast.KindBlockquote:
			elements = append(elements, &slack.RichTextQuote{
				Type:     slack.RTEQuote,
				Elements: parseDraftInline(n, source, false),
			})
		}
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

// appendBlankLine inserts a newline-only section between top-level paragraphs so
// they are visually separated in the composer, mirroring blank lines in markdown.
func appendBlankLine(elements []slack.RichTextElement) []slack.RichTextElement {
	if len(elements) == 0 {
		return elements
	}
	return append(elements, &slack.RichTextSection{
		Type: slack.RTESection,
		Elements: []slack.RichTextSectionElement{
			&slack.RichTextSectionTextElement{Type: slack.RTSEText, Text: "\n"},
		},
	})
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
