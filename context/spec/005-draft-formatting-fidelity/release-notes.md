# Release-notes text for the next `pv-v*` release

This repo has no committed changelog: release notes are auto-generated at tag time (`generate_release_notes: true` in `.github/workflows/release.yaml`) and the maintainer edits the release body on GitHub. `scripts/update.sh` scans the published release-notes body for lines beginning with `CONFIG-CHANGE:` and surfaces them to users on upgrade.

Paste the block below into the release body of the first `pv-v*` release that ships the draft-formatting-fidelity feature (spec 005). The `CONFIG-CHANGE:` lines must stay at the start of their lines, one note per line, for the updater's grep to find them.

---

## Draft & message formatting fidelity (`conversations_draft_message`, `conversations_add_message`)

Drafted and posted messages now render lists and mentions the way a person typing in Slack would get them. Both tools share the markdown → `rich_text` converter, so both inherit the change.

CONFIG-CHANGE: conversations_add_message now converts `<@U…>`, `<#C…>` and `<!subteam^S…>` into live Slack mentions. A workflow that was posting `<@U123>` as inert-looking text will now genuinely notify that person, link that channel, and ping that user group. Review any automation that passes reference codes through to message text before upgrading.
CONFIG-CHANGE: conversations_add_message now refuses to post a message containing a reference it cannot resolve — it previously posted the raw code. This is breaking for any caller passing unverified IDs: the tool returns an error naming the offending reference, and nothing is posted.
CONFIG-CHANGE: bot (`xoxb`) and OAuth (`xoxp`) tokens need the `usergroups:read` scope to send a message referencing a user group via conversations_add_message. Without it the user-group check cannot complete and the write is refused as "could not be verified", where it previously posted the raw `<!subteam^S…>` code. Add the scope and reinstall the app, or drop the reference.

### Lists

Lines beginning with a bullet character — `•`, `◦`, `‣`, `⁃`, `·` followed by a space — now become native Slack lists. Previously they were plain text lines with the character typed in, with a stray empty line inserted between the introducing sentence and the list; both faults are gone. Bold, italic, inline code, and links survive inside list items. The markdown forms that already worked — `-`, `*`, `+`, `1.`, `1)` — are unchanged. A bullet character mid-sentence, or one with no space after it, is left alone.

### Mentions

`<@U…>` / `<@W…>`, `<#C…>` and `<!subteam^S…>` become real mentions, which Slack renders with the current display name. This applies inside list items, quotes, and headings, and to directly posted messages as well as drafts. A `|label` suffix is dropped in favour of the live name.

`@here`, `@channel` and `@everyone` are deliberately written as inert text, never as live broadcasts — they stay visible and readable in the draft but cannot arm a workspace-wide alert on the user's behalf. Typed plain-text names such as `@dana` are still not turned into mentions; only the bracketed references Slack itself produces are resolved.

### Unverifiable references stop the write

A reference that names nothing real, or that cannot be checked against the workspace, refuses the whole write. Nothing is created or updated, and an existing draft at the destination is neither read nor modified. The two cases produce textually distinct errors — "does not exist in this workspace" is a self-correctable mistake, "could not be verified" is a transient or permissions problem worth retrying.

### Readback

When the assistant reads a draft back, mentions are shown by name (`@dana`, `#general`, `@eng`) instead of raw codes, including for drafts a human wrote by hand. A reference whose name cannot be determined is still shown in its raw form, never silently omitted. The readback remains display-only: names are resolved live and are not stable across calls, and the exact original content of an existing draft is still preserved in full for lossless restore.

### Unchanged paths

`content_type: text/plain`, `content_type: application/json`, and `conversations_add_message`'s raw `blocks` parameter get no conversion and no validation — literal means literal. This keeps the lossless draft-restore contract intact, so a draft mentioning a since-deactivated colleague stays restorable byte-for-byte.
