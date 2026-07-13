---
description: Fixes one bug end-to-end — diagnoses the root cause, applies a scoped fix with a regression test, re-verifies the touched criteria, amends the spec on divergence, and opens a PR against the provectus fork.
argument-hint: '[bug — description, or GitHub issue URL/number]'
---

# Fix a Bug End-to-End

Takes one bug — from a free-text description or a GitHub issue — and drives it through diagnosis, a scoped fix with a regression test, re-verification of the touched acceptance criteria, and a GitHub PR until it is ready to merge. On the way it keeps the owning spec honest: when the fix changes documented behavior, it amends that spec rather than letting it drift. This project has no ticket tracker. Re-run `/awos:flow` to change any delivery decision recorded in `context/product/delivery-flow.md`.

## Arguments

`$ARGUMENTS` — a free-text bug description, or a GitHub issue URL or number. If empty, ask the user.

## Context Discipline

A flow degrades in one long context window. Per §8 of `context/product/delivery-flow.md`:

- Run every isolatable stage in a subagent (a subagent can invoke `/awos:*` commands via the Skill tool; its context is discarded on completion). Subagent reports must be terse — paths, verdicts, counts — never full diff, log, or review content.
- After each completed stage, append an entry to the flow log, `context/fix-log-{BUG_ID}.md` — `BUG_ID` is set in the fetch stage (a short slug for a description, or the issue number for a GitHub issue), so the path is known from the first stage on and never moves; when classify resolves an owning spec the log records the spec dir, but stays keyed to the bug. Each entry: the stage name, what was produced and where (paths, branch, commit), the classification verdict once known, any decisions taken, and which stage comes next. The log is the flow's memory outside the context window — a fresh session resumes by reading this one small file. It is committed with the work (commit-push stages it), so it must never become an uncommittable leftover: **once the PR is opened, stop writing to the tracked log.** From that point, report late-stage progress (gate results, close-out evidence) to the user, and resume the remote stages from remote state — the open PR and its check status. The close stage leaves a clean working tree and never writes a final entry it cannot commit.
- Never launch a nested headless session (`claude -p`) from this command. This project uses a manual trigger (§6).
- Tell every dispatched subagent: tools are functional — do not test them or make exploratory calls; every call needs a purpose. Run each delegated stage on the model tier recorded in §8 — the fast tier for mechanical transport work, the strongest for judgment (diagnosis, classification, fix, review).
- A subagent's report is a claim, not a fact. Before acting on a report that names files and lines, asserts a root cause, or reports a test outcome, spot-check it — read the named lines, run the named test. The diagnose and regression-test stages spell out their specific checks; the principle applies to every delegated report.
- Every fixed-choice interaction with the user — the divergence confirmation, keep/drop on review findings — goes through `AskUserQuestion` with the recorded default marked, never a prose question. Plain prose is only for inherently free-form input (a bug description).
- An unanswered `AskUserQuestion` (the harness returns `No response after 60s`) is handled by run mode. Read the `AWOS_UNATTENDED` environment variable: when set, take the safe default and continue; when unset the run is interactive, so re-ask once, then proceed naming the default you took. A timeout never authorizes an irreversible step: the divergence spec-amendment confirmation treats an unanswered prompt as a no.

This command is an orchestrator. It diagnoses and decides, but the code change goes through a delegated specialist — **do not edit code in the main context**.

## Self-Improvement Loop

This command is maintained through its own runs. When a run exposes a defect in the flow itself — a recorded fact disproven by reality (a "no X" claim, a dead link, a wrong state name), a missing step (an undocumented bootstrap, a transition chain), or a stage instruction that had to be worked around — fix the flow **in the same run**:

1. Patch the affected file(s) right in this working copy: this command file, `context/product/delivery-flow.md` (correct the fact where it is recorded), or the reused skill.
2. Stage those edits in the commit-push stage alongside the code change — same branch, same PR. Never park flow fixes for a separate PR; a flow that shipped its work while still carrying a known-wrong instruction has not finished the job. A defect found after the PR is open waits: report it as pending in the close-out, and the next run applies it at the workspace stage.
3. Record the correction in the flow log — with the observation that disproved the old text — and promote it into the decision record's **Local Customizations** section, so a future `/awos:flow` regeneration preserves it instead of resurrecting the defect.
4. Two kinds of defect are not yours to fix. A delivery _decision_ (a gate, the merge policy, the autonomy level) belongs to whoever owns the team's process — report the friction and leave the change to a `/awos:flow` re-run. A defect in how `/awos:flow` generated this command cannot be fixed here — tell the user so they can report it to the AWOS repo.

<!-- awos:flow:stage=fetch-bug -->

### Step 1: Fetch & Normalize the Bug

Resolve the bug from `$ARGUMENTS`:

- **A GitHub issue URL or number** → fetch it via `gh issue view <url-or-number> --repo provectus/slack-mcp-server --comments` (the `github` MCP is the fallback if `gh` fails). Use the issue number as `BUG_ID`. Also read any linked issues/PRs it references.
- **A free-text description** → use it directly; derive a short kebab slug as `BUG_ID`.

Extract and keep: the reported symptom, reproduction steps if given, and the affected area. Store `BUG_ID` for the flow log and branch name. Run this stage on the fast tier. (There is no crash-reporting tool in this project, so no stack symbolication.)

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=resume-detection -->

### Step 2: Detect the Entry Point

Start with a cheap preflight on the fast model tier (per §8): is this bug **already fixed**? If it came from a GitHub issue, check whether the issue is closed or a merged PR references it on `provectus/slack-mcp-server`; report and stop rather than re-fixing. Then, if this bug's flow log exists (`context/fix-log-{BUG_ID}.md`), read it first — it names the last completed stage and carries the branch, commit, classification verdict, and PR state, and is the resume signal for the middle stages that produce no scannable artifact.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=workspace -->

### Step 3: Prepare the Workspace

Per §2–§3: confirm `context/` is reachable (in-repo directory); warn on a dirty working tree (uncommitted AWOS artifacts left by `/awos:flow` are an expected cause, not a blocker); `git fetch fork master`; create the branch from `fork/master` using the `fix/<kebab-slug>` convention (e.g. `fix/channels-refresh-counts-only`). Store it as `BRANCH`. No submodules; main checkout (no worktrees).

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=diagnose -->

### Step 4: Diagnose

Reproduce the bug and find the root cause. Delegate the investigation to the built-in `Explore` subagent or the `go-mcp-backend` specialist via `[Agent: go-mcp-backend]` (strongest tier — judgment work) — the orchestrator does not read the whole codebase or write code itself. The subagent returns terse: the reproduction, the root-cause location (file/function), and a proposed minimal fix shape. If the bug cannot be reproduced, report that and stop rather than guessing.

A symptom rarely has exactly one renderer. The diagnosis is not done at the first root cause: enumerate **every surface that produces or consumes the symptom data** — grep for sibling code paths (other builders of the same tool response, other token-mode branches, the edge-client vs. standard-client fallback, cache readers vs. writers) — and return a verdict per surface, affected or clean. One data gap routinely hides behind several surfaces.

The diagnosis report labels every claim **verified** (the subagent read the named `file:line`, or executed the reproduction) or **hypothesis** — and the orchestrator re-reads the named lines before accepting the fix shape, trimming it to what the code actually shows. A subagent report is a claim, not a fact.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=classify -->

### Step 5: Classify — Conformance vs. Divergence

This gate decides whether the spec gets amended later, so it runs **before** any fix touches behavior. Locate the owning `context/spec/NNN-*/` for the affected behavior (read its `functional-spec.md`), then classify:

- **Conformance bug** — the code violates a _correct_ spec. → Fix the code and add a regression test; **do not** amend the spec.
- **Divergence** — the spec was wrong or incomplete, or the fix intentionally changes documented behavior. → Fix, add a regression test, and **amend** the owning spec in the `amend-spec` stage.

If the bug maps to **no** existing spec (legacy or cross-cutting behavior), do not fabricate one — record "no owning spec" and proceed without amendment. Record the verdict and the owning spec dir (or "none") in the flow log; later stages read it.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=fix -->

### Step 6: Fix

Delegate the code change to a specialist via `[Agent: go-mcp-backend]` (or `release-infra` for build/packaging bugs, `testing-expert` for test-harness bugs) — the orchestrator never edits code itself. Keep the change scope-disciplined: a flat task list targeting the root cause, no opportunistic refactors beyond what the fix needs. Pass the subagent the root-cause findings from Step 4 and the classification, not a re-derivation.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=regression-test -->

### Step 7: Regression Test

Add one Go unit test (`testify`, run by `make test`) that fails on the old code and passes on the fix, capturing the bug so it cannot silently return. Delegate it via `[Agent: testing-expert]`. Honor the `<!-- skip-tests: true -->` marker: if the owning spec's `tasks.md` carries it, skip the automated test and note that the regression is covered by the look-and-feel check in the next stage instead.

"Fails on the old code" is demonstrated, not asserted. The orchestrator verifies the test targets a **changed** site: revert the fix hunk (e.g. `git stash` the fixed files), run `make test` and watch the new test fail, restore the fix, watch it pass — then record the fail→pass evidence in the flow log. A green-but-vacuous test that asserts an already-correct path captures nothing — reject it and have the specialist retarget the changed lines.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=verify-criteria -->

### Step 8: Verify the Touched Criteria

Re-check **only** the acceptance criteria the bug touched, with `/awos:verify`'s evidence discipline. This is scoped: it does not re-run the whole acceptance set, does not flip the spec's Status, and honors `<!-- skip-tests: true -->`. Report the criteria checked and their evidence.

Running the app to verify is the flow's job, not the user's. Nothing shared blocks a run (§2): for a criterion needing a live render, build the binary (`make build`) and drive it directly (stdio MCP handshake, or the SSE endpoint on a free port). Reserve the manual `AskUserQuestion` fallback for a criterion the agent genuinely cannot render.

Scale the evidence to what changed. When the fix touched only the data or payload and the diff contains no output-path edits, the sanctioned evidence is the demonstrated failing→passing regression test plus a unit-level check of the changed data — standing up a full server run to watch an unchanged path repeat itself is disproportionate. A fix that edits the tool-response path itself still drives the MCP tool for real.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=amend-spec -->

### Step 9: Amend the Spec (on divergence)

Conditional on the Step 5 verdict:

- **Conformance** — nothing to amend; skip to the next stage.
- **Divergence** — confirm the amendment with the user first (`AskUserQuestion`: amend the spec / leave as a pending divergence — an unanswered confirmation means do not amend). Then invoke `/awos:spec` in update mode for the owning spec, passing the spec directory and a description of the behavior change (e.g. `/awos:spec amend spec NNN: <what changed and why>`). Its Update Mode edits the affected acceptance criteria in place and appends a dated `## Change Log` entry — no new spec index, and a `Completed` Status is left untouched.

If the fix revealed that `product-definition.md` or `architecture.md` also drifted, surface the same `/awos:product <…>` / `/awos:architecture <…>` suggestions as suggestions, never auto-edits.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=local-review -->

### Step 10: Local Review

First run the static gate: `make test` green (`go fmt ./...` to keep the diff formatted). Then the AI review — it must stay independent of this conversation's authorship bias, so it never happens inline in the orchestrator's own window, which just drove the fix. Dispatch a dedicated **reviewer subagent** (`[Agent: code-reviewer]`, strongest tier) with this fixed verbatim prompt — do not add run-time focus areas drawn from what was fixed:

> Review the diff `git diff fork/master...HEAD` — a bug fix for: {the normalized symptom}. This is a Go MCP server (`github.com/mark3labs/mcp-go`) using the `slack-go` Web API and a custom edge client, with token capability-gating, a JSON file cache, tiered rate limiting, and zap logging. Check that the fix is correct and scope-disciplined, that the regression test targets the changed lines, and check error handling, 429/retry behavior, cache freshness/isolation, token-mode gating, and Go idiom. Confirm no unrelated behavior changed. Write all findings to `context/spec/{SPEC_NAME}/review.md` (or `context/review-{BUG_ID}.md` if the bug maps to no spec), most-severe first. Return only the verdict, the finding count by severity, and that file's path.

Lead the presentation to the user with the review path on its own line — `Review file: <path>` — before the verdict and findings, and record the same path in the flow log. Collect a keep/drop decision (`AskUserQuestion`), apply only accepted findings — via a fresh agent that reads the review file and the diff, never from your summary — and re-run `make test` after fixes.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=commit-push -->

### Step 11: Commit & Push

Write this stage's flow-log entry **before** staging so the log rides in this commit — this is the flow-log's last committed state (see Context Discipline). Then stage all changed files, excluding `.env`, credentials, and secrets. Commit with a Conventional-Commit message referencing the bug (e.g. `fix(channels): refresh counts-only path`; include `Closes #<issue>` when the bug came from a GitHub issue). No pre-commit hooks. Push: `git push -u fork {BRANCH}`.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=remote-gates -->

### Step 12: Remote Gates

From here the PR is open — **do not append to the tracked flow-log** (Context Discipline). Report gate progress to the user instead; resume relies on remote state.

- **Sync check (§2):** before opening the PR, `git fetch fork master` and rebase `BRANCH` onto `fork/master`. On conflicts: delegate resolution to a subagent (per §8), re-run `make test`, and force-push (`git push --force-with-lease fork {BRANCH}`).
- **Open the PR** against the provectus fork — never upstream `origin`:

  ```
  gh pr create --repo provectus/slack-mcp-server --base master --head {BRANCH} --title "<conventional-commit title>" --body "<symptom, root cause, fix; Closes #<issue> if applicable>"
  ```

- **Wait on CI, fixing failures in a loop.** Checks: `Unit Tests` (`make test`) and `Security (Trivy)`. Watch with the `Monitor` tool — a poll loop over `gh pr checks {PR} --repo provectus/slack-mcp-server` emitting each check's terminal result (pass/fail/cancel), timeout ~10 minutes, poll 30s. On a failure, **reuse the `gha-diagnosis` skill** to diagnose from the failed job's logs, apply the fix (delegated to a specialist subagent), push, re-check until green. On a poll-window expiry, auto-relaunch the monitor once; past ~20 minutes unsettled, ask the user (§4).

No automatic reviewer bot and no second human reviewer — CI is the only remote gate.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=merge -->

### Step 13: Ready to Merge

Per §5, **a human merges** — the flow does not merge. Re-check mergeability (the target may have moved while CI ran): `git fetch fork master` and confirm `BRANCH` still rebases cleanly; if not, sync per §2 (resolution delegated per §8), force-push, and return to Step 12 so CI runs again.

Then **stop and report the ready-to-merge state**: the PR link, that every check is green, and the branch. The maintainer merges on GitHub. Do not merge, tag, or deploy.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=close-ticket -->

### Step 14: Close the Loop

Per §5's Definition of Done (ticketless — the report to the user is the close): gather and report the final state — the PR link, all checks green, and the branch. When the bug came from a GitHub issue, the `Closes #<issue>` in the PR body closes it on merge; the flow does not close it directly.

Include the local review and the spec-amendment outcome in the reported evidence, so neither is buried. From the flow log, report: the review **verdict**, the **finding count** (by severity), the **review file path**, that a manual keep/drop gate ran over the findings, and — for a divergence fix — that the owning spec was amended (the criteria touched and the Change Log entry).

Leave a clean working tree: do not write a closing flow-log entry (the log was finalized at commit-push and the PR is now open). If any flow-created artifact is still uncommitted, surface it in the report rather than leaving it behind.

<!-- /awos:flow:stage -->

---

<!-- awos:flow:generated date=2026-07-10 version=2.3.0 source=context/product/delivery-flow.md -->
