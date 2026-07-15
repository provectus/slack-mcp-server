# Flow Log: 003-service-binary-flip

## 2026-07-15 — specs (retrofit)

- Feature: **Service Binary Flip (Development ↔ Release)** — flip the background service between the release binary and a locally built debug binary via a file-based pin.
- Retrofit run: implementation already shipped on branch `feat/service-binary-flip` (commit 517ab30, PR https://github.com/provectus/slack-mcp-server/pull/18) via a brainstorm-first session; this run backfills the AWOS artifacts into the pre-existing spec directory.
- Produced: `functional-spec.md` (approved via gate). `design.md` predates this run (brainstorming output) and remains the design source.
- Decision: reused existing directory `003-service-binary-flip`; skipped `create-spec-directory.sh` (it always allocates a new index).
- Next: `/awos:tech` → `technical-considerations.md`.

## 2026-07-15 — tech + tasks + flow fix (retrofit)

- Produced: `technical-considerations.md` (approved via gate), `tasks.md` (all slices `[x]` — retrofit of shipped work; known gap: no automated harness for `run-with-tokens.sh`).
- Updated `context/product/architecture.md` (stale binary-resolution line → pin symlink order).
- Flow fix (user-approved): `/awos:tasks`' internal review question is skipped under `/implement-feature` — recorded in the command's Step 4 and delivery-flow.md §10 Local Customizations. Observation: flow says "no gate" after tasks, inner command demanded one.
- Next: commit specs → verify → local review → push; then check CodeRabbit comments on PR #18 (user request).
