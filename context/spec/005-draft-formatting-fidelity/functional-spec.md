# Functional Specification: Draft & Message Formatting Fidelity

- **Roadmap Item:** None — defect-driven. Corrects the "Markdown → native Slack rich_text formatting" capability delivered in Phase 2 (Acting on Slack & Self-Hosting).
- **Status:** Draft
- **Author:** Aleksandr Makarov

---

## 1. Overview and Rationale (The "Why")

When the assistant writes a Slack message or draft on the user's behalf, the result is supposed to look like something a person typed in Slack — real bulleted lists, real @-mentions. Two things break that promise.

**Lists come out fake.** The assistant very often writes list items using the bullet character `•`, the same character Slack itself displays. Slack does not recognise it as a list marker, so the result is a run of ordinary text lines that each happen to start with a `•`, separated from the sentence above them by a stray empty line. It looks close enough to fool a glance and wrong enough to need hand-editing. Only the dash form (`- item`) produces a genuine Slack list today. Across the user's recorded message and draft calls, 25 of 49 used the `•` form and only 8 used the dash — so the broken path is the common one.

**Mentions come out as codes.** When the assistant refers to a person, channel, or user group, it uses the reference Slack itself hands back — a short code in angle brackets. Those codes pass straight through to the draft, so the user opens Slack and sees `<@D0BJE66FPU5>` where a name should be. Worse, when the assistant reads its own draft back to check its work, it sees the same raw code, so it cannot tell the user who it actually tagged — or notice that it tagged the wrong thing entirely.

Both faults land on a message the user is about to send to colleagues. The point of having the assistant draft it is that the draft is ready to go; today the user must fix the formatting by hand first, which removes most of the value.

**Desired outcome.** A draft the assistant writes renders in Slack the same way it would if the user had typed it: real lists, real mentions showing real names. And when the assistant reads a draft back, it can see and report who it tagged.

**Success looks like:** a draft containing a bulleted list and a person mention needs no hand-editing before sending, and the assistant's own account of what it wrote names the people it tagged.

---

## 2. Functional Requirements (The "What")

### 2.1 Lists written with common bullet characters become real Slack lists

The assistant's list should render as a Slack list regardless of which common bullet character it typed.

- **Acceptance Criteria:**
  - [ ] Given the assistant writes a message where several consecutive lines each begin with `•` followed by a space, when the user opens the resulting draft in Slack, then those lines appear as a native Slack bulleted list — indented, with Slack's own bullet — and not as plain lines with a `•` typed into them.
  - [ ] The same holds for the other bullet characters the assistant commonly uses: `◦`, `‣`, `⁃`, and `·`.
  - [ ] Given a numbered list written as `1)`, `2)`, `3)`, when the user opens the draft, then it appears as a native Slack numbered list — the same result as writing `1.`, `2.`, `3.`.
  - [ ] Given a list is introduced by a line of ordinary text immediately above it, when the user opens the draft, then the list starts directly beneath that line with no empty line inserted between them.
  - [ ] Given the assistant writes a list using the existing dash (`-`), asterisk (`*`), or `1.` forms, when the user opens the draft, then it still appears as a native Slack list exactly as it does today.
  - [ ] Given a line begins with a bullet character but is not part of a list — for example a single line of prose that happens to start with `·` — when the user opens the draft, then the text is preserved and readable; no content is lost.
  - [ ] Given a list item's text itself contains bold, italic, inline code, or a link, when the user opens the draft, then that styling survives inside the list item.

### 2.2 People, channel, and user-group references become real mentions

A reference the assistant writes should become a working Slack mention that displays a name.

- **Acceptance Criteria:**
  - [ ] Given the assistant writes a person reference in a message, when the user opens the draft in Slack, then it appears as a real mention showing that person's display name — highlighted and clickable — not as a code in angle brackets.
  - [ ] Given the assistant writes a channel reference, when the user opens the draft, then it appears as a real channel link showing the channel's name.
  - [ ] Given the assistant writes a user-group reference, when the user opens the draft, then it appears as a real group mention showing the group's name.
  - [ ] Given a message contains several references mixed into ordinary sentence text, when the user opens the draft, then every reference resolves and the surrounding words, punctuation, and spacing are unchanged.
  - [ ] Given a mention appears inside a list item, a quote, or a heading, when the user opens the draft, then it resolves there too.
  - [ ] Given the assistant posts a message directly rather than drafting it, then the same resolution applies to the posted message.

### 2.3 Workspace-wide alerts are never armed on the user's behalf

`@here`, `@channel`, and `@everyone` notify large numbers of people at once. The assistant must not be able to turn one into a live alert.

- **Acceptance Criteria:**
  - [ ] Given the assistant writes `@here`, `@channel`, or `@everyone` in any form, when the user opens the draft, then it appears as ordinary text — visible and readable, but not a live alert that would notify the channel if sent as-is.
  - [ ] Given such a reference is written, when the user opens the draft, then its text is intact and legible — it is never mangled, split across lines, or partly swallowed.

### 2.4 A reference that cannot be verified stops the write

The assistant sometimes uses a code that does not name a real person or channel — for example pasting a conversation's code where a person's belongs. Writing that into a draft produces a message the user cannot send without editing, and the user may not notice until after sending.

- **Acceptance Criteria:**
  - [ ] Given the assistant writes a person reference whose code matches no real person in the workspace, when it tries to create or replace the draft, then nothing is written and the assistant is told which reference is unrecognised, so it can correct itself and retry.
  - [ ] The same applies to an unrecognised channel reference and an unrecognised user-group reference.
  - [ ] Given the workspace cannot be consulted at that moment — for example the check for a user group cannot be completed — when the assistant tries to write, then nothing is written and the assistant is told the reference could not be verified, distinguishing "could not check" from "does not exist".
  - [ ] Given a write is refused for an unverifiable reference, when the user later looks at Slack, then any draft that already existed at that destination is untouched — the refusal never damages existing content.
  - [ ] Given the assistant is posting a message directly rather than drafting, then an unverifiable reference stops that post the same way.
  - [ ] Given a message contains no references at all, then no verification happens and nothing about today's behaviour changes.

### 2.5 The assistant can see who it tagged

When the assistant reads a draft back — its own, or one already sitting at the destination — the mentions must be legible to it.

- **Acceptance Criteria:**
  - [ ] Given a draft contains a person mention, when the assistant reads the draft's readable text back, then that mention appears as the person's name prefixed with `@`, not as a code.
  - [ ] The same holds for channel mentions and user-group mentions.
  - [ ] Given a draft was written by the user by hand and contains mentions, when the assistant reads it back before deciding whether to overwrite it, then those mentions are shown by name so the assistant can describe the draft accurately to the user.
  - [ ] Given a mention's name cannot be determined, when the assistant reads the draft back, then the reference is still shown in a form that makes clear something is there — the readback never silently omits it.
  - [ ] The readback stays a display aid only: the exact original content of an existing draft is still preserved in full and can be restored unchanged, exactly as today.

---

## 3. Scope and Boundaries

### In-Scope

- Recognising the common bullet characters (`•`, `◦`, `‣`, `⁃`, `·`) and the `1)` numbered form as real list markers.
- Removing the stray empty line between a line of text and the list that follows it.
- Turning person, channel, and user-group references into real Slack mentions in both drafted and directly posted messages.
- Keeping `@here` / `@channel` / `@everyone` as plain text, intact and readable.
- Refusing to write when a reference cannot be verified, with an explanation naming the offending reference.
- Showing mentions by name when a draft is read back.

### Out-of-Scope

- **Turning typed names into mentions.** Writing `@dana` as plain typed text will not become a mention — guessing which person was meant risks tagging the wrong colleague. Only the bracketed references Slack itself produces are resolved.
- **Making `@here` / `@channel` / `@everyone` live.** Explicitly excluded above, by decision.
- **Storing anything new on disk.** The existing workspace lists of people and channels are used as they are; user groups are checked live only when one is actually referenced.
- **Any other formatting gap** — tables, images, nested quotes, emoji shortcodes, strikethrough, and similar are untouched by this work.
- **Group-DM (MPIM) support**, **Guided Token Extraction & Setup**, and **Setup Diagnostics** — the remaining roadmap items, addressed in their own specifications.

---

## Change Log

_No amendments yet._
