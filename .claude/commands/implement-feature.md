---
description: Implements one feature end-to-end — takes a roadmap item, GitHub issue, or description, runs the AWOS chain, reviews and opens a PR against the provectus fork.
argument-hint: '[feature — roadmap item, GitHub issue URL, or description; empty resumes the next roadmap item]'
---

# Implement a Feature End-to-End

Takes one feature — from a `context/product/roadmap.md` item, a GitHub issue, or a free-text description — and drives it through spec, implementation, verification, local review, and a GitHub PR until it is ready to merge. There is no dedicated ticket tracker; work is keyed by its spec directory, and a GitHub-issue source is closed by the PR (`Closes #N`) on merge. Re-run `/awos:flow` to change any delivery decision recorded in `context/product/delivery-flow.md`.

## Arguments

`$ARGUMENTS` — a roadmap item name, a GitHub issue URL or number, or a free-text feature description. If empty, resume from the next incomplete item in `context/product/roadmap.md` (the first unchecked `- [ ]`, as `/awos:spec` does); if the roadmap is missing or fully complete, ask the user.

## Context Discipline

A flow this long degrades in one context window — judgment is worst exactly where it matters most, at review time. Per §8 of `context/product/delivery-flow.md`:

- Run every isolatable stage in a subagent (a subagent can invoke `/awos:*` commands via the Skill tool; its context is discarded on completion). Subagent reports must be terse — paths, verdicts, counts — never full document or review content. For this project the isolatable stages are the mechanical ones (fetch, CI-failure diagnosis, the reviewer, conflict resolution); the AWOS chain itself stays in the main context (see below).
- After each completed stage, append an entry to `context/spec/{SPEC_NAME}/flow-log.md`: the stage name, what was produced and where (paths, branch, commit), any decisions taken along the way, and which stage comes next. `SPEC_NAME` exists once the specs stage creates the spec directory — the stages before it (fetch, resume-detection, workspace) are cheap and re-runnable, so they log nothing; logging starts with the specs stage, and the log's first entry records the feature title so resume can match a log to its feature. The log is the flow's memory outside the context window — a fresh session (after a restart, a crash, or a resumed run) reads this one small file instead of re-deriving state from the whole repo. The log is committed with the work (Step 9 stages it alongside the code), so it must never become an uncommittable leftover: **once the PR is opened, stop writing to the tracked log**, since a commit adding log lines is unwelcome on a PR under review. From that point report late-stage progress to the user, and resume the remote stages from remote state (the open PR, the check status), which the resume-detection stage already inspects. The close stage leaves a clean working tree and never writes a final entry it cannot commit.
- Never launch a nested headless session (`claude -p`) from this command — permission modes, PATH, and timeouts differ per machine. This project uses a manual trigger (§6); there is no unattended chaining.
- Tell every dispatched subagent: tools are functional — do not test them or make exploratory calls; every call needs a purpose. Run each delegated stage on the model tier recorded in §8 — the fast tier for mechanical transport work (fetch, resume checks, first-pass CI-log triage), the strongest for judgment (implementation, review, verification, conflict resolution).
- A subagent's report is a claim, not a fact. Before acting on a report that names files and lines, asserts a root cause, or reports a test outcome, spot-check it — read the named lines, run the named test — rather than relaying it verbatim into the next stage.
- Every fixed-choice interaction with the user — an approval gate verdict, keep/drop on review findings — goes through `AskUserQuestion` with the recorded default marked, never a prose question. Plain prose is only for inherently free-form input (a feature description).
- An unanswered `AskUserQuestion` (the harness returns `No response after 60s` — its guard so runs never hang) is handled by run mode. Read the `AWOS_UNATTENDED` environment variable: when set, take the safe default and continue; when unset the run is interactive, so re-ask once, then proceed naming the default you took so the user can correct it. A timeout never authorizes an irreversible step.

## Self-Improvement Loop

This command is maintained through its own runs. When a run exposes a defect in the flow itself — a recorded fact disproven by reality (a "no X" claim, a dead link, a wrong state name), a missing step (an undocumented bootstrap, a transition chain), or a stage instruction that had to be worked around — fix the flow **in the same run**:

1. Patch the affected file(s) right in this working copy: this command file, `context/product/delivery-flow.md` (correct the fact where it is recorded), or the reused skill.
2. Stage those edits in the commit-push stage alongside the code change — same branch, same PR. Never park flow fixes for a separate PR; a flow that shipped its work while still carrying a known-wrong instruction has not finished the job. A defect found after the PR is open waits: report it as pending in the close-out, and the next run applies it at the workspace stage.
3. Record the correction in the flow log — with the observation that disproved the old text — and promote it into the decision record's **Local Customizations** section, so a future `/awos:flow` regeneration preserves it instead of resurrecting the defect.
4. Two kinds of defect are not yours to fix. A delivery _decision_ (a gate, the merge policy, the autonomy level) belongs to whoever owns the team's process — report the friction and leave the change to a `/awos:flow` re-run. A defect in how `/awos:flow` generated this command cannot be fixed here — tell the user so they can report it to the AWOS repo.

<!-- awos:flow:stage=fetch-ticket -->

### Step 1: Fetch & Normalize the Feature

Resolve the feature from `$ARGUMENTS`:

- **Empty** → read `context/product/roadmap.md` and take the first unchecked `- [ ]` item as the feature (its bold name + description). If the roadmap is missing or fully checked, ask the user for a description.
- **A GitHub issue URL or number** → fetch it via `gh issue view <url-or-number> --repo provectus/slack-mcp-server --comments` (the `github` MCP is the fallback if `gh` fails). Use the issue title + body as the feature; read any linked issues/PRs it references. Keep the **issue number** as `ISSUE_NUMBER` — later stages add `Closes #ISSUE_NUMBER` to the PR so it closes on merge.
- **A description** → use it directly.

Extract and keep a short feature title and its description (and `ISSUE_NUMBER` when the source is a GitHub issue). There is no formal ticket ID; later stages key work by `SPEC_NAME`. This stage is cheap; run it on the fast tier.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=resume-detection -->

### Step 2: Detect the Entry Point

Start with a cheap preflight on the fast model tier (per §8): is this feature **already delivered**? Glob `context/spec/*/` and check whether one owns this feature — if its `functional-spec.md` has `Status: Completed` (or all `tasks.md` items are `[x]`), or a merged PR for its branch already exists on `provectus/slack-mcp-server`, report that and stop rather than re-implementing. When the source is a GitHub issue, also check whether the issue is already closed or a merged PR references it (`gh issue view {ISSUE_NUMBER} --repo provectus/slack-mcp-server`) — if so, report and stop.

Otherwise resolve resume state: `SPEC_NAME` is not known yet on a fresh run — glob `context/spec/*/flow-log.md` and match this feature by the title in each log's first entry; if one matches, read it first — it names the last completed stage and carries the branch, commit, and PR state. The log is a convenience, not ground truth: for the spec-generation stages the on-disk artifacts win when they disagree with the log (a manual or partial rerun can leave it stale) — cross-check `context/spec/`, and if they differ, resume from the first missing artifact and repair the log to match. Past spec generation there is no such artifact to scan, so the log is the only resume signal.

**Pre-generated specs:** a spec directory may already exist for this feature. If so, resume from the first missing artifact — skip `/awos:spec` if `functional-spec.md` exists, skip `/awos:tech` if `technical-considerations.md` exists, skip `/awos:tasks` if `tasks.md` exists.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=workspace -->

### Step 3: Prepare the Workspace

Per §2–§3 of `context/product/delivery-flow.md`:

- Confirm `context/product/` and `context/spec/` exist (they are plain in-repo directories — always reachable).
- Warn on a dirty working tree. Uncommitted AWOS artifacts — `context/product/delivery-flow.md` and this command file, left by `/awos:flow` — are an expected dirty-tree cause; surface them as such rather than treating them as a blocker.
- Fetch the base branch: `git fetch fork master`.
- Create the feature branch from `fork/master` using the `feat/<kebab-slug>` convention (e.g. `feat/group-dm-support`). Store it as `BRANCH`.
- No submodules; this is the main checkout (worktrees are not used for this project).

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=specs -->

### Step 4: Generate Specs and Tasks

Run the AWOS commands sequentially in the **main context** — per §8, every one of them either interviews the user or dispatches its own subagents, so none can be isolated in a subagent:

1. `/awos:spec` — passing the normalized feature as context. **Approval gate:** after it completes, present the `functional-spec.md` path and `AskUserQuestion` (approve / revise) before continuing.
2. `/awos:tech` — **Approval gate:** after it completes, present the `technical-considerations.md` path and `AskUserQuestion` (approve / revise) before continuing.
3. `/awos:tasks` — no gate; proceed straight to implementation. The task list stays revisable by re-running `/awos:tasks`.

Store the spec directory name (e.g. `003-group-dm-support`) as `SPEC_NAME`. Write the first flow-log entry now, recording the feature title.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=commit-specs -->

### Step 5: Commit Specs

Stage `context/spec/{SPEC_NAME}/` and commit with a Conventional-Commit message (e.g. `docs(spec): add {SPEC_NAME} functional and technical spec`). This repo owns `context/`, so the commit lands on `BRANCH`.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=implement -->

### Step 6: Implement via Subagents

Run `/awos:implement` in the **main context** — it dispatches its own coding subagents (per §8, a command that dispatches subagents cannot itself run inside one). It delegates all coding to specialists (`go-mcp-backend`, `release-infra`, `testing-expert`) and tracks progress — do not implement tasks in the main context. Wait for all tasks to complete.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=verify -->

### Step 7: Verify

Run `/awos:verify` in the **main context** (it uses `AskUserQuestion` for criteria without an agent-driven render path), returning the verdict and the list of gaps. Address gaps before proceeding.

Running the app to verify is the flow's job, not the user's. Nothing shared blocks a run (§2): the server binds `127.0.0.1:13080` only in SSE/HTTP mode and unit tests need no running service. For a criterion that needs a live render, build the binary (`make build`) and drive it directly — a stdio MCP handshake, or the SSE endpoint on a free port — and verify against that. Reserve the manual `AskUserQuestion` fallback for a criterion the agent genuinely cannot render here.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=local-review -->

### Step 8: Local Review

First run the static gate: `make test` must be green (and `go fmt ./...` to keep the diff formatted). Then the AI review — it must stay independent of this conversation's authorship bias, so it never happens inline in the orchestrator's own window, which just drove the implementation:

- Dispatch a dedicated **reviewer subagent** (`[Agent: code-reviewer]`, strongest tier) with this fixed verbatim prompt — do not add run-time focus areas drawn from what you implemented; the author framing the review is the bias:

  > Review the diff `git diff fork/master...HEAD` for the feature specified in `context/spec/{SPEC_NAME}/functional-spec.md` and `technical-considerations.md`. This is a Go MCP server (`github.com/mark3labs/mcp-go`) using the `slack-go` Web API and a custom edge client, with token capability-gating, a JSON file cache, tiered rate limiting, and zap logging. Check correctness against the spec's acceptance criteria, token-mode capability gating, error handling and 429/retry behavior, cache freshness/isolation, and Go idiom. Write all findings to `context/spec/{SPEC_NAME}/review.md`, most-severe first. Return only the verdict, the finding count by severity, and that file's path — never the full review body.

- **Lead the review presentation to the user with the path on its own line** — `Review file: context/spec/{SPEC_NAME}/review.md` — before the verdict and findings. A fixed opening line surfaces reliably; a path appended after a long findings list gets dropped. Record the same path in this stage's flow-log entry.
- Collect the keep/drop decisions on the findings with `AskUserQuestion`. A fresh agent that reads the review file and the diff applies the accepted findings — relay the user's decisions, not your own summary. Re-run `make test` after fixes.

The PR opens only after this review passes (change-request timing: after local review, per §4).

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=commit-push -->

### Step 9: Commit & Push

Write this stage's flow-log entry **before** staging so the log rides in this commit — this is the flow-log's last committed state (see Context Discipline). Then stage all changed files, excluding `.env`, credentials, and secrets. Commit with a Conventional-Commit message referencing the feature (e.g. `feat(mpim): add group-DM read support`; append `Closes #ISSUE_NUMBER` when the source was a GitHub issue). There are no pre-commit hooks in this repo. Push `BRANCH` to the fork: `git push -u fork {BRANCH}`.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=remote-gates -->

### Step 10: Remote Gates

From here the PR is open — **do not append to the tracked flow-log** (Context Discipline): a commit adding log lines is unwelcome on a PR under review. Report gate progress to the user instead; resume relies on remote state, not the log.

- **Sync check (§2):** before opening the PR, `git fetch fork master` and rebase `BRANCH` onto `fork/master`. On conflicts: delegate resolution to a subagent (per §8), re-run `make test` on the result, and force-push (`git push --force-with-lease fork {BRANCH}`).
- **Open the PR** against the provectus fork — never upstream `origin`:

  ```
  gh pr create --repo provectus/slack-mcp-server --base master --head {BRANCH} --title "<conventional-commit title>" --body "<summary + link to the spec; add 'Closes #ISSUE_NUMBER' when the source was a GitHub issue>"
  ```

- **Wait on CI, fixing failures in a loop.** Two checks run on the PR: `Unit Tests` (`make test`) and `Security (Trivy)` (fails on CRITICAL/HIGH vuln findings). Watch them with the `Monitor` tool — a poll loop over `gh pr checks {PR} --repo provectus/slack-mcp-server` that emits each check's terminal result (pass/fail/cancel — not just success) and exits when all are settled. Size the timeout to ~10 minutes, poll interval 30s. On a failure, **reuse the `gha-diagnosis` skill** (per §7) to diagnose from the failed job's logs, apply the fix (delegated to a specialist subagent), push, and re-check until green. On a poll-window expiry, auto-relaunch the monitor once; past ~20 minutes unsettled, ask the user (§4 max-wait & escalation).

There is no automatic reviewer bot and no second human reviewer (§4) — CI is the only remote gate.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=merge -->

### Step 11: Ready to Merge

Per §5, **a human merges** — the flow does not merge. Re-check mergeability first (the target may have moved while CI ran): `git fetch fork master` and confirm `BRANCH` still rebases cleanly onto `fork/master`; if not, sync per §2 (resolution delegated per §8), force-push, and return to Step 10 so CI runs again on the new commit.

Then **stop and report the ready-to-merge state**: the PR link, that every check is green, and the branch. The maintainer merges on GitHub. Do not merge, tag, or deploy.

<!-- /awos:flow:stage -->

<!-- awos:flow:stage=close-ticket -->

### Step 12: Close the Loop

Per §5's Definition of Done (the report to the user is the close): gather and report the final state — the PR link, the merge-readiness (all checks green), and the branch. When the source was a GitHub issue, note that the `Closes #ISSUE_NUMBER` in the PR body closes the issue on merge (the flow does not close it directly). Remind the maintainer to **check off the corresponding `context/product/roadmap.md` item** after they merge (for a roadmap-sourced feature), and that a user-facing release is a separate manual `make release TAG=vX.Y.Z` (batched, out of this flow).

Include the local review in the reported evidence — it is a real gate but gets buried otherwise. From the local-review stage's flow-log entry, report: the review **verdict**, the **finding count** (by severity), the **review file path** (`context/spec/{SPEC_NAME}/review.md`), and that a manual keep/drop gate ran over the findings.

Leave a clean working tree: do not write a closing flow-log entry (the log was finalized at commit-push and the PR is now open). If any flow-created artifact is still uncommitted, surface it in the report rather than leaving it behind.

<!-- /awos:flow:stage -->

---

<!-- awos:flow:generated date=2026-07-10 version=2.3.0 source=context/product/delivery-flow.md -->
