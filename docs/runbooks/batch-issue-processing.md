# Batch issue processing

How an agent should run a batch of issues (a `/goal` over a set of tracker
issues) so the work stays reviewable, parallel where it can be, and safe to
merge.

Every rule below is earned by a specific failure or friction observed in batch
runs:

- **2026-05 eight-issue sequential batch** (issues #365–#372 → 8 PRs #373,
  #374, #376–#381; the one acceptance-criterion deferral was filed as a
  separate follow-up, issue #375 — which is why the PR numbers skip it).
- **2026-05 ten-issue automated batch** (issues #384–#393). This run added the
  quality-over-throughput lessons: post-commit dual local review before push,
  fresh `@codex review` trigger tracking, thread-aware review closure, the
  three-state size-gate classification (`within budget` /
  `size-gated: justified overage` / `size-gated: split recommended`) with
  explicit sign-off paths, pause/resume state recovery, and follow-up capture
  for late unresolved review threads.

Per the "Earned rules" principle in [`AGENTS.md`](../../AGENTS.md), do not
generalize beyond what those runs actually taught.

## The unit of work: one issue → one PR

1. **One issue per branch, one branch per PR.** Never bundle multiple issues
   into a single PR. Small blast radius is what makes a batch reviewable: CI
   failures localize to one issue, the human can merge at their own cadence,
   and a revert touches exactly one concern. **Earned by:** the eight-issue
   batch shipped as eight independent PRs and every one was reviewable in
   isolation; the value was in the isolation, not the volume.
2. **Each PR is self-contained and self-verifying.** It includes the fix plus
   regression tests, and the test must fail when the production code is broken
   (mutation or negative assertion — see "Clean code" rule 6 in `AGENTS.md`). A
   PR whose tests would pass with the fix reverted does not count as done.
3. **Run the local CI gate and dual local review before pushing**, then confirm
   CI green and run the adversarial pass (`@codex review`) on the pushed head
   commit. Resolve or explicitly defer every finding before marking ready. This
   is the cross-cutting checklist in `AGENTS.md`, applied per PR.

## Parallelize independent issues by default

Sequential execution was the single biggest throughput cost of the 2026-05
run: eight independent issues were processed one after another when most had no
ordering dependency.

1. **Map dependencies before starting.** For the issue set, sketch which issues
   touch overlapping paths or have a real ordering constraint (one's fix
   depends on another's merge). Issues touching disjoint paths are independent
   — but "disjoint" means more than different files: two issues in the same Go
   package can still collide through shared types, an atomic rename (Clean-code
   rule 3 in `AGENTS.md`), or `go.mod`/`go.sum`. Treat shared package/module
   surface as a dependency, not as independent.
2. **Default to parallel for independent issues.** Open a branch per
   independent issue and progress them concurrently rather than draining them
   in series. Serialize **only** where a dependency is real — a shared file, a
   migration ordering, or an API a later issue consumes.
3. **Cap concurrency to what you can keep green.** Each in-flight PR still owes
   the full per-PR gate (local CI, CI green, `@codex review` triage). The
   worker's per-state capacity caps are the hard ceiling; your review bandwidth
   is the practical one. Do not open more parallel work than you can drive to
   mergeable without letting reviews rot.
4. **Respect the size budget per PR regardless of parallelism**
   (`policy.max_changed_files: 12`, `policy.max_changed_loc: 300`).
   Parallelism is about more small PRs, never bigger ones — but the budget
   is a *gate* to be classified and disclosed, not a ceiling that forces
   LOC cuts at the expense of quality. See "Size gate semantics" below.

### Size gate semantics

Size budget is an auto-merge eligibility gate, not a quality throttle nor an
LOC-reduction mandate. **Quality, correctness, performance, and necessary
regression coverage take precedence over LOC compliance**: if HIGH/MEDIUM
review feedback exposes a real correctness, safety, performance, or coverage
gap, fix it even when that pushes the PR over the default budget. Never
delete meaningful tests, weaken state-machine coverage, drop race coverage,
or prefer compact code over clear reliable code solely to satisfy the
budget.

Classify every PR into exactly one of three states and call it out in the PR
body:

- `within budget` — diff fits the effective budget; standard auto-merge path.
- `size-gated: justified overage` — over the budget because the extra LOC
  pays for correctness, regression coverage, race/state-machine safety, or
  other best-practice hardening that cannot be split without losing
  atomicity. Keep it out of auto-merge and provide a human sign-off bundle:
  why the extra scope is necessary, the final head SHA, local verification,
  CI state, bot review state, unresolved-thread state, and residual risk.
- `size-gated: split recommended` — over the budget because of scope creep,
  unrelated cleanup, or genuinely separable concerns. Stop and split into
  smaller PRs; do not ask for size-gate sign-off in lieu of splitting.

Only reduce LOC when the code is genuinely redundant, over-abstracted,
duplicated without purpose, or outside scope.

## Review the committed diff before every push

The 2026-05 ten-issue batch showed that review before commit is too easy to
invalidate with an amend. Make the commit object the review artifact:

1. **Refresh `origin/main`, then commit or amend first**, with only explicit
   paths staged.
2. **Before every push**, dispatch two independent diff-only reviewers against
   the exact committed range (`origin/main...HEAD`):
   - a Codex reviewer (subagent or `codex exec`)
   - a Claude Code reviewer (subagent or hardened `claude -p`)

   Bare `claude -p` is shorthand for a diff-only invocation that receives the
   diff on stdin, disables tool use, and avoids session carryover, for example
   `claude -p '<review prompt>' --permission-mode bypassPermissions
   --no-session-persistence --tools "" --max-turns 2`.
3. **Do not feed either reviewer your conclusions.** Provide the issue/PR
   intent, head SHA, changed files, relevant SPEC/upstream pointers, and the
   diff. Ask for severity-tagged findings and a final verdict.
4. **HIGH/MEDIUM/Critical findings block the push.** Fix them, amend, and rerun
   both reviewers unless the human explicitly signs off on accepting that risk
   before push. LOW/P3 findings should be fixed when they are small, localized,
   and regression-like; otherwise record the decision in review notes and the
   PR body.
5. **A failed reviewer run is not approval.** Retry with a bounded diff-only
   prompt. If one reviewer family remains unavailable, use an equivalent
   successful diff-only review from the same family or get explicit human
   sign-off before pushing. Record the fallback/sign-off in local review notes
   before a first push, then copy it into the PR body when the PR exists. Do not
   push with only one local reviewer verdict. The pushed PR still needs the
   GitHub `@codex review` gate.

This local review gate is in addition to the GitHub bot review. It catches
pre-push defects; it does not replace the post-push, thread-aware gate. Two
reviewers are the minimum for a pushed head; the blast radius controls how many
rounds you run after that. A pure documentation change may need only one clean
dual-review round, while destructive or concurrent code paths need repeated
rounds until the findings stop.

## Surface deferrals at the moment you defer, not at the end

In the 2026-05 run, one acceptance criterion that could not be met was carried
silently and only disclosed in the final summary, tracked late as #375. That
robbed the human of the chance to weigh in while the context was fresh.

1. **The instant you decide an acceptance criterion cannot be met in this PR,
   say so** — in the PR body and to the human — with the reason and the
   proposed follow-up.
2. **File the follow-up issue immediately** (tagged `area:tech-debt` or
   `area:spec-alignment` as appropriate, per `AGENTS.md`) and link it from the
   PR. A deferral without a tracked issue is just a silent gap.
3. **Never let "done" hide a deferral.** The end-of-batch summary should
   restate deferrals already raised, not introduce them.

If a worker-run agent cannot proceed only because an external dependency is
unresolved (for example an overlapping PR has not merged), it must not rely on
free-text `.aiops/RUN_SUMMARY.md` parsing to pause the worker. It should write
the strict `.aiops/BLOCKED.json` artifact:

```json
{"version":1,"kind":"external_dependency","reason":"PR #455 still open","retry_after_seconds":3600}
```

The worker records this as an `external_blocker` cooldown in the retrying
state. This preserves the SPEC boundary: tracker comments/state changes remain
agent-side, while the worker only holds its scheduler claim until the cooldown
expires and the tracker poll confirms the issue is still active. Keep the wire
shape aligned with
[`docs/protocols/blocked-artifact.schema.json`](../protocols/blocked-artifact.schema.json);
do not add aliases or prose heuristics for old artifacts.

## Keep a live status checklist

Mid-batch, the only way the human could reconstruct state was by reading the PR
list. Maintain a running checklist instead and refresh it on every meaningful
transition:

```text
issue → branch/worktree → PR → head → state
(triaged | in-progress | draft | CI green | bot-review-pending | review clean |
 threads-resolved | within budget | size-gated: justified overage |
 size-gated: split recommended | mergeable | merged | merged-by-user |
 deferred→#NNN | skipped)
```

This mirrors the per-event status-checklist discipline the harness expects when
watching PR activity; apply it to the batch as a whole.

For each PR, also track:

- current head SHA
- validation commands run locally
- CI conclusion and run id
- `@codex review` trigger comment id and reaction state
- unresolved review thread ids, including whether each is outdated
- size-budget classification (`within budget` / `size-gated: justified overage`
  / `size-gated: split recommended`) and, if over budget, the rationale
- deferral/follow-up issue links

Treat the PR body as the public ledger for the same facts. Update it after each
material push instead of leaving stale validation or review status in place.

## Treat bot review and review threads as live gates

`@codex review` is not a normal check run and `latestReviews` can lag the fresh
trigger. For every pushed head:

1. Comment `@codex review` and record that issue-comment id.
2. Wait for the trigger's 👀 reaction to appear, then disappear, and require a
   positive completion signal: Codex posted a review/comment for that head or
   reacted 👍 to the trigger.
3. Query review threads with GraphQL. Flat review comments are not enough.
4. Resolve addressed or outdated threads only after the fresh trigger completes
   and a thread-aware recheck shows no non-outdated actionable threads.
5. If a PR is merged with non-trivial bot review activity, sanity-check the next
   `Capture unresolved reviews` workflow run or linked follow-up issues. That
   workflow is a backstop, not a merge substitute.

## Pause, resume, and external merges

Long batches may cross model quota windows or human intervention. Before
pausing, write down the live ledger: issue, branch/worktree, PR, head SHA, CI
state, trigger comment id, unresolved thread ids, size-gate state, and next
action. Schedule the wakeup only after that state exists.

On resume, do not trust the paused snapshot. Re-fetch `origin/main`, refresh
each live issue/PR with `gh`, and reclassify externally changed work:

- If the human merged a PR, mark it `merged-by-user` and move on.
- If the head changed, restart local verification and review gates for that
  head.
- If a trigger comment is still active, wait for that exact trigger or start a
  fresh review on the current head.
- If unresolved review follow-up issues were created after merge, link them in
  the batch summary and close only after their PRs land.

## Merge and goal-clear are human actions by default

The 2026-05 run deliberately left every merge to the human and could not clear
its own `/goal` at the end (the Stop hook released only after the human ran
`/goal clear`). That is the safe default and it is intentional:

1. **The agent drives each PR to *mergeable*, then stops.** It does not merge,
   it does not force-push `main`, and it does not change `go.mod`'s `go`
   directive (see `AGENTS.md`). These are durable guardrails, not per-session
   reminders.
2. **The batch's terminal state depends on human actions** — merging the PRs
   and clearing the goal. Plan for PRs to queue waiting for a human; that is
   expected, not a stall.

## Authorized auto-merge flow (opt-in, scope-bounded)

The default above (human merges) stands unless the human grants explicit,
scope-bounded authorization for the agent to merge. Authorization is per-batch
and per-scope — "you may merge the PRs for issues #365–#372 once they pass the
gate" — never a standing grant. Approving one merge does not authorize the
next.

When auto-merge **is** authorized, a PR may be merged only when **all** of
these hold:

1. **CI is green** on the head commit (not a stale run).
2. **`@codex review` is clean** — no unresolved HIGH/MEDIUM findings — and
   there are **zero unresolved, non-outdated review threads**. The review must
   be fresh for the current head: trigger-comment reactions have converged and
   thread-aware GraphQL state is clean.
3. **Every acceptance criterion is met, or each gap is deferred to a tracked,
   linked issue** (per the deferral protocol above).
4. **The PR is classified `within budget` and changes no
   `policy.deny_paths`.** Read both from the repo's `WORKFLOW.md`; do not trust
   a hardcoded list — workflows differ (defaults are 12 files / 300 LOC and
   deny `infra/**`, `deploy/**`, `db/migrations/**`, `secrets/**`, but e.g. the
   `github-local` example uses 20 / 600). `size-gated: justified overage`
   requires explicit human size-gate sign-off before merge and is never
   auto-mergeable; `size-gated: split recommended` is a hard stop — split the
   PR instead of asking for sign-off.
5. **Branch protection's required reviews are satisfied.**
6. **The merge uses the agreed method** (default: squash) into the agreed base.

**Native auto-merge enforces only required status checks (CI) and branch
protection's required reviews — not gates 2–4.** `gh pr merge --auto` merges the
instant required CI passes, regardless of an open `@codex` finding, an
unresolved review thread, an unmet acceptance criterion, or a size-budget
breach. So:

- Confirm gates 2–4 **yourself, immediately before** enabling auto-merge.
- A new commit re-opens them — any push restarts the `@codex` round, so
  re-confirm before re-enabling.
- Prefer native auto-merge over an immediate merge only after the non-check
  gates are already confirmed, and only to win the CI race — not as a
  substitute for checking the policy gates.

Hard stops — **always require human sign-off even under an auto-merge grant:**

- Force-pushing or merging into `main` out of band, or any history rewrite.
- Editing `go.mod`'s `go` directive opportunistically, or touching `policy.deny_paths`.
- A PR classified `size-gated: justified overage` — flag, don't merge without
  explicit human size-gate sign-off (acceptable to merge once given). A PR
  classified `size-gated: split recommended` — flag and split; do not seek
  sign-off as a substitute for splitting.
- Anything the human's instructions for this batch put off-limits. When a PR
  sits at the edge of the grant, use `AskUserQuestion` rather than assuming the
  grant covers it.

Stop the moment the human revokes the grant or asks you to stop.
