# Functional Specification: List Channel Members

- **Roadmap Item:** Expanded Slack Surface — give the assistant a way to see who belongs to a channel, kept fast through caching (extends the "Broader Coverage" phase).
- **Status:** Draft
- **Author:** AWOS implement-feature flow

---

## 1. Overview and Rationale (The "Why")

Today the assistant can list channels and search users, but it cannot answer a basic, frequently asked question: **"Who is in this channel?"** To reason about a conversation — who to mention, who has seen a message, who owns a topic — the assistant needs the roster of a channel's members.

Fetching that roster from Slack on every request is slow and burns rate limit, especially for large channels. So, like the existing users and channels lists, the member roster is kept in a fast local store and refreshed in the background, so repeat questions about the same channel answer instantly.

**Desired outcome:** the assistant can ask for the members of any channel it can see, get back each member's ID and name quickly, and optionally narrow the list to just real people. Repeated requests for the same channel are fast and do not re-hit Slack until the stored roster ages out or a refresh is explicitly requested.

**Success measures:** a member request for an already-seen channel returns without a new Slack round-trip; a first-time request returns the complete roster; the names shown match what the user sees in Slack.

---

## 2. Functional Requirements (The "What")

- **As** the assistant, **I want to** list the members of a specific channel, **so that** I can tell the user who belongs to it and reason about the conversation.
  - **Acceptance Criteria:**
    - [ ] Given a channel the assistant can see, when the assistant requests that channel's members, then it receives a list where each entry shows the member's user ID, display name, and real name.
    - [ ] When the requested channel has many members, then the response contains the complete roster (every member is returned, not a partial page).
    - [ ] Given an empty or members-hidden channel, when the assistant requests its members, then it receives an empty list rather than an error.

- **The user identifies the channel by either its ID or its name.**
  - **Acceptance Criteria:**
    - [ ] When the assistant supplies a channel ID (e.g. `C0123ABCD`), then the members of that channel are returned.
    - [ ] When the assistant supplies a channel name (e.g. `#general` or `general`), then the same members are returned as if the ID had been supplied.
    - [ ] Given a channel reference that matches no known channel, when the assistant requests its members, then it receives a clear message saying the channel was not found, and no partial or misleading list.

- **The member names come from the same source as the rest of the product, so they are consistent.**
  - **Acceptance Criteria:**
    - [ ] When a member is shown, then their display name and real name match what the assistant already reports elsewhere for that same user.
    - [ ] Given a member whose profile details are not yet known locally, when the roster is returned, then that member still appears (by ID), with whatever name information is available, rather than being dropped.

- **The assistant can narrow the list to exclude bots and deactivated accounts.**
  - **Acceptance Criteria:**
    - [ ] By default, when the assistant requests a channel's members, then everyone the channel reports is included — real people, bots, and deactivated accounts.
    - [ ] When the assistant asks to exclude bots, then bot users are omitted and all other members remain.
    - [ ] When the assistant asks to exclude deactivated accounts, then deactivated users are omitted and all other members remain.
    - [ ] When the assistant asks to exclude both, then only active human members remain.

- **The roster is remembered so repeat requests are fast.**
  - **Acceptance Criteria:**
    - [ ] Given a channel whose members were requested recently, when the assistant requests them again, then the answer is served from the remembered roster without a fresh Slack lookup.
    - [ ] The remembered roster survives a restart of the server (a repeat request after restart is still fast and does not require re-fetching from Slack, until it ages out).
    - [ ] When the remembered roster has aged past its freshness window, then the next request transparently refreshes it and returns the up-to-date roster.
    - [ ] When the assistant explicitly asks to refresh a channel's members, then the roster is re-fetched from Slack and the returned list reflects the current membership.
    - [ ] Remembered rosters are kept separately per workspace, so members from one workspace are never shown for another.

- **While the roster is first being gathered, the assistant is told to wait rather than shown a wrong answer.**
  - **Acceptance Criteria:**
    - [ ] Given a channel whose roster has never been gathered and a background gather is in progress, when the assistant requests its members, then it receives a short "still preparing, please retry shortly" message rather than an empty or partial list.

---

## 3. Scope and Boundaries

### In-Scope

- Listing the members of a single named/identified channel.
- Showing each member's user ID, display name, and real name.
- An optional filter to exclude bot users and/or deactivated accounts.
- Remembering the roster locally, per workspace, with background refresh and an explicit refresh option — matching how users and channels are already remembered.
- Returning the complete roster in one response.

### Out-of-Scope

- Adding or removing channel members (membership management is not part of this change).
- Group-DM (MPIM) member handling as a distinct feature — covered by the separate "Group-DM (MPIM) Support" roadmap item.
- User-group membership — already covered by the existing user-group tools.
- Pagination of the member list (the complete roster is returned in one response).
- Full member profiles beyond ID and name (title, email, timezone, etc.).
- Guided onboarding and setup diagnostics — separate roadmap items.

---

## Change Log

_Dated amendments made after the spec was first written._

- [none yet]
