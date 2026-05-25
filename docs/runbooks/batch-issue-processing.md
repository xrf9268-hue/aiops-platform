# Batch issue processing

How an agent should run a batch of issues (a `/goal` over a set of tracker
issues) so the work stays reviewable, parallel where it can be, and safe to
merge.

Every rule below is earned by a specific failure or friction observed in the
**2026-05 eight-issue sequential batch** (#365–#372 → PRs #373–#381; one
acceptance-criterion deferral tracked as #375). Per the "Earned rules"
principle in [`AGENTS.md`](../../AGENTS.md), do not generalize beyond what that
run actually taught.

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
3. **Run the local CI gate before pushing**, then confirm CI green and run the
   adversarial pass (`@codex review`) on the head commit. Resolve or
   explicitly defer every finding before marking ready. This is the
   cross-cutting checklist in `AGENTS.md`, applied per PR.

## Parallelize independent issues by default

Sequential execution was the single biggest throughput cost of the 2026-05
run: eight independent issues were processed one after another when most had no
ordering dependency.

1. **Map dependencies before starting.** For the issue set, sketch which issues
   touch overlapping paths or have a real ordering constraint (one's fix
   depends on another's merge). Issues touching disjoint paths are independent.
2. **Default to parallel for independent issues.** Open a branch per
   independent issue and progress them concurrently rather than draining them
   in series. Serialize **only** where a dependency is real — a shared file, a
   migration ordering, or an API a later issue consumes.
3. **Cap concurrency to what you can keep green.** Each in-flight PR still owes
   the full per-PR gate (local CI, CI green, `@codex review` triage). Do not
   open more parallel work than you can drive to mergeable without letting
   reviews rot.
4. **Respect the size budget per PR regardless of parallelism**
   (`policy.max_changed_files: 12`, `policy.max_changed_loc: 300`). Parallelism
   is about more small PRs, never bigger ones.

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

## Keep a live status checklist

Mid-batch, the only way the human could reconstruct state was by reading the PR
list. Maintain a running checklist instead and refresh it on every meaningful
transition:

```text
issue → branch → PR → state (draft | CI green | review clean | mergeable | merged | deferred→#NNN)
```

This is the same discipline `AGENTS.md` requires for PR-activity events; apply
it to the batch as a whole.

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
   there are **zero unresolved, non-outdated review threads**.
3. **Every acceptance criterion is met, or each gap is deferred to a tracked,
   linked issue** (per the deferral protocol above).
4. **The PR is within the policy size budget** and changes no `policy.deny_paths`
   (`infra/**`, `deploy/**`, `db/migrations/**`, `secrets/**`).
5. **The merge uses the agreed method** (default: squash) into the agreed base.

Prefer GitHub's native auto-merge so the platform enforces required checks:
enable auto-merge on the PR rather than issuing an immediate merge, so a
later-arriving failing check still blocks the merge.

Hard stops — **always require human sign-off even under an auto-merge grant:**

- Force-pushing or merging into `main` out of band, or any history rewrite.
- Editing `go.mod`'s `go` directive, or touching `policy.deny_paths`.
- A PR that breached its scope (size budget, unrelated refactors) — flag, don't
  merge.
- Anything the human's instructions for this batch put off-limits. When a PR
  sits at the edge of the grant, use `AskUserQuestion` rather than assuming the
  grant covers it.

Stop the moment the human revokes the grant or asks you to stop.
