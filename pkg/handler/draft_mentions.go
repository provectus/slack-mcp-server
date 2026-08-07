package handler

import (
	"regexp"
	"strings"

	"github.com/slack-go/slack"
)

// mentionKind classifies a Slack reference literal.
type mentionKind string

const (
	mentionUser      mentionKind = "user"
	mentionChannel   mentionKind = "channel"
	mentionUserGroup mentionKind = "usergroup"
	mentionBroadcast mentionKind = "broadcast"

	// The malformed classes: reference syntax the assistant plainly meant as a
	// reference, carrying a code that cannot be one. The commonest case in the
	// field is <@D…> — the assistant pastes the DMChannelID column users_search
	// returns into a person reference, and a D… code is a conversation.
	//
	// These exist so a wrong code is *visible* to the AC 2.4 gate. A grammar
	// strict enough to reject them outright would make them invisible: no
	// reference, nothing to verify, and the raw code sails through into the
	// draft as plain text — which is the defect these classes close.
	mentionBadUser      mentionKind = "invalid_user"
	mentionBadChannel   mentionKind = "invalid_channel"
	mentionBadUserGroup mentionKind = "invalid_usergroup"
)

// malformed reports whether the kind is a reference whose code cannot name what
// its syntax claims. validateMentions refuses these without consulting the
// workspace: it is a grammar fact, not a lookup.
func (k mentionKind) malformed() bool {
	switch k {
	case mentionBadUser, mentionBadChannel, mentionBadUserGroup:
		return true
	}
	return false
}

// mentionRef is one Slack reference lifted out of the markdown source by
// preprocessDraftSource. It is a pure value: parsing never touches the network.
type mentionRef struct {
	// Kind is the reference class.
	Kind mentionKind
	// ID is the U…/W… user, C…/G…/D… channel or S… user-group ID. Empty for a
	// broadcast. For a malformed kind it is the ID-shaped code the assistant
	// supplied, which is precisely the wrong class for its sigil.
	ID string
	// Range is "here", "channel" or "everyone" for a broadcast. Empty otherwise.
	Range string
	// Label is the "|label" suffix Slack sometimes includes. It is parsed for
	// diagnostics only and never emitted: the user/channel/usergroup rich_text
	// elements carry only an ID (slack-go v0.19.0), and Slack resolves the
	// display name client-side, so an embedded label could only ever go stale.
	Label string
	// Literal is the exact source text this reference was lifted from, label
	// included. Code spans and preformatted blocks restore it verbatim.
	Literal string
}

// The reference grammar. One alternation, applied both to the raw source (by
// preprocessDraftSource) and to the loss check's want side (by
// canonicalizeReferences), so the two can never disagree about what a reference
// is.
//
// Each ID class matches any plausible Slack ID shape — an uppercase letter
// followed by at least two uppercase alphanumerics — and the *prefix decides the
// class after the match*, in mentionRefFromMatch. Matching first and classifying
// second is deliberate: a prefix-strict pattern silently fails to match a wrong
// code, and something that never matches can never be refused. See the
// mentionBadUser comment.
const (
	mentionIDPattern = `[A-Z][A-Z0-9]{2,}`

	mentionUserPattern      = `<@(` + mentionIDPattern + `)(?:\|([^>]*))?>`
	mentionChannelPattern   = `<#(` + mentionIDPattern + `)(?:\|([^>]*))?>`
	mentionUserGroupPattern = `<!subteam\^(` + mentionIDPattern + `)(?:\|([^>]*))?>`
	mentionBroadcastPattern = `<!(here|channel|everyone)(?:\|([^>]*))?>`
)

var mentionAlternation = strings.Join([]string{
	mentionUserPattern,
	mentionChannelPattern,
	mentionUserGroupPattern,
	mentionBroadcastPattern,
}, "|")

var (
	// draftMentionRe scans text for reference literals.
	draftMentionRe = regexp.MustCompile(mentionAlternation)
	// draftMentionAnchoredRe matches a string that is exactly one reference.
	draftMentionAnchoredRe = regexp.MustCompile(`\A(?:` + mentionAlternation + `)\z`)
)

// parseMentionRef parses a complete reference literal. It reports false for
// anything that is not exactly one reference — "<@lowercase>" and "<@U>" are not
// ID-shaped at all, "<#C123" is unterminated — which are left to goldmark's
// existing handling untouched.
//
// A reference whose syntax is right but whose code is wrong ("<@D0BJE66FPU5>")
// DOES parse, as one of the malformed kinds. It is a reference the assistant
// wrote and got wrong, and the write gate has to see it to refuse it.
func parseMentionRef(literal string) (mentionRef, bool) {
	m := draftMentionAnchoredRe.FindStringSubmatch(literal)
	if m == nil {
		return mentionRef{}, false
	}
	return mentionRefFromMatch(m), true
}

// mentionRefFromMatch builds a mentionRef from a draftMentionRe submatch. Group
// pairs run in alternation order: user, channel, usergroup, broadcast. The
// sigil fixes what the author meant; the ID's prefix decides whether the code
// they supplied can actually be that.
func mentionRefFromMatch(m []string) mentionRef {
	ref := mentionRef{Literal: m[0]}
	switch {
	case m[1] != "":
		ref.ID, ref.Label = m[1], m[2]
		ref.Kind = classifyMentionID(ref.ID, "UW", mentionUser, mentionBadUser)
	case m[3] != "":
		ref.ID, ref.Label = m[3], m[4]
		ref.Kind = classifyMentionID(ref.ID, "CGD", mentionChannel, mentionBadChannel)
	case m[5] != "":
		ref.ID, ref.Label = m[5], m[6]
		ref.Kind = classifyMentionID(ref.ID, "S", mentionUserGroup, mentionBadUserGroup)
	default:
		ref.Kind, ref.Range, ref.Label = mentionBroadcast, m[7], m[8]
	}
	return ref
}

// classifyMentionID returns ok when the ID starts with one of the prefixes that
// class permits, and bad otherwise.
func classifyMentionID(id, prefixes string, ok, bad mentionKind) mentionKind {
	if id != "" && strings.IndexByte(prefixes, id[0]) >= 0 {
		return ok
	}
	return bad
}

// malformedReason explains, for the calling assistant, why a malformed
// reference's code cannot be what its syntax claims — naming the class the code
// actually belongs to whenever the prefix makes that obvious, so the model can
// self-correct in one step instead of guessing.
func (r mentionRef) malformedReason() string {
	var wanted, starts string
	switch r.Kind {
	case mentionBadUser:
		wanted, starts = "a user ID", "user IDs start with U or W"
	case mentionBadChannel:
		wanted, starts = "a conversation ID", "conversation IDs start with C, G or D"
	case mentionBadUserGroup:
		wanted, starts = "a user-group ID", "user-group IDs start with S"
	default:
		return ""
	}

	looksLike := starts
	if r.ID != "" {
		switch r.ID[0] {
		case 'U', 'W':
			looksLike = "a " + string(r.ID[0]) + "… code identifies a person"
		case 'C', 'G', 'D':
			looksLike = "a " + string(r.ID[0]) + "… code identifies a conversation"
		case 'S':
			looksLike = "an S… code identifies a user group"
		}
	}
	// The prefixes handled above are, by construction, never the class this
	// reference wanted — a code with the right prefix would not be malformed —
	// so looksLike can only ever name a genuinely different class.
	return r.ID + " is not " + wanted + " (" + looksLike + ")"
}

// canonicalLiteral returns the label-free textual form of a reference. It is the
// shared vocabulary between the loss check's two halves: canonicalizeReferences
// rewrites the want side to it, and flattenRichTextContent writes it for each
// mention element on the got side.
//
// Broadcasts canonicalise to "@here" / "@channel" / "@everyone" rather than
// "<!here>", matching what the converter emits. Both tokenise to the single word
// "here", so the choice is neutral for the loss check either way.
// A malformed reference canonicalises under the sigil it was written with, not
// the class its code belongs to: "<@D0BJE66FPU5>" stays "<@D0BJE66FPU5>". The
// refusal must quote back what the assistant actually wrote, and the loss check
// must see the same string on both sides.
func (r mentionRef) canonicalLiteral() string {
	switch r.Kind {
	case mentionUser, mentionBadUser:
		return "<@" + r.ID + ">"
	case mentionChannel, mentionBadChannel:
		return "<#" + r.ID + ">"
	case mentionUserGroup, mentionBadUserGroup:
		return "<!subteam^" + r.ID + ">"
	case mentionBroadcast:
		return "@" + r.Range
	default:
		return r.Literal
	}
}

// element returns the rich_text section element this reference becomes. style is
// the style of the text run the reference was cut out of, so a bold sentence
// keeps its mention bold.
//
// Broadcasts deliberately return a plain text element reading "@here" /
// "@channel" / "@everyone": a slack.RichTextSectionBroadcastElement is NEVER
// constructed by the converter. @here, @channel and @everyone notify large
// numbers of people at once, and the assistant must not be able to arm a
// workspace-wide alert on the user's behalf. A text run is inert by
// construction, reads correctly, and converges with what happens when the
// assistant simply types "@here" as prose.
func (r mentionRef) element(style *slack.RichTextSectionTextStyle) slack.RichTextSectionElement {
	switch r.Kind {
	case mentionUser:
		return &slack.RichTextSectionUserElement{
			Type:   slack.RTSEUser,
			UserID: r.ID,
			Style:  style,
		}
	case mentionChannel:
		return &slack.RichTextSectionChannelElement{
			Type:      slack.RTSEChannel,
			ChannelID: r.ID,
			Style:     style,
		}
	case mentionUserGroup:
		// slack-go v0.19.0's usergroup element carries only Type and
		// UsergroupID — no style field, and no place for a display name.
		return &slack.RichTextSectionUserGroupElement{
			Type:        slack.RTSEUserGroup,
			UsergroupID: r.ID,
		}
	default:
		// Broadcasts (above) and the three malformed classes land here as inert
		// text. For a malformed reference this is defence in depth: it is
		// refused by validateMentions long before conversion output is used, but
		// the converter must be incapable of minting a user element around a D…
		// conversation code even if that gate is ever bypassed.
		return &slack.RichTextSectionTextElement{
			Type:  slack.RTSEText,
			Text:  r.canonicalLiteral(),
			Style: style,
		}
	}
}

// splitSectionElements expands the reference placeholders in one section's
// element list, cutting each carrying text run into alternating text and
// user/channel/usergroup elements.
//
// Two runs are expanded rather than converted:
//
//   - Code-styled runs. A backtick code span is invisible to the line scan, so
//     `<@U123>` does get a placeholder; showing the reader a live mention inside
//     what they wrote as code would be wrong, and leaving the raw placeholder
//     would ship a private-use rune into the draft.
//   - Link elements. A reference in a link label or destination stays literal
//     text for the same reason; rich_text links carry a flat string label with
//     no room for a nested element.
func splitSectionElements(src draftSource, elems []slack.RichTextSectionElement) []slack.RichTextSectionElement {
	if len(src.Refs) == 0 {
		return elems
	}

	out := make([]slack.RichTextSectionElement, 0, len(elems))
	for _, e := range elems {
		switch el := e.(type) {
		case *slack.RichTextSectionTextElement:
			if el.Style != nil && el.Style.Code {
				el.Text = src.expandPlaceholders(el.Text)
				out = append(out, el)
				continue
			}
			for _, span := range src.splitPlaceholders(el.Text) {
				if span.Ref != nil {
					out = append(out, span.Ref.element(el.Style))
					continue
				}
				// Slack's drafts API rejects a section containing a text element
				// with an empty string ("invalid_message"), which is exactly what
				// a run consisting only of a reference would leave behind.
				if span.Text == "" {
					continue
				}
				out = append(out, &slack.RichTextSectionTextElement{
					Type:  slack.RTSEText,
					Text:  span.Text,
					Style: el.Style,
				})
			}

		case *slack.RichTextSectionLinkElement:
			el.Text = src.expandPlaceholders(el.Text)
			el.URL = src.expandPlaceholders(el.URL)
			out = append(out, el)

		default:
			out = append(out, e)
		}
	}
	return out
}

// collectMentionRefs returns every live mention in a built rich_text block, in
// document order, deduplicated by reference. It walks the same four container
// kinds flattenRichTextContent does, so a mention inside a list item, quote or
// preformatted block is found exactly like one in a bare section.
//
// Deduplication is what keeps a cold cache from turning "thanks <@U1>, <@U1>
// and <@U1>" into three users.info calls, and it is by canonical literal rather
// than by bare ID so a user and a channel could never collide.
//
// Broadcast elements are skipped: the converter never emits one, and a
// broadcast has nothing to resolve.
//
// Walking the elements is not enough on its own, so the text runs of live
// blocks are scanned for reference literals too. Malformed references are the
// designed reason — they are deliberately never turned into an element — but
// the scan is the backstop for any reference that reaches a live run as raw
// text by some other route (see the comment at the scan itself). Code-styled
// runs and preformatted blocks are excluded, because a reference the user wrote
// as code is content to be read literally.
func collectMentionRefs(rtb *slack.RichTextBlock) []mentionRef {
	if rtb == nil {
		return nil
	}

	var refs []mentionRef
	seen := make(map[string]bool)
	add := func(ref mentionRef) {
		key := ref.canonicalLiteral()
		if seen[key] {
			return
		}
		seen[key] = true
		refs = append(refs, ref)
	}

	// scanText is false for preformatted blocks: their contents are verbatim
	// code, never a live reference.
	collect := func(elems []slack.RichTextSectionElement, scanText bool) {
		for _, e := range elems {
			switch el := e.(type) {
			case *slack.RichTextSectionUserElement:
				add(mentionRef{Kind: mentionUser, ID: el.UserID})
			case *slack.RichTextSectionChannelElement:
				add(mentionRef{Kind: mentionChannel, ID: el.ChannelID})
			case *slack.RichTextSectionUserGroupElement:
				add(mentionRef{Kind: mentionUserGroup, ID: el.UsergroupID})
			case *slack.RichTextSectionTextElement:
				if !scanText || (el.Style != nil && el.Style.Code) {
					continue
				}
				for _, m := range draftMentionRe.FindAllStringSubmatch(el.Text, -1) {
					// Every reference still sitting as literal text in a LIVE
					// run is collected, malformed or not. It is tempting to
					// assume a well-formed reference in live text is a literal
					// the converter deliberately preserved — it is not. The
					// pre-parse line scan and goldmark disagree about a handful
					// of inputs (a reference indented four columns inside a list
					// item is the known one: the scanner calls it indented code
					// and skips it, goldmark calls it list content and renders
					// it live), and every such disagreement leaves a raw "<@U…>"
					// as visible text in a live block AND invisible to the AC
					// 2.4 gate. Collecting unconditionally closes that class
					// whatever route produced it: the reference is verified, and
					// an unverifiable one refuses the write instead of shipping.
					//
					// Safe by construction: broadcasts already render as "@here"
					// and never re-match the grammar, code-styled runs and
					// preformatted blocks are skipped above, and link elements
					// are never scanned — so a reference the user wrote to be
					// read literally still costs no lookup.
					add(mentionRefFromMatch(m))
				}
			}
		}
	}

	for _, el := range rtb.Elements {
		switch e := el.(type) {
		case *slack.RichTextSection:
			collect(e.Elements, true)
		case *slack.RichTextList:
			for _, item := range e.Elements {
				if s, ok := item.(*slack.RichTextSection); ok {
					collect(s.Elements, true)
				}
			}
		case *slack.RichTextQuote:
			collect(e.Elements, true)
		case *slack.RichTextPreformatted:
			collect(e.Elements, false)
		}
	}
	return refs
}

// neutralizeBroadcasts rewrites every broadcast literal in a string to its inert
// canonical form — "<!here>" and the labelled "<!here|@here>" both become
// "@here", and likewise for channel and everyone — leaving every other reference
// class byte-for-byte untouched.
//
// It exists for the notification/fallback text of conversations_add_message.
// "<!here>" is Slack's own on-the-wire encoding of a live @here broadcast, so a
// caller's raw markdown carrying that literal would put a genuine broadcast
// encoding in the field Slack uses to build the notification, even though the
// rich_text blocks alongside it render the same reference as inert text (see
// mentionRef.element). Rewriting the fallback to the same "@here" the blocks
// already contain makes the two agree, and makes functional spec §2.3 — the
// assistant can never raise a workspace-wide alert — a property of what we send
// rather than of undocumented Slack behaviour.
//
// User, channel and user-group references are deliberately NOT rewritten:
// Slack resolves those server-side when building the notification, and that is
// the wanted behaviour for a message that legitimately mentions someone.
func neutralizeBroadcasts(s string) string {
	return draftMentionRe.ReplaceAllStringFunc(s, func(literal string) string {
		ref, ok := parseMentionRef(literal)
		if !ok || ref.Kind != mentionBroadcast {
			return literal
		}
		return ref.canonicalLiteral()
	})
}

// canonicalizeReferences rewrites every reference literal in a string to its
// canonical, label-free form. draftContentLoss applies it to the want side so a
// labelled reference stops contributing a token the block can never produce:
// "<@U123|dana>" would otherwise put "dana" in want while the emitted user
// element carries only the ID, manufacturing a spurious "conversion dropped
// content" refusal.
func canonicalizeReferences(s string) string {
	return draftMentionRe.ReplaceAllStringFunc(s, func(literal string) string {
		ref, ok := parseMentionRef(literal)
		if !ok {
			return literal
		}
		return ref.canonicalLiteral()
	})
}
