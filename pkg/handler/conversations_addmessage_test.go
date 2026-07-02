package handler

import (
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

// TestAddMessageMarkdownRendersRichText is a regression guard for the
// conversations_add_message send path. It used to convert markdown with the
// external slack-go-util helper, which emitted section blocks that render bold,
// links and lists less faithfully (and could leak literal markdown markers).
// The send path now shares markdownToRichTextBlock with the drafts path, so a
// representative message must become a rich_text block with real inline styles
// and no literal "**"/"[]()" markup surviving in the text.
func TestAddMessageMarkdownRendersRichText(t *testing.T) {
	input := "Deploy **shipped** to prod. See the [runbook](https://example.com/run).\n\n- check dashboards\n- watch errors"

	rtb, err := markdownToRichTextBlock(input)
	if err != nil {
		t.Fatalf("markdownToRichTextBlock returned error: %v", err)
	}
	if rtb.Type != slack.MBTRichText {
		t.Fatalf("expected rich_text block, got %q", rtb.Type)
	}

	flat := flattenRichText(rtb)
	for _, marker := range []string{"**", "](http"} {
		if strings.Contains(flat, marker) {
			t.Errorf("literal markdown marker %q leaked into rendered text: %q", marker, flat)
		}
	}

	var foundBold, foundLink, foundList bool
	for _, el := range rtb.Elements {
		switch e := el.(type) {
		case *slack.RichTextSection:
			for _, se := range e.Elements {
				switch x := se.(type) {
				case *slack.RichTextSectionTextElement:
					if x.Style != nil && x.Style.Bold && strings.Contains(x.Text, "shipped") {
						foundBold = true
					}
				case *slack.RichTextSectionLinkElement:
					if x.URL == "https://example.com/run" {
						foundLink = true
					}
				}
			}
		case *slack.RichTextList:
			foundList = true
		}
	}

	if !foundBold {
		t.Error("expected a bold styled run for 'shipped'")
	}
	if !foundLink {
		t.Error("expected a link element with the runbook URL")
	}
	if !foundList {
		t.Error("expected a rich_text_list for the bullet items")
	}
}
