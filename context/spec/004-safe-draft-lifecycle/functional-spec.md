# Functional Specification: Safe Slack Draft Lifecycle

- **Roadmap Item:** Not on the roadmap — a defect-driven fix to the existing draft-message capability delivered in Phase 2 ("Acting on Slack & Self-Hosting").
- **Status:** Draft
- **Author:** Aleksandr Makarov

---

## 1. Overview and Rationale (The "Why")

### The problem

The assistant can put a draft message into the user's Slack for them to review and send. Today that capability can destroy the user's own writing, and it reports success in cases where it did something different from what it claims.

Three distinct problems have been observed in real use:

**It overwrites drafts the user wrote by hand.** When the assistant is asked to draft a message somewhere the user already has an unsent draft, it replaces that draft. It has no way to tell its own drafts apart from ones the user typed themselves, and Slack keeps no history of a draft once it is replaced. The user has no way to see what is about to be destroyed and no way to get it back. In one session the user asked, after the fact, whether a draft they had written themselves could be recovered; it could not.

**It does not reliably know whether it created or replaced a draft — and reports the wrong one.** Asked to revise a draft it just made, the assistant sometimes updates the existing draft and sometimes silently leaves the old one behind and makes a second. Either way it tells the user "updated in place." The user is then left with several near-identical unsent drafts in Slack, discovers them later, and has to work out by hand which one is current.

**A draft meant as a reply in a conversation thread may not end up attached to that thread.** In one session the user asked for a reply under a specific message, was told it was done, and found the draft was not where they expected. Whether the draft genuinely landed in the wrong place, or whether the user was looking at one of the duplicate drafts left over from the previous problem, is unresolved and must be established against real Slack rather than assumed.

### The desired outcome

The user can let the assistant work on their Slack drafts without ever risking their own unsent writing.

Concretely, after this change:

- Nothing the user wrote themselves is destroyed without them seeing it first and agreeing.
- Anything that is displaced can be put back exactly as it was.
- What the assistant reports having done always matches what actually happened.
- Asking for repeated revisions of one message leaves exactly one draft, not a pile.
- A draft meant for a thread is in that thread.

### Deliberate restraint

This is a fix to one existing capability, not a new feature area. It adds no new abilities for the assistant to invoke — everything below is a change to how the existing draft-message capability behaves. Reading a draft happens only where it is needed to protect the user, and what is read comes back as part of that protection rather than as a capability of its own. This keeps the assistant's permanent overhead unchanged for every user in every session, since the cost of an ability is paid continuously whether or not it is used.

### How we measure success

- No further reports of lost hand-written drafts.
- Repeated revision of a single message leaves exactly one draft in Slack, every time.
- What the assistant claims it did matches what is in Slack, every time.

---

## 2. Functional Requirements (The "What")

### 2.1 The user's own drafts are never silently destroyed

- **As a** user, **I want** the assistant to refuse to overwrite a draft I wrote myself, **so that** my unsent writing is never lost without my knowledge.

  The assistant may freely replace a draft it created itself. For any other draft — including every draft that already existed before this change, and any draft the assistant cannot positively confirm it wrote — it must stop, show the user the full existing text, and wait for explicit permission before replacing it.

  - **Acceptance Criteria:**
    - [ ] Given the user has an unsent draft they wrote by hand in a channel, when the user asks the assistant to draft a message in that same channel, then the existing draft is left completely unchanged, and the assistant reports that a draft is already there and shows its full text.
    - [ ] Given the assistant has just refused as above, when the user confirms they want it replaced, then the draft is replaced with the new text and the assistant confirms the replacement.
    - [ ] Given a draft that existed in Slack before this change was installed, when the assistant is asked to draft a message at that same destination, then it treats the draft as the user's own and refuses as above.
    - [ ] Given the assistant created a draft earlier in this same conversation, when the user asks for a revision of it, then it is replaced without any confirmation prompt.
    - [ ] Given a draft that is scheduled to send at a future time, when the assistant is asked to draft a message at that destination, then the scheduled draft is never replaced or removed under any circumstances, including when the user authorizes an overwrite.

### 2.2 The assistant reports truthfully what it did

- **As a** user, **I want** the assistant's report to state whether it created a new draft or replaced an existing one, **so that** I can trust what I am told and know how many drafts are sitting in my Slack.

  - **Acceptance Criteria:**
    - [ ] When the assistant creates a brand-new draft, then its report says a draft was created.
    - [ ] When the assistant replaces an existing draft, then its report says the draft was replaced.
    - [ ] Given the assistant reports a draft as created or replaced, when the user looks in Slack, then the draft the report identifies is the one actually there.
    - [ ] Given the assistant is asked to revise the same message several times in a row, when the user then looks at their Slack drafts, then exactly one draft for that message exists.
    - [ ] Given the assistant revises a message it drafted moments earlier, when it reports the result, then it does not claim to have replaced a draft while having in fact created an additional one.

### 2.3 Displaced text is handed back, and can be put back exactly as it was

- **As a** user, **I want** any draft text that gets displaced returned to me in a form that restores unchanged, **so that** authorizing an overwrite is never a one-way door.

  Whenever the assistant refuses to overwrite, and whenever it goes ahead with an authorized overwrite, the displaced text is returned to the user both readably and in a form that can be put straight back with identical formatting. Restoring must not re-interpret or reformat the text on the way back in, because converting formatted text back and forth loses detail.

  - **Acceptance Criteria:**
    - [ ] Given the assistant refuses to overwrite a draft, when it reports the refusal, then the existing draft's full text is included, both readably and in a form that can be restored unchanged.
    - [ ] Given the user authorized an overwrite, when the assistant reports the replacement, then the text that was displaced is included in the same two forms.
    - [ ] Given the assistant displaced a draft and reported its previous text, when the user asks for that draft to be restored, then the draft in Slack is returned to exactly its previous content, with all formatting — bold, italics, lists, quotes, code, links, and mentions of people — identical to before.
    - [ ] Given a draft containing formatting of every kind the assistant can produce, when it is read and then written straight back unchanged, then the draft in Slack is unchanged.
    - [ ] Given the user asks to restore a draft, when the restore completes, then exactly one draft exists at that destination and no leftover draft remains from the restore.

### 2.4 A draft meant for a thread lands in that thread

- **As a** user, **I want** a draft written as a reply to appear as a reply to that message, **so that** when I send it, it goes where I intended.

  Whether this works today is unknown and must be established against real Slack — created against a conversation with the user themselves, never a shared channel — before deciding whether a fix is needed.

  - **Acceptance Criteria:**
    - [ ] Given the user asks for a draft replying to a specific message, when the draft is created and the user opens that message's thread in Slack, then the draft is waiting there as a reply.
    - [ ] Given the user asks for a draft in a conversation without replying to anything, when the draft is created, then it is waiting in the conversation's main message box and is not attached to any thread.
    - [ ] Given a draft was created as a reply to a message, when the user asks for it to be revised, then the revision stays a reply to that same message and does not move to the conversation's main message box.
    - [ ] Given a draft exists as a reply in a thread, when the assistant is asked to draft a message to the conversation's main message box, then the threaded draft is not treated as the one to replace.

### 2.5 Draft behavior follows the existing permission settings

- **As a** self-hoster, **I want** all of this governed by the same switch and the same per-conversation permissions that already control draft writing, **so that** there is one setting to reason about and unsent private text is not exposed by default.

  - **Acceptance Criteria:**
    - [ ] Given draft functionality is switched off, when the assistant is asked to draft a message, then it reports that the capability is disabled and takes no action, and no draft text is read or revealed.
    - [ ] Given draft functionality is restricted to certain conversations, when the assistant is asked to draft a message in a conversation outside those, then it declines and takes no action, and no draft text from that conversation is read or revealed.
    - [ ] Given the user's Slack credentials are of a kind that cannot reach drafts at all, when a draft action is attempted, then the assistant explains that this credential type cannot access drafts, rather than failing obscurely.

---

## 3. Scope and Boundaries

### In-Scope

- Refusing to replace any draft the assistant did not demonstrably create, showing the existing text instead, with an explicit way for the user to authorize the replacement.
- Reporting truthfully whether a draft was created or replaced, and identifying the draft that actually resulted.
- Returning displaced text in a form that restores exactly, and accepting that form back for restoration.
- Targeting a specific known draft directly, rather than guessing from the destination.
- Establishing against real Slack whether a threaded draft actually lands in its thread, and fixing it if it does not.
- Leaving scheduled drafts entirely alone.
- Keeping all of the above under the existing draft permission switch and per-conversation rules.

### Out-of-Scope

- **Any new ability for the assistant to invoke.** This is a change to how the existing draft-message capability behaves. Explicitly excluded: a standalone way to list or read drafts, and a way to delete a draft. Both were considered and cut — each would add permanent overhead to every session for every user, and neither is needed to solve the problem. Leftover drafts stop being created once the create-versus-replace defect is fixed, and any that remain are removed by the user in Slack directly.
- **Sending a draft.** Drafts remain for the user to review and send themselves.
- **Scheduled drafts.** Left alone entirely — never replaced, never deleted.
- **Draft history or versioning.** Only the immediately displaced content is recoverable, from the report of the action that displaced it. There is no accumulating history and no stored archive.
- **Persisting anything to disk.** Displaced text is returned in the assistant's report only; nothing is written to a file for later. If the conversation is gone, so is the displaced text — prevention, not archival, is the protection.
- **File attachments on drafts.** Adding, reading, or preserving attachments is not covered.
- **Drafts addressed to more than one conversation at once.**
- **Editing drafts in any Slack client directly** — this concerns only what the assistant does.
- Other roadmap items, automatically excluded: **Group-DM (MPIM) Support**, **Guided Token Extraction & Setup**, **Setup Diagnostics**.

---

## Change Log

_No amendments yet._
