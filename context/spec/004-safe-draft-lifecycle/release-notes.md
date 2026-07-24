# Release-notes text for the next `pv-v*` release

This repo has no committed changelog: release notes are auto-generated at tag time (`generate_release_notes: true` in `.github/workflows/release.yaml`) and the maintainer edits the release body on GitHub. `scripts/update.sh` scans the published release-notes body for lines beginning with `CONFIG-CHANGE:` and surfaces them to users on upgrade.

Paste the block below into the release body of the first `pv-v*` release that ships the safe-draft-lifecycle feature (spec 004). The `CONFIG-CHANGE:` lines must stay at the start of their lines, one note per line, for the updater's grep to find them.

---

## Safe draft lifecycle (`conversations_draft_message`)

The draft tool no longer silently overwrites an existing draft, and its response format changed.

CONFIG-CHANGE: conversations_draft_message no longer overwrites an existing draft by default — it returns the draft's content with action "existing_draft_found" and writes nothing. Callers wanting the old replace-in-place behavior must pass overwrite: true.
CONFIG-CHANGE: conversations_draft_message now returns a JSON object (action, draft_id, channel_id, thread_ts, draft{text, blocks_json, last_updated_client}, displaced, note) instead of the previous CSV row (draft_id,channel_id,thread_ts). Any automation parsing the CSV must switch to the JSON shape.

Also in this change: `content_type: application/json` accepts verbatim rich_text block JSON for lossless restore of displaced content; the optional `draft_id` parameter targets a specific draft (destination must still match); scheduled drafts are never replaced or deleted; every write is confirmed by re-listing so the reported action and draft id match what Slack actually holds.
