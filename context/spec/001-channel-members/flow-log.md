# Flow Log: List Channel Members

Feature title: **List Channel Members** (List channel members and persist them in cache)

---

## specs — functional-spec

- Produced: `context/spec/001-channel-members/functional-spec.md` (Status: Draft → approved at gate).
- Decisions: member rows = user ID + display name + real name (resolved via users cache); channel input = ID or #name; return full roster (no pagination); optional filter to exclude bots and/or deactivated; per-workspace persisted cache with TTL + background refresh + explicit refresh.
- Branch: `feat/channel-members-cache` (from `fork/master`).
- Next: `/awos:tech` (technical-considerations.md).

## specs — technical-considerations

- Produced: `context/spec/001-channel-members/technical-considerations.md` (drafted by `go-mcp-backend`, claims spot-checked against api.go/channels.go/server.go/slack-go).
- Key decision: third cache tier — per-channel roster map in one workspace-scoped JSON file with per-entry TTL; `SlackAPI` interface gains `GetUsersInConversationContext` (edge+standard already implement it). No new tech → no hire.
- Approved at gate.
- Next: `/awos:tasks`.

## specs — tasks

- Produced: `context/spec/001-channel-members/tasks.md` (`<!-- not-user-reviewed -->`, no skip-tests). 3 slices: (1) provider fetch+cache, (2) `channels_members` tool, (3) Feature Testing & Regression.
- Agents: `go-mcp-backend` (impl+verify), `testing-expert` (QA). No general-purpose, no missing MCPs.
- Per parent flow, /awos:tasks has no gate.
- Next: commit specs, then `/awos:implement`.

