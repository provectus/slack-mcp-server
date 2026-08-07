package handler

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// draftSource is the document handed to goldmark by markdownToRichTextBlock,
// after the pre-parse rewrites in preprocessDraftSource. It is the only place
// markdown is mutated before parsing.
type draftSource struct {
	// Text is the rewritten markdown goldmark parses.
	Text string
	// Refs holds every Slack reference lifted out of the source, in document
	// order. Text carries a placeholder at each reference's position, addressed
	// by index into this slice.
	Refs []mentionRef
}

// sourceSpan is one piece of a text run after placeholder splitting: either
// literal text (Ref nil) or a reference.
type sourceSpan struct {
	Text string
	Ref  *mentionRef
}

// draftPlaceholderRune marks a lifted reference in the preprocessed source. It
// is a private-use codepoint, and a placeholder is that rune plus ASCII digits
// plus the rune again — no character in it is inline-special to CommonMark, so
// goldmark treats a placeholder as one indivisible text run in every position:
// paragraph, heading, list item, blockquote, emphasis span and link label.
const draftPlaceholderRune = '\uE000'

var draftPlaceholderRe = regexp.MustCompile("\\x{E000}([0-9]+)\\x{E000}")

// draftPlaceholder renders the placeholder for the reference at index i.
func draftPlaceholder(i int) string {
	return string(draftPlaceholderRune) + strconv.Itoa(i) + string(draftPlaceholderRune)
}

// draftBulletLineRe matches a line that opens a list item with one of the
// unicode bullet characters the assistant commonly types. The pattern is
// ^-anchored with at most three leading spaces (CommonMark's marker-indent
// budget) so a mid-sentence bullet ("Options: • a • b", "x·y") never matches,
// and requires whitespace after the marker so "•a" is left verbatim — rewriting
// it would silently change the user's visible text to "-a".
var draftBulletLineRe = regexp.MustCompile("^([ \t]{0,3})([•◦‣⁃·])([ \t]+)(.*)$")

// preprocessDraftSource rewrites the raw markdown before goldmark parses it.
//
// Slack does not treat "•" as a list marker, and neither does CommonMark, so a
// run of "• item" lines renders as ordinary text lines that merely look like a
// list. Rewriting the marker to "- " ahead of the parse makes goldmark emit a
// genuine rich_text_list through its own well-tested list machinery, instead of
// requiring a fork of its most subtle block parser.
//
// The rewrite is token-neutral for draftContentLoss: tokenizeWords splits on
// !IsLetter && !IsNumber, and every recognised marker is a separator on both
// sides of the check.
//
// It also lifts every Slack reference literal out of the source into Refs,
// leaving an index placeholder behind. goldmark mangles "<…>" forms
// unpredictably — "<!here>" splits into two text elements, "<@U123>" splits at a
// context-dependent position, and "<!subteam^S123|@eng>" is claimed by the
// autolink parser and emerges as a link element — so a post-pass over assembled
// text elements cannot work. Ensuring goldmark never sees the "<" is the design.
func preprocessDraftSource(markdown string) (draftSource, error) {
	if strings.ContainsRune(markdown, draftPlaceholderRune) {
		// Restoring a user-typed U+E000 as part of a placeholder would be a
		// silent content change. Refusing is honest, and real Slack messages
		// effectively never contain a private-use codepoint.
		return draftSource{}, fmt.Errorf(
			"markdown contains the reserved character %U, which the converter uses internally to mark Slack references; remove it and retry",
			draftPlaceholderRune)
	}
	text, refs := rewriteDraftLines(markdown)
	return draftSource{Text: text, Refs: refs}, nil
}

// rewriteDraftLines performs the line scan. Fenced and indented code are emitted
// verbatim: a bullet character there is content, not a marker, and a reference
// there is something the user wrote to be read literally.
//
// Indented code is measured relative to the enclosing list item's content
// column, not to column zero. CommonMark nests the two: inside "- b", whose
// content starts at column 4, a code block needs eight columns, and four is
// simply the item's own continuation paragraph. Without that, a line like
// "    please review <@U…>" under a nested bullet is read as code by this scan
// while goldmark renders it as live list content — and the reference ships as
// raw visible text, unlifted and unconverted.
func rewriteDraftLines(markdown string) (string, []mentionRef) {
	lines := strings.Split(markdown, "\n")
	var refs []mentionRef

	var (
		fence          draftFence
		inFence        bool
		inIndentedCode bool
		// The column an indented code block must keep to stay open: its opening
		// line's base + 4.
		codeIndent int
		// Content columns of the list items currently open, outermost first. A
		// line dedented past the innermost one closes it.
		listIndents []int
		// An indented code block can only start after a blank line (or at the
		// start of the document); otherwise the indentation is a paragraph
		// continuation or list-item content.
		prevBlank = true
	)

	for i, line := range lines {
		if inFence {
			if draftFenceCloses(line, fence) {
				inFence = false
			}
			prevBlank = false
			continue
		}

		if strings.TrimSpace(line) == "" {
			// A blank line does not terminate an indented code block, nor a list.
			prevBlank = true
			continue
		}

		indent := leadingIndentWidth(line)

		if inIndentedCode {
			if indent >= codeIndent {
				prevBlank = false
				continue
			}
			inIndentedCode = false
		}

		for len(listIndents) > 0 && indent < listIndents[len(listIndents)-1] {
			listIndents = listIndents[:len(listIndents)-1]
		}
		base := 0
		if len(listIndents) > 0 {
			base = listIndents[len(listIndents)-1]
		}

		if indent >= base+4 && prevBlank {
			inIndentedCode = true
			codeIndent = base + 4
			prevBlank = false
			continue
		}

		if f, ok := draftFenceOpen(line); ok {
			fence, inFence = f, true
			prevBlank = false
			continue
		}

		prevBlank = false
		if m := draftBulletLineRe.FindStringSubmatch(line); m != nil {
			// Groups 1 (indent), 3 (post-marker whitespace) and 4 (content) are
			// preserved byte-for-byte; only the marker itself changes.
			line = m[1] + "-" + m[3] + m[4]
		}
		// Checked after the bullet rewrite so a normalised "• x" opens a list
		// context exactly as the "- x" it just became.
		if content, ok := draftListItemContentIndent(line); ok {
			listIndents = append(listIndents, content)
		}
		lines[i] = liftMentionRefs(line, &refs)
	}

	return strings.Join(lines, "\n"), refs
}

// draftListItemContentIndent reports the column at which a list item's content
// begins, for a line that opens one — "  - b" gives 4. It recognises the
// CommonMark markers ("-", "*", "+", "1." and "1)") and requires whitespace and
// then content after the marker, so "*emphasis*", a "---" thematic break and a
// lone "-" setext underline are all correctly not list items.
//
// Only the width matters: the caller uses it as the base an indented code block
// must clear, never to rewrite the line.
func draftListItemContentIndent(line string) (int, bool) {
	col := 0
	i := 0
	for ; i < len(line); i++ {
		switch line[i] {
		case ' ':
			col++
			continue
		case '\t':
			col += 4 - col%4
			continue
		}
		break
	}
	if i == len(line) {
		return 0, false
	}

	switch c := line[i]; {
	case c == '-' || c == '*' || c == '+':
		i++
		col++
	case c >= '0' && c <= '9':
		n := 0
		for i < len(line) && line[i] >= '0' && line[i] <= '9' && n < 9 {
			i++
			col++
			n++
		}
		if i >= len(line) || (line[i] != '.' && line[i] != ')') {
			return 0, false
		}
		i++
		col++
	default:
		return 0, false
	}
	markerEnd := col

	// At least one space or tab, then actual content.
	pad := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		if line[i] == '\t' {
			col += 4 - col%4
		} else {
			col++
		}
		i++
		pad = col - markerEnd
	}
	if pad == 0 || i == len(line) {
		return 0, false
	}
	if pad > 4 {
		// CommonMark caps the marker's padding at four columns; beyond that the
		// content is an indented code block inside an item whose content starts
		// one column after the marker.
		return markerEnd + 1, true
	}
	return col, true
}

// liftMentionRefs replaces every reference literal on one line with its index
// placeholder, appending the parsed references in source order.
func liftMentionRefs(line string, refs *[]mentionRef) string {
	if !strings.Contains(line, "<") {
		return line
	}
	return draftMentionRe.ReplaceAllStringFunc(line, func(literal string) string {
		ref, ok := parseMentionRef(literal)
		if !ok {
			return literal
		}
		placeholder := draftPlaceholder(len(*refs))
		*refs = append(*refs, ref)
		return placeholder
	})
}

// expandPlaceholders restores the original reference literals in a text run.
// Code spans, preformatted blocks and link labels use it: they must show what
// the user wrote, never a converted mention, and must certainly never ship a
// private-use rune into the draft.
func (s draftSource) expandPlaceholders(text string) string {
	if len(s.Refs) == 0 || !strings.ContainsRune(text, draftPlaceholderRune) {
		return text
	}
	return draftPlaceholderRe.ReplaceAllStringFunc(text, func(placeholder string) string {
		if ref, ok := s.refAt(placeholder); ok {
			return ref.Literal
		}
		return placeholder
	})
}

// splitPlaceholders cuts a text run into alternating literal and reference
// spans. Literal spans are byte-identical to their slice of the input, so
// surrounding words, punctuation and spacing survive unchanged.
func (s draftSource) splitPlaceholders(text string) []sourceSpan {
	if len(s.Refs) == 0 || !strings.ContainsRune(text, draftPlaceholderRune) {
		return []sourceSpan{{Text: text}}
	}

	var spans []sourceSpan
	last := 0
	for _, loc := range draftPlaceholderRe.FindAllStringIndex(text, -1) {
		if loc[0] > last {
			spans = append(spans, sourceSpan{Text: text[last:loc[0]]})
		}
		ref, ok := s.refAt(text[loc[0]:loc[1]])
		if !ok {
			// Unreachable — every placeholder this function sees was minted by
			// draftPlaceholder against Refs — but if it ever happened, skipping
			// the span would fold the placeholder into the NEXT literal run and
			// ship a U+E000 into the draft. Emitting it as its own literal keeps
			// it visible to the caller's expandPlaceholders/normalisation instead
			// of laundering it, and makes the leak structurally impossible to
			// reach by this route.
			spans = append(spans, sourceSpan{Text: text[loc[0]:loc[1]]})
			last = loc[1]
			continue
		}
		spans = append(spans, sourceSpan{Ref: &ref})
		last = loc[1]
	}
	if last < len(text) {
		spans = append(spans, sourceSpan{Text: text[last:]})
	}
	return spans
}

// refAt resolves a placeholder to the reference it addresses.
func (s draftSource) refAt(placeholder string) (mentionRef, bool) {
	m := draftPlaceholderRe.FindStringSubmatch(placeholder)
	if m == nil {
		return mentionRef{}, false
	}
	i, err := strconv.Atoi(m[1])
	if err != nil || i < 0 || i >= len(s.Refs) {
		return mentionRef{}, false
	}
	return s.Refs[i], true
}

// draftFence records an open fenced-code block so only a matching fence closes
// it: a ``` run is not closed by ~~~, nor by a shorter run of backticks.
type draftFence struct {
	char byte
	len  int
}

// draftFenceOpen reports whether the line opens a fenced code block.
func draftFenceOpen(line string) (draftFence, bool) {
	t := strings.TrimLeft(line, " \t")
	if t == "" {
		return draftFence{}, false
	}
	c := t[0]
	if c != '`' && c != '~' {
		return draftFence{}, false
	}
	n := 0
	for n < len(t) && t[n] == c {
		n++
	}
	if n < 3 {
		return draftFence{}, false
	}
	// CommonMark: a backtick fence's info string may not contain a backtick.
	if c == '`' && strings.ContainsRune(t[n:], '`') {
		return draftFence{}, false
	}
	return draftFence{char: c, len: n}, true
}

// draftFenceCloses reports whether the line closes the given open fence: the
// same character, at least as long a run, and nothing else on the line.
func draftFenceCloses(line string, f draftFence) bool {
	t := strings.TrimSpace(line)
	if len(t) < f.len {
		return false
	}
	for i := 0; i < len(t); i++ {
		if t[i] != f.char {
			return false
		}
	}
	return true
}

// leadingIndentWidth returns the column width of a line's leading whitespace,
// expanding tabs to the next multiple of 4 as CommonMark does. A bare tab is
// therefore worth 4 columns, so "\t• x" is indented code and not a list item.
func leadingIndentWidth(line string) int {
	w := 0
	for _, r := range line {
		switch r {
		case ' ':
			w++
		case '\t':
			w += 4 - w%4
		default:
			return w
		}
	}
	return w
}
