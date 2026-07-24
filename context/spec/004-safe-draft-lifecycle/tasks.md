# Tasks: 004-safe-draft-lifecycle

Derived from `functional-spec.md` and `technical-considerations.md` in this directory. Locked decisions (see `flow-log.md`): zero new MCP tools; clean break to a JSON result; provenance decided by agent-side content matching; fail-safe invariant — a missed destination match creates a duplicate, never overwrites; threading already works, so §2.4 work is regression coverage only; `drafts.delete` is not exposed.

Every slice leaves the server building and running. `make test` must be green at the end of each slice.

---

- [x] **Slice 1: The server can read a draft's body**

  > Groundwork with no behavior change: the edge client already receives the full draft body from `drafts.list`, it just discards it. Nothing else can be built until it is decoded.
  - [x] Add `Blocks json.RawMessage \`json:"blocks"\`` and `LastUpdatedClient string \`json:"last_updated_client"\`` to the `Draft` struct in `pkg/provider/edge/drafts.go`. Leave other returned fields (`is_from_composer`, `date_created`, `file_ids`, `team_id`, `user_id`) undecoded. **[Agent: go-mcp-backend]**
  - [x] Add an exported sentinel `edge.ErrDraftConflict` and map the `draft_has_conflict` API error from `DraftsUpdate` onto it so callers can match with `errors.Is`. Leave all other error handling as-is. **[Agent: go-mcp-backend]**
  - [x] Verify: unit tests over a recorded `drafts.list` payload asserting `Blocks` and `LastUpdatedClient` decode, and that a `draft_has_conflict` response from `DraftsUpdate` satisfies `errors.Is(err, edge.ErrDraftConflict)` while an unrelated error does not. Run `make test`. Delete any scratch fixtures not committed as testdata. **[Agent: go-mcp-backend]**

- [x] **Slice 2: Draft content can be normalised and rendered**

  > Pure logic, no wiring. This is the load-bearing piece for consent: if normalisation is wrong, the agent either over-asks (safe) or wrongly skips consent (not safe).
  - [x] Create `pkg/handler/draft_normalize.go` with `normalizeDraftBlocks(raw json.RawMessage) (json.RawMessage, error)` — recursively delete every `block_id` key, re-marshal via `encoding/json` for canonical key order, preserve everything else verbatim (including element types this server never emits: user/channel mentions, emoji, unknown future types). **[Agent: go-mcp-backend]**
  - [x] Add `renderDraftBlocksText(raw json.RawMessage) string` in the same file — best-effort display rendering of rich_text into plain text with structural hints (newlines between sections, list markers, `>` for quotes, fenced code). On decode failure, degrade to a placeholder directing the reader to `blocks_json`. Display-only; document that it is never a restore source. **[Agent: go-mcp-backend]**
  - [x] Verify: table-driven tests per technical-considerations §4 — for every formatting kind `markdownToRichTextBlock` emits (bold, italic, bold+italic, inline code, fenced code, ordered/unordered/nested lists, quotes, links, autolinks, headings), assert `normalize(sent)` equals `normalize(sent + injected block_id + shuffled key order)`; assert unknown element types survive byte-preserved apart from `block_id`/key order; assert idempotence `normalize(normalize(x)) == normalize(x)`. Run `make test`. **[Agent: go-mcp-backend]**

- [x] **Slice 3: A draft that already exists is never overwritten without an explicit assertion**

  > The headline safety change and the core of the consent protocol. After this slice the destructive behavior is gone.
  - [x] Introduce a narrow interface seam over the drafts operations the handler consumes (`DraftsList`, `DraftsCreate`, `DraftsUpdate`) so handler tests can record calls without network. Wire the real provider through it; no behavior change. **[Agent: go-mcp-backend]**
  - [x] Replace the CSV `draftResult` (`pkg/handler/conversations.go:304-309`) with the JSON `draftActionResult` per technical-considerations §2.2: `action`, `draft_id`, `channel_id`, `thread_ts`, `draft{text,blocks_json,last_updated_client}`, `displaced{...}`, `note`. Clean break — no CSV shim, no dual shape. **[Agent: go-mcp-backend]**
  - [x] Add the `overwrite` boolean parameter (default `false`) to the tool schema and `parseParamsToolDraftMessage`. **[Agent: go-mcp-backend]**
  - [x] Rework `ConversationsDraftMessageHandler`: on an existing draft at the destination with `overwrite=false`, write nothing and return a **successful** result with `action: existing_draft_found` carrying the draft's normalised `blocks_json`, readable `text`, and `last_updated_client` (documented in `note` as a weak hint, never the provenance decision). With `overwrite=true`, update and return `action: replaced` with `displaced` populated from the pre-update listing. With no draft found, create and return `action: created` — including when `overwrite=true`. **[Agent: go-mcp-backend]**
  - [x] Confirm every write by re-listing and reporting the id and as-stored content Slack actually holds, never an assumed id (technical-considerations §2.6). If the confirming re-list cannot find the draft, return an honest error naming the write that succeeded and the confirmation that failed — never a fabricated success. **[Agent: go-mcp-backend]**
  - [x] Verify: handler tests through the mock seam — existing draft + no `overwrite` produces `existing_draft_found` and records **zero** create/update calls; `overwrite=true` records an update carrying the freshly-listed concurrency token and returns `displaced`; empty destination produces `created`; a re-list miss produces an error not a success. Run `make test`. **[Agent: go-mcp-backend]**

- [x] **Slice 4: Displaced content can be restored exactly**

  > Closes the loop opened by Slice 3 — authorising an overwrite stops being a one-way door.
  - [x] Extend `content_type` with `application/json`: validate the body parses as a JSON array in which every element is `"type": "rich_text"` (reject anything else explicitly rather than let Slack drop it silently), run it through `normalizeDraftBlocks`, and **bypass `markdownToRichTextBlock` and `draftContentLoss` entirely** — the input is already-valid blocks, not markdown. **[Agent: go-mcp-backend]**
  - [x] Verify: tests asserting the markdown converter and loss check are never invoked on the JSON path (mock/spy), non-rich_text block types are rejected, and a `blocks_json` previously returned by the tool round-trips to a byte-identical stored draft. Run `make test`. **[Agent: go-mcp-backend]**

- [x] **Slice 5: A specific draft can be targeted, and scheduled drafts are untouchable**

  > Removes the last dependence on destination-guessing, and hard-guards the one case where an overwrite must never be honoured.
  - [x] Add the optional `draft_id` parameter. Resolve it within the listed drafts; not found → explicit error with nothing written. Found → its destination must match the requested `channel_id`/`thread_ts` (compared via `normalizeTS`), else an explicit error — `draft_id` is targeting, not retargeting, which also keeps the channel-policy check covering the draft actually touched. **[Agent: go-mcp-backend]**
  - [x] Add an explicit scheduled-draft guard on the resolved draft (`DateScheduled != 0` → refuse) that applies regardless of `overwrite` and regardless of how the draft was resolved, so a known `draft_id` cannot bypass the skip that `findDraftForDestination` performs on the destination path. **[Agent: go-mcp-backend]**
  - [x] Verify: tests for `draft_id` not found, `draft_id` with mismatched destination, `draft_id` pointing at a scheduled draft with `overwrite=true` (must refuse), and destination-path scheduled drafts staying skipped. Run `make test`. **[Agent: go-mcp-backend]**

- [x] **Slice 6: A draft edited underneath us fails safe**

  > The real race: the user edits in Slack between our list and our update. Live-confirmed that Slack rejects the write and the draft survives.
  - [x] Handle `edge.ErrDraftConflict` in the handler: re-list and return a **successful** result with `action: conflict` carrying the draft's current content, meaning "the draft changed underneath you; the consent protocol restarts from this content". Never retry the write automatically. **[Agent: go-mcp-backend]**
  - [x] Verify: mock returns `ErrDraftConflict` from update → result is a success with `action: conflict`, current content populated, and no retry recorded. Run `make test`. **[Agent: go-mcp-backend]**

- [x] **Slice 7: Permission and credential gating cover the new read path**

  > The refusal response now carries private draft text, so a gating regression would leak it. This makes the ordering a tested invariant rather than an incidental property.
  - [x] Confirm and lock the ordering: the policy check in `parseParamsToolDraftMessage` (`conversations.go:360`) must complete before any `DraftsList` (`:401`). Add the explicit non-session-token error for defense in depth — "drafts require a browser-session token (xoxc/xoxd); the configured token type cannot access Slack's drafts API" — alongside the existing `!provider.IsOAuth()` registration gate at `server.go:238`. **[Agent: go-mcp-backend]**
  - [x] Verify: with the tool disabled and with the channel denied, assert the policy error is returned and the mock proves `DraftsList` was **never** called — no draft text read or revealed. Assert the credential error text for a non-session token. Run `make test`. **[Agent: go-mcp-backend]**

- [x] **Slice 8: The agent can learn the protocol from the schema alone**

  > The tool description is the only channel through which a calling LLM learns compare-then-consent. The current description is now actively false.
  - [x] Rewrite the tool description in `pkg/server/server.go:239` — it currently claims "If a draft already exists for the same channel/thread it is replaced in place", which becomes untrue. Cover, in order: non-destructive default; the compare-then-consent protocol (byte-identical `blocks_json` → own draft → `overwrite: true` without asking; different or never-written → show `text`, get consent); `blocks_json` authoritative and `text` display-only, never a restore source; `replaced` returns displaced content for restore; scheduled drafts never touched; session token required and drafts are never sent. Add parameter descriptions for `overwrite` and `draft_id` restating their halves of the contract. **[Agent: go-mcp-backend]**
  - [x] Update any repo documentation covering this tool's parameters or output (README and any docs/ tool reference), including the CSV→JSON result change. **[Agent: go-mcp-backend]**
  - [x] Prepare the release-notes entry flagging the behavior and response-format change for the next `pv-v*` fork release, using the repo's `CONFIG-CHANGE:` convention so the updater surfaces it. **[Agent: release-infra]**
  - [x] Verify: build the binary (`make build`), drive it over a stdio MCP handshake, and confirm the registered tool schema exposes `overwrite`, `draft_id` and the `application/json` content type with the new description text. Delete any transcript or scratch files produced during the check. **[Agent: go-mcp-backend]**

- [x] **Slice 9: One maintained live probe replaces five investigation scripts**

  > The investigation left five ad-hoc files sharing helpers across file boundaries and carrying hardcoded draft ids from a single session.
  - [x] Consolidate `drafts_live_probe_test.go`, `drafts_cause_probe_test.go`, `drafts_cleanup_probe_test.go`, `drafts_content_probe_test.go` and `drafts_afteredit_probe_test.go` in `pkg/provider/edge/` into one documented `drafts_probe_test.go` behind the `liveprobe` build tag. Keep one probe per still-relevant ground truth: upsert round-trip stability, threaded-destination round-trip, block-normalisation ground truth, conflict-on-stale-token. Remove the hardcoded investigation draft ids (`Dr0BKDD3L6CR`, `Dr0BKBBMJE8J`, `Dr0BL5M9E0KA`); probes create and clean up their own drafts. Keep shared helpers in the same file. Document at the top what each probe establishes and the safety rules — session token required, DM-to-self only, never a shared channel. **[Agent: go-mcp-backend]**
  - [x] Verify: `make test` is green and does **not** compile or run the probes; `go vet -tags liveprobe ./pkg/provider/edge/` passes; confirm no hardcoded draft ids remain. Do not run the probes against live Slack in this task. **[Agent: go-mcp-backend]**

- [x] **Slice 10: Feature Testing & Regression**

  > Verifies the whole feature end-to-end against functional-spec.md, run after all implementation slices are complete.
  - [x] Read functional-spec.md acceptance criteria in full. Generate acceptance-level tests that verify the entire feature as a whole — not individual slices. Cover applicable layers (unit for pure logic, integration for service interactions, e2e for user flows) based on the project's testing stack. Write tests with RED validation (must fail before implementation is confirmed done). Annotate each test with `@spec: 004-safe-draft-lifecycle` and `@regression` if suitable for long-term regression. **[Agent: testing-expert]**
  - [x] Run all generated tests. All must pass. Fix any failures before proceeding. **[Agent: testing-expert]**
