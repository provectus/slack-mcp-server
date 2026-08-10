package handler

import (
	"strings"
	"testing"
)

// preprocessDraftSource is a pure string→string rewrite, so the fence state
// machine and the marker pattern are pinned here rather than through the
// converter.
func TestUnitPreprocessDraftSource_BulletRewrite(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// All five markers the assistant commonly types.
		{"bullet U+2022", "• a\n• b\n", "- a\n- b\n"},
		{"white bullet U+25E6", "◦ a\n◦ b\n", "- a\n- b\n"},
		{"triangular bullet U+2023", "‣ a\n‣ b\n", "- a\n- b\n"},
		{"hyphen bullet U+2043", "⁃ a\n⁃ b\n", "- a\n- b\n"},
		{"middle dot U+00B7", "· a\n· b\n", "- a\n- b\n"},

		// Groups 1, 3 and 4 survive byte-for-byte; only the marker changes.
		{"tab after marker preserved", "•\ta\n", "-\ta\n"},
		{"multiple spaces after marker preserved", "•   a\n", "-   a\n"},
		{"trailing content preserved", "• a **b** `c`\n", "- a **b** `c`\n"},

		// No space after the marker: rewriting would change visible text.
		{"no space after marker left alone", "•a\n", "•a\n"},
		{"no space after middle dot left alone", "·a\n", "·a\n"},

		// Mid-sentence markers are not list markers.
		{"mid-sentence markers left alone", "Options: • a • b\nx·y\n", "Options: • a • b\nx·y\n"},

		// CommonMark's marker-indent budget is three spaces.
		{"three leading spaces rewritten", "   • a\n", "   - a\n"},
		{"four leading spaces left alone", "    • a\n", "    • a\n"},

		// 1) is already an ordered marker in CommonMark and goldmark. Rewriting
		// it to 1. would strip the digit from the visible text and manufacture a
		// false draftContentLoss refusal, so nothing here may touch it.
		{"ordered paren marker left alone", "Intro:\n1) a\n2) b\n", "Intro:\n1) a\n2) b\n"},
		{"ordered dot marker left alone", "1. a\n2. b\n", "1. a\n2. b\n"},

		// Fenced code: the marker is content, not a marker.
		{"backtick fence left alone", "```\n• a\n```\n• b\n", "```\n• a\n```\n- b\n"},
		{"tilde fence left alone", "~~~\n• a\n~~~\n• b\n", "~~~\n• a\n~~~\n- b\n"},
		{"fence with info string", "```go\n• a\n```\n• b\n", "```go\n• a\n```\n- b\n"},
		{"shorter run does not close a longer fence", "````\n• a\n```\n• b\n````\n• c\n", "````\n• a\n```\n• b\n````\n- c\n"},
		{"backticks do not close a tilde fence", "~~~\n• a\n```\n~~~\n• b\n", "~~~\n• a\n```\n~~~\n- b\n"},
		{"unclosed fence leaves the rest verbatim", "intro\n\n```\n• a\n• b\n", "intro\n\n```\n• a\n• b\n"},
		{"fence nested in a list item", "- item\n\n  ```\n  • a\n  ```\n\n• b\n", "- item\n\n  ```\n  • a\n  ```\n\n- b\n"},

		// Indented code: 4 spaces, or a tab (worth 4 columns).
		{"indented code block left alone", "intro\n\n    • a\n    • b\n\n• c\n", "intro\n\n    • a\n    • b\n\n- c\n"},
		{"tab-indented code block left alone", "intro\n\n\t• a\n\n• b\n", "intro\n\n\t• a\n\n- b\n"},
		{"blank line does not end an indented code block", "intro\n\n    • a\n\n    • b\n\n• c\n", "intro\n\n    • a\n\n    • b\n\n- c\n"},

		// Whole-document shapes.
		{"empty input", "", ""},
		{"intro then bullets", "Intro:\n\n• a\n• b\n\nOutro.", "Intro:\n\n- a\n- b\n\nOutro."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, err := preprocessDraftSource(tt.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if src.Text != tt.want {
				t.Fatalf("preprocessDraftSource(%q)\n got: %q\nwant: %q", tt.in, src.Text, tt.want)
			}
		})
	}
}

// TestUnitSplitPlaceholders_UnresolvedPlaceholderStaysItsOwnSpan pins the
// failure branch of splitPlaceholders.
//
// Every placeholder the converter sees was minted by draftPlaceholder against
// Refs, so refAt cannot fail today. But it used to `continue` without advancing
// `last`, which folded the unresolved placeholder into the NEXT literal span —
// laundering a U+E000 rune into ordinary draft text where nothing downstream
// would look at it again. The failure branch now emits the placeholder as its
// own span and advances past it, so the surrounding literals stay exactly what
// they were and the spans still reassemble byte-for-byte.
func TestUnitSplitPlaceholders_UnresolvedPlaceholderStaysItsOwnSpan(t *testing.T) {
	ref, ok := parseMentionRef("<@U0AAAAA1>")
	if !ok {
		t.Fatal("fixture reference must parse")
	}
	src := draftSource{Refs: []mentionRef{ref}}

	// Index 0 resolves; index 7 is out of range and takes the failure branch.
	text := "before " + draftPlaceholder(0) + " middle " + draftPlaceholder(7) + " after"
	spans := src.splitPlaceholders(text)

	var rebuilt string
	var unresolved int
	for _, s := range spans {
		if s.Ref != nil {
			rebuilt += draftPlaceholder(0)
			continue
		}
		rebuilt += s.Text
		if s.Text == draftPlaceholder(7) {
			unresolved++
			continue
		}
		if strings.ContainsRune(s.Text, draftPlaceholderRune) {
			t.Fatalf("an unresolved placeholder was folded into a literal span: %q", s.Text)
		}
	}

	if unresolved != 1 {
		t.Fatalf("expected the unresolved placeholder as exactly one span of its own, got %d in %+v", unresolved, spans)
	}
	if rebuilt != text {
		t.Fatalf("spans must reassemble byte-for-byte\n got: %q\nwant: %q", rebuilt, text)
	}

	// The surrounding literals are untouched, and the resolvable reference on
	// the same run still resolves.
	if spans[0].Text != "before " {
		t.Fatalf("leading literal changed: %q", spans[0].Text)
	}
	if spans[len(spans)-1].Text != " after" {
		t.Fatalf("trailing literal changed: %q", spans[len(spans)-1].Text)
	}
	if spans[1].Ref == nil || spans[1].Ref.canonicalLiteral() != "<@U0AAAAA1>" {
		t.Fatalf("the resolvable reference must still resolve, got %+v", spans[1])
	}
}

// TestUnitRewriteDraftLines_IndentedCodeIsRelativeToListContent pins the line
// scan's list-context clause. Four columns inside a "  - b" item, whose content
// starts at column 4, is that item's continuation paragraph — goldmark renders
// it live — so the scan must not call it code and skip the reference in it.
// Eight columns there IS code, and must still be left verbatim, as must four
// columns at the top level.
func TestUnitRewriteDraftLines_IndentedCodeIsRelativeToListContent(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantRefs []string
	}{
		{
			name:     "reference in nested list-item continuation is lifted",
			in:       "- a\n  - b\n\n    please review <@U0BOGUS1> thanks\n",
			wantRefs: []string{"<@U0BOGUS1>"},
		},
		{
			name:     "reference in a top-level list-item continuation is lifted",
			in:       "- a\n\n  please review <@U0BOGUS1> thanks\n",
			wantRefs: []string{"<@U0BOGUS1>"},
		},
		{
			name:     "code inside a list item is still code",
			in:       "- a\n\n      <@U0BOGUS1>\n",
			wantRefs: nil,
		},
		{
			name:     "code inside a nested list item is still code",
			in:       "- a\n  - b\n\n        <@U0BOGUS1>\n",
			wantRefs: nil,
		},
		{
			name:     "top-level indented code is still code",
			in:       "intro\n\n    <@U0BOGUS1>\n",
			wantRefs: nil,
		},
		{
			name:     "dedenting out of the list restores the column-zero measure",
			in:       "- a\n\nintro\n\n    <@U0BOGUS1>\n",
			wantRefs: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, refs := rewriteDraftLines(tt.in)

			var got []string
			for _, r := range refs {
				got = append(got, r.canonicalLiteral())
			}
			if len(got) != len(tt.wantRefs) {
				t.Fatalf("rewriteDraftLines(%q) lifted %v, want %v", tt.in, got, tt.wantRefs)
			}
			for i := range got {
				if got[i] != tt.wantRefs[i] {
					t.Fatalf("rewriteDraftLines(%q) lifted %v, want %v", tt.in, got, tt.wantRefs)
				}
			}
			if len(tt.wantRefs) == 0 && text != tt.in {
				t.Fatalf("a source with nothing to lift must be a byte-for-byte no-op\n got: %q\nwant: %q", text, tt.in)
			}
		})
	}
}
