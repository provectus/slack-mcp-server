package handler

import (
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

// All nine reference forms in the tech spec's grammar table parse, and each
// yields the right kind, ID/range and label.
func TestUnitParseMentionRef_AllGrammarForms(t *testing.T) {
	tests := []struct {
		name      string
		literal   string
		kind      mentionKind
		id        string
		rangeName string
		label     string
		canonical string
	}{
		{"user", "<@U0AAAAA1>", mentionUser, "U0AAAAA1", "", "", "<@U0AAAAA1>"},
		{"user labelled", "<@U0AAAAA1|dana>", mentionUser, "U0AAAAA1", "", "dana", "<@U0AAAAA1>"},
		{"user W prefix", "<@W0AAAAA1>", mentionUser, "W0AAAAA1", "", "", "<@W0AAAAA1>"},
		{"channel", "<#C0AAAAA1>", mentionChannel, "C0AAAAA1", "", "", "<#C0AAAAA1>"},
		{"channel labelled", "<#C0AAAAA1|general>", mentionChannel, "C0AAAAA1", "", "general", "<#C0AAAAA1>"},
		{"usergroup", "<!subteam^S0AAAAA1>", mentionUserGroup, "S0AAAAA1", "", "", "<!subteam^S0AAAAA1>"},
		{"usergroup labelled", "<!subteam^S0AAAAA1|@eng>", mentionUserGroup, "S0AAAAA1", "", "@eng", "<!subteam^S0AAAAA1>"},
		{"broadcast here", "<!here>", mentionBroadcast, "", "here", "", "@here"},
		{"broadcast channel", "<!channel>", mentionBroadcast, "", "channel", "", "@channel"},
		{"broadcast everyone", "<!everyone>", mentionBroadcast, "", "everyone", "", "@everyone"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, ok := parseMentionRef(tt.literal)
			if !ok {
				t.Fatalf("parseMentionRef(%q) did not parse", tt.literal)
			}
			if ref.Kind != tt.kind {
				t.Errorf("kind: got %q, want %q", ref.Kind, tt.kind)
			}
			if ref.ID != tt.id {
				t.Errorf("id: got %q, want %q", ref.ID, tt.id)
			}
			if ref.Range != tt.rangeName {
				t.Errorf("range: got %q, want %q", ref.Range, tt.rangeName)
			}
			if ref.Label != tt.label {
				t.Errorf("label: got %q, want %q", ref.Label, tt.label)
			}
			if ref.Literal != tt.literal {
				t.Errorf("literal: got %q, want %q", ref.Literal, tt.literal)
			}
			if got := ref.canonicalLiteral(); got != tt.canonical {
				t.Errorf("canonicalLiteral: got %q, want %q", got, tt.canonical)
			}
		})
	}
}

// Anything that is not exactly one reference must be left to goldmark's existing
// handling, so it can never be lifted out of the source or converted.
//
// These are near misses because they are not ID-shaped at all — lowercase text,
// a one-letter code, an unterminated bracket. A reference that IS ID-shaped but
// carries a code of the wrong class ("<@D0BJE66FPU5>", "<!subteam^X0AAAAA1>") is
// NOT a near miss: it parses as a malformed kind so the write gate can refuse
// it. See TestUnitParseMentionRef_MalformedReferences.
func TestUnitParseMentionRef_NearMisses(t *testing.T) {
	for _, literal := range []string{
		"<@lowercase>",
		"<@U>",
		"<@u0AAAAA1>",
		"<!subteam^S0AAAAA1",
		"<#C123",
		"<#c0AAAAA1>",
		"<!heretic>",
		"<@U0AAAAA1",
		"@U0AAAAA1",
		"< @U0AAAAA1>",
		"<@U0AAAAA1><@U0BBBBB2>",
	} {
		t.Run(literal, func(t *testing.T) {
			if ref, ok := parseMentionRef(literal); ok {
				t.Fatalf("parseMentionRef(%q) parsed as %+v, want no match", literal, ref)
			}
		})
	}
}

// Reference syntax carrying a code of the wrong class parses as a malformed
// kind — the whole point being that it becomes visible to validateMentions
// instead of sliding through as plain text.
//
// The first case is the live-verified regression: the assistant pasted the
// DMChannelID column users_search returns into a person reference, and the
// draft was written with "<@D0BJE66FPU5>" as literal text.
func TestUnitParseMentionRef_MalformedReferences(t *testing.T) {
	tests := []struct {
		literal   string
		kind      mentionKind
		id        string
		canonical string
		reason    string
	}{
		{"<@D0BJE66FPU5>", mentionBadUser, "D0BJE66FPU5", "<@D0BJE66FPU5>",
			"D0BJE66FPU5 is not a user ID (a D… code identifies a conversation)"},
		{"<@C0AAAAA1>", mentionBadUser, "C0AAAAA1", "<@C0AAAAA1>",
			"C0AAAAA1 is not a user ID (a C… code identifies a conversation)"},
		{"<@S0AAAAA1>", mentionBadUser, "S0AAAAA1", "<@S0AAAAA1>",
			"S0AAAAA1 is not a user ID (an S… code identifies a user group)"},
		{"<@T0AAAAA1>", mentionBadUser, "T0AAAAA1", "<@T0AAAAA1>",
			"T0AAAAA1 is not a user ID (user IDs start with U or W)"},
		{"<#S123ABC>", mentionBadChannel, "S123ABC", "<#S123ABC>",
			"S123ABC is not a conversation ID (an S… code identifies a user group)"},
		{"<#U0AAAAA1>", mentionBadChannel, "U0AAAAA1", "<#U0AAAAA1>",
			"U0AAAAA1 is not a conversation ID (a U… code identifies a person)"},
		{"<!subteam^X0AAAAA1>", mentionBadUserGroup, "X0AAAAA1", "<!subteam^X0AAAAA1>",
			"X0AAAAA1 is not a user-group ID (user-group IDs start with S)"},
		// A label does not rescue a wrong code, and is dropped from the canonical
		// form exactly as it is for a well-formed reference.
		{"<@D0BJE66FPU5|dana>", mentionBadUser, "D0BJE66FPU5", "<@D0BJE66FPU5>",
			"D0BJE66FPU5 is not a user ID (a D… code identifies a conversation)"},
	}

	for _, tt := range tests {
		t.Run(tt.literal, func(t *testing.T) {
			ref, ok := parseMentionRef(tt.literal)
			if !ok {
				t.Fatalf("parseMentionRef(%q) did not parse — a malformed reference must be visible to the write gate", tt.literal)
			}
			if ref.Kind != tt.kind {
				t.Errorf("kind: got %q, want %q", ref.Kind, tt.kind)
			}
			if !ref.Kind.malformed() {
				t.Errorf("kind %q must report malformed()", ref.Kind)
			}
			if ref.ID != tt.id {
				t.Errorf("id: got %q, want %q", ref.ID, tt.id)
			}
			if got := ref.canonicalLiteral(); got != tt.canonical {
				t.Errorf("canonicalLiteral: got %q, want %q", got, tt.canonical)
			}
			if got := ref.malformedReason(); got != tt.reason {
				t.Errorf("malformedReason:\n got: %q\nwant: %q", got, tt.reason)
			}
		})
	}
}

// The well-formed classes must keep reporting malformed() == false, so widening
// the grammar cannot quietly start refusing valid references.
func TestUnitMentionKindMalformed_WellFormedClassesUnaffected(t *testing.T) {
	for _, literal := range []string{
		"<@U0AAAAA1>", "<@W0AAAAA1|dana>",
		"<#C0AAAAA1>", "<#G0AAAAA1>", "<#D0BJE66FPU5>",
		"<!subteam^S0AAAAA1|@eng>",
		"<!here>", "<!channel>", "<!everyone>",
	} {
		t.Run(literal, func(t *testing.T) {
			ref, ok := parseMentionRef(literal)
			if !ok {
				t.Fatalf("parseMentionRef(%q) did not parse", literal)
			}
			if ref.Kind.malformed() {
				t.Fatalf("%q classified as malformed (%q) — valid references must be unaffected", literal, ref.Kind)
			}
		})
	}
}

// A malformed reference must never become a user, channel or usergroup element.
// validateMentions refuses it first, but the converter has to be independently
// incapable of minting a mention around a conversation code.
func TestUnitSplitSectionElements_MalformedNeverBecomesAMentionElement(t *testing.T) {
	for _, literal := range []string{"<@D0BJE66FPU5>", "<#S123ABC>", "<!subteam^X0AAAAA1>"} {
		t.Run(literal, func(t *testing.T) {
			input := "bad ref check " + literal + " here"
			rtb, err := markdownToRichTextBlock(input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, e := range allSectionElements(rtb) {
				switch el := e.(type) {
				case *slack.RichTextSectionUserElement:
					t.Fatalf("malformed reference became a user element (%q)", el.UserID)
				case *slack.RichTextSectionChannelElement:
					t.Fatalf("malformed reference became a channel element (%q)", el.ChannelID)
				case *slack.RichTextSectionUserGroupElement:
					t.Fatalf("malformed reference became a usergroup element (%q)", el.UsergroupID)
				}
			}
			assertNoPlaceholders(t, rtb)

			// It is still collected, which is what lets validateMentions refuse.
			refs := collectMentionRefs(rtb)
			if len(refs) != 1 || !refs[0].Kind.malformed() || refs[0].canonicalLiteral() != literal {
				t.Fatalf("expected exactly one malformed reference %q, got %+v", literal, refs)
			}
		})
	}
}

// A malformed reference the user wrote as code is content, not a reference —
// the same rule that leaves a well-formed <@U…> in a code span alone. Refusing
// a draft because it quotes a bad ID inside backticks would be a false positive.
func TestUnitCollectMentionRefs_MalformedInCodeIsNotCollected(t *testing.T) {
	input := "Use `<@D0BJE66FPU5>` literally.\n\n```\ncurl -d '<@D0BJE66FPU5>'\n```\n"

	rtb, err := markdownToRichTextBlock(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if refs := collectMentionRefs(rtb); len(refs) != 0 {
		t.Fatalf("a malformed reference inside code must not be collected, got %+v", refs)
	}
}

// A near-miss in real source text must survive preprocessing untouched: no
// placeholder, no reference table entry.
func TestUnitPreprocessDraftSource_NearMissesNotLifted(t *testing.T) {
	input := "Hi <@lowercase> and <@U> and <@u0AAAAA1> and <#C123 unterminated.\n"

	src, err := preprocessDraftSource(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src.Text != input {
		t.Fatalf("near-miss text was rewritten:\n got: %q\nwant: %q", src.Text, input)
	}
	if len(src.Refs) != 0 {
		t.Fatalf("expected no lifted references, got %+v", src.Refs)
	}
}

// canonicalLiteral must round-trip through parseMentionRef for the three live
// classes: the canonical form is what the loss check and the readback fallback
// both speak, so it has to remain a parseable reference.
func TestUnitCanonicalLiteral_RoundTrips(t *testing.T) {
	for _, literal := range []string{
		"<@U0AAAAA1|dana>",
		"<#C0AAAAA1|general>",
		"<!subteam^S0AAAAA1|@eng>",
	} {
		t.Run(literal, func(t *testing.T) {
			ref, ok := parseMentionRef(literal)
			if !ok {
				t.Fatalf("parseMentionRef(%q) did not parse", literal)
			}
			again, ok := parseMentionRef(ref.canonicalLiteral())
			if !ok {
				t.Fatalf("canonicalLiteral %q does not parse back", ref.canonicalLiteral())
			}
			if again.Kind != ref.Kind || again.ID != ref.ID || again.Range != ref.Range {
				t.Fatalf("round-trip changed the reference: %+v -> %+v", ref, again)
			}
			if again.Label != "" {
				t.Errorf("canonical form must be label-free, got label %q", again.Label)
			}
			if got := again.canonicalLiteral(); got != ref.canonicalLiteral() {
				t.Errorf("canonicalLiteral is not idempotent: %q vs %q", got, ref.canonicalLiteral())
			}
		})
	}
}

// canonicalizeReferences strips labels and normalises broadcasts, leaving every
// other byte of the string alone.
func TestUnitCanonicalizeReferences(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Hi <@U0AAAAA1|dana>, see <#C0AAAAA1|general>.", "Hi <@U0AAAAA1>, see <#C0AAAAA1>."},
		{"cc <!subteam^S0AAAAA1|@eng> please", "cc <!subteam^S0AAAAA1> please"},
		{"<!here> and <!channel> and <!everyone>", "@here and @channel and @everyone"},
		{"no references here at all", "no references here at all"},
		{"near miss <@lowercase> untouched", "near miss <@lowercase> untouched"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := canonicalizeReferences(tt.in); got != tt.want {
				t.Fatalf("canonicalizeReferences(%q)\n got: %q\nwant: %q", tt.in, got, tt.want)
			}
		})
	}
}

// Lifting a reference out and splitting it back in must be byte-identical for
// everything around it: surrounding words, punctuation and spacing are the
// user's text and may not shift by a single byte.
func TestUnitSplitPlaceholders_SurroundingTextByteIdentical(t *testing.T) {
	inputs := []string{
		"Hi <@U0AAAAA1>, please ping <#C0AAAAA1> and <!subteam^S0AAAAA1|@eng> — thanks!",
		"<@U0AAAAA1>: leading reference, trailing text.",
		"trailing reference <@U0AAAAA1>",
		"<@U0AAAAA1><@U0BBBBB2> back to back",
		"double  spaces   around <@U0AAAAA1>  preserved",
		"no references at all",
	}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			src, err := preprocessDraftSource(input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var sb strings.Builder
			for _, span := range src.splitPlaceholders(src.Text) {
				if span.Ref != nil {
					sb.WriteString(span.Ref.Literal)
					continue
				}
				sb.WriteString(span.Text)
			}
			if got := sb.String(); got != input {
				t.Fatalf("reassembly is not byte-identical\n got: %q\nwant: %q", got, input)
			}
		})
	}
}

// expandPlaceholders restores the original literal, label included.
func TestUnitExpandPlaceholders_RestoresOriginalLiteral(t *testing.T) {
	input := "Ping <@U0AAAAA1|dana> and <!here> now."
	src, err := preprocessDraftSource(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(src.Refs) != 2 {
		t.Fatalf("expected 2 lifted references, got %d: %+v", len(src.Refs), src.Refs)
	}
	if got := src.expandPlaceholders(src.Text); got != input {
		t.Fatalf("expandPlaceholders\n got: %q\nwant: %q", got, input)
	}
}

// The collision guard: restoring a user-typed U+E000 as part of a placeholder
// would be a silent content change, so the converter refuses instead.
func TestUnitPreprocessDraftSource_PlaceholderCollisionErrors(t *testing.T) {
	if _, err := preprocessDraftSource("before \uE000 after"); err == nil {
		t.Fatal("expected an error for input containing U+E000")
	}
	if _, err := markdownToRichTextBlock("before \uE000 after"); err == nil {
		t.Fatal("expected markdownToRichTextBlock to surface the collision error")
	}
}

// splitSectionElements cuts a text run into the right element sequence and keeps
// every surrounding byte intact.
func TestUnitSplitSectionElements_TextAndMentionSequence(t *testing.T) {
	input := "Hi <@U0AAAAA1>, ping <#C0AAAAA1> and <!subteam^S0AAAAA1|@eng> — thanks!"
	src, err := preprocessDraftSource(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	elems := splitSectionElements(src, []slack.RichTextSectionElement{
		&slack.RichTextSectionTextElement{Type: slack.RTSEText, Text: src.Text},
	})

	if len(elems) != 7 {
		t.Fatalf("expected 7 elements, got %d: %#v", len(elems), elems)
	}
	assertTextElement(t, elems[0], "Hi ")
	assertUserElement(t, elems[1], "U0AAAAA1")
	assertTextElement(t, elems[2], ", ping ")
	assertChannelElement(t, elems[3], "C0AAAAA1")
	assertTextElement(t, elems[4], " and ")
	assertUserGroupElement(t, elems[5], "S0AAAAA1")
	assertTextElement(t, elems[6], " — thanks!")

	// The tail text after the last reference must survive too.
	elems = splitSectionElements(src, []slack.RichTextSectionElement{
		&slack.RichTextSectionTextElement{Type: slack.RTSEText, Text: src.Text + " tail"},
	})
	last, ok := elems[len(elems)-1].(*slack.RichTextSectionTextElement)
	if !ok || last.Text != " — thanks! tail" {
		t.Fatalf("tail text was lost: %#v", elems[len(elems)-1])
	}
}

// A mention inside a bold run keeps the run's style, so the sentence renders as
// the user wrote it.
func TestUnitSplitSectionElements_PreservesStyle(t *testing.T) {
	src, err := preprocessDraftSource("hi <@U0AAAAA1> there")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	bold := &slack.RichTextSectionTextStyle{Bold: true}
	elems := splitSectionElements(src, []slack.RichTextSectionElement{
		&slack.RichTextSectionTextElement{Type: slack.RTSEText, Text: src.Text, Style: bold},
	})
	user, ok := elems[1].(*slack.RichTextSectionUserElement)
	if !ok {
		t.Fatalf("expected a user element, got %T", elems[1])
	}
	if user.Style == nil || !user.Style.Bold {
		t.Fatalf("expected the user element to keep the bold style, got %+v", user.Style)
	}
}

// A code-styled run expands rather than converts: what the user wrote as code
// must read as code, and must never ship a private-use rune.
func TestUnitSplitSectionElements_CodeStyleExpandsInsteadOfConverting(t *testing.T) {
	src, err := preprocessDraftSource("`<@U0AAAAA1>`")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	elems := splitSectionElements(src, []slack.RichTextSectionElement{
		&slack.RichTextSectionTextElement{
			Type:  slack.RTSEText,
			Text:  strings.Trim(src.Text, "`"),
			Style: &slack.RichTextSectionTextStyle{Code: true},
		},
	})
	if len(elems) != 1 {
		t.Fatalf("expected a single element, got %d: %#v", len(elems), elems)
	}
	assertTextElement(t, elems[0], "<@U0AAAAA1>")
}

// §2.3 enforcement. @here, @channel and @everyone notify large numbers of people
// at once; the converter must never be able to arm one. This is the assertion
// that pins that decision down — a broadcast element must not appear anywhere in
// converter output, and the reference must still be visible and readable.
func TestUnitMarkdownToRichTextBlock_BroadcastsNeverProduceBroadcastElement(t *testing.T) {
	cases := []struct{ literal, want string }{
		{"<!here>", "@here"},
		{"<!channel>", "@channel"},
		{"<!everyone>", "@everyone"},
		{"<!here|here>", "@here"},
	}
	for _, tc := range cases {
		t.Run(tc.literal, func(t *testing.T) {
			input := "Heads up " + tc.literal + " — the deploy is done.\n\n" +
				"- item " + tc.literal + "\n\n" +
				"> quote " + tc.literal + "\n"

			rtb, err := markdownToRichTextBlock(input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			for _, e := range allSectionElements(rtb) {
				if b, ok := e.(*slack.RichTextSectionBroadcastElement); ok {
					t.Fatalf("converter emitted a broadcast element (range %q) — a workspace-wide alert must never be armed", b.Range)
				}
			}

			flat := flattenRichText(rtb)
			if !strings.Contains(flat, tc.want) {
				t.Fatalf("expected the inert text %q in the output, got %q", tc.want, flat)
			}
			// AC 2.3: never mangled, split across lines, or partly swallowed.
			if strings.Contains(flat, "<!") || strings.Contains(flat, tc.want+">") {
				t.Fatalf("broadcast literal leaked or was mangled: %q", flat)
			}
			assertNoPlaceholders(t, rtb)
			if missing := draftContentLoss(input, rtb); len(missing) > 0 {
				t.Fatalf("broadcast produced spurious content loss: %v", missing)
			}
		})
	}
}

func assertTextElement(t *testing.T, e slack.RichTextSectionElement, want string) {
	t.Helper()
	el, ok := e.(*slack.RichTextSectionTextElement)
	if !ok {
		t.Fatalf("expected a text element, got %T", e)
	}
	if el.Text != want {
		t.Fatalf("text element: got %q, want %q", el.Text, want)
	}
}

func assertUserElement(t *testing.T, e slack.RichTextSectionElement, wantID string) {
	t.Helper()
	el, ok := e.(*slack.RichTextSectionUserElement)
	if !ok {
		t.Fatalf("expected a user element, got %T", e)
	}
	if el.UserID != wantID {
		t.Fatalf("user element: got %q, want %q", el.UserID, wantID)
	}
	if el.Type != slack.RTSEUser {
		t.Fatalf("user element type: got %q, want %q", el.Type, slack.RTSEUser)
	}
}

func assertChannelElement(t *testing.T, e slack.RichTextSectionElement, wantID string) {
	t.Helper()
	el, ok := e.(*slack.RichTextSectionChannelElement)
	if !ok {
		t.Fatalf("expected a channel element, got %T", e)
	}
	if el.ChannelID != wantID {
		t.Fatalf("channel element: got %q, want %q", el.ChannelID, wantID)
	}
	if el.Type != slack.RTSEChannel {
		t.Fatalf("channel element type: got %q, want %q", el.Type, slack.RTSEChannel)
	}
}

func assertUserGroupElement(t *testing.T, e slack.RichTextSectionElement, wantID string) {
	t.Helper()
	el, ok := e.(*slack.RichTextSectionUserGroupElement)
	if !ok {
		t.Fatalf("expected a usergroup element, got %T", e)
	}
	if el.UsergroupID != wantID {
		t.Fatalf("usergroup element: got %q, want %q", el.UsergroupID, wantID)
	}
	if el.Type != slack.RTSEUserGroup {
		t.Fatalf("usergroup element type: got %q, want %q", el.Type, slack.RTSEUserGroup)
	}
}

// TestUnitCollectMentionRefs_WellFormedLiteralInLiveTextIsCollected pins the
// backstop that makes the AC 2.4 gate independent of how a reference got there.
//
// The tempting assumption is that a well-formed reference in a live text run is
// always a literal the converter chose to preserve, so only malformed forms
// need the text scan. That is false: the pre-parse line scan and goldmark can
// disagree about whether a line is code, and every such disagreement leaves a
// raw "<@U…>" as visible text in a live block. Collected unconditionally, it is
// verified — and an unverifiable one refuses the write instead of shipping.
//
// The block here is built by hand precisely because it must not depend on any
// converter route staying broken.
func TestUnitCollectMentionRefs_WellFormedLiteralInLiveTextIsCollected(t *testing.T) {
	live := func(text string) *slack.RichTextSection {
		return &slack.RichTextSection{
			Type:     slack.RTESection,
			Elements: []slack.RichTextSectionElement{&slack.RichTextSectionTextElement{Type: slack.RTSEText, Text: text}},
		}
	}

	rtb := slack.NewRichTextBlock("",
		live("please review <@U0BOGUS1> thanks"),
		&slack.RichTextList{
			Type:     slack.RTEList,
			Style:    slack.RTEListBullet,
			Elements: []slack.RichTextElement{live("see <#C0BOGUS1> and <!subteam^S0BOGUS1>")},
		},
	)

	got := map[string]bool{}
	for _, ref := range collectMentionRefs(rtb) {
		got[ref.canonicalLiteral()] = true
	}
	for _, want := range []string{"<@U0BOGUS1>", "<#C0BOGUS1>", "<!subteam^S0BOGUS1>"} {
		if !got[want] {
			t.Fatalf("well-formed reference %q left as literal text in a live run must still be collected, got %v", want, got)
		}
	}
}

// The backstop must not turn a reference the user wrote as code into a lookup:
// code-styled runs and preformatted blocks stay content to be read literally,
// exactly as before.
func TestUnitCollectMentionRefs_WellFormedLiteralInCodeStaysUncollected(t *testing.T) {
	codeStyled := &slack.RichTextSection{
		Type: slack.RTESection,
		Elements: []slack.RichTextSectionElement{
			&slack.RichTextSectionTextElement{
				Type:  slack.RTSEText,
				Text:  "<@U0BOGUS1>",
				Style: &slack.RichTextSectionTextStyle{Code: true},
			},
		},
	}
	pre := &slack.RichTextPreformatted{
		RichTextSection: slack.RichTextSection{
			Type:     slack.RTEPreformatted,
			Elements: []slack.RichTextSectionElement{&slack.RichTextSectionTextElement{Type: slack.RTSEText, Text: "curl -d '<#C0BOGUS1>'"}},
		},
	}

	if refs := collectMentionRefs(slack.NewRichTextBlock("", codeStyled, pre)); len(refs) != 0 {
		t.Fatalf("a reference written as code must cost no lookup, got %+v", refs)
	}
}
