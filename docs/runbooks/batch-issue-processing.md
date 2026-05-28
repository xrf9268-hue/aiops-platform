# Batch issue processing

How an agent should run a batch of issues (a `/goal` over a set of tracker
issues) so the work stays reviewable, parallel where it can be, and safe to
merge.

> **The per-PR review & merge gates are shared.** Dual diff-only reviewers,
> `@codex review` convergence, GraphQL review-thread closure, the size-gate
> three states, the merge / authorized-auto-merge gate list, and regression +
> mutation test discipline live once in
> [`pr-review-merge-protocol.md`](pr-review-merge-protocol.md). This runbook
> covers only the **batch-orchestration delta** — parallelism, the live ledger,
> deferral timing, pause/resume, and the *opt-in* authorization wrapper around
> the shared merge gate.

Every rule below is earned by a specific failure or friction observed in batch
runs:

- **2026-05 eight-issue sequential batch** (issues #365–#372 → 8 PRs #373,
  #374, #376–#381; the one acceptance-criterion deferral was filed as a
  separate follow-up, issue #375 — which is why the PR numbers skip it).
- **2026-05 ten-issue automated batch** (issues #384–#393). This run added the
  quality-over-throughput lessons now in the shared protocol (post-commit dual
  local review before push, fresh `@codex review` trigger tracking,
  thread-aware review closure, the three-state size-gate classification with
  explicit sign-off paths) plus the batch-level pause/resume state recovery and
  follow-up capture for late unresolved review threads.

Per the "Earned rules" principle in [`AGENTS.md`](../../AGENTS.md), do not
generalize beyond what those runs actually taught.

## The unit of work: one issue → one PR

1. **One issue per branch, one branch per PR.** Never bundle multiple issues
   into a single PR. Small blast radius is what makes a batch reviewable: CI
   failures localize to one issue, the human can merge at their own cadence,
   and a revert touches exactly one concern. **Earned by:** the eight-issue
   batch shipped as eight independent PRs and every one was reviewable in
   isolation; the value was in the isolation, not the volume.
2. **Each PR is self-contained and self-verifying** — fix plus regression tests,
   mutation-verified per the protocol's test discipline
   ([protocol §1](pr-review-merge-protocol.md#1-test-discipline-regression--mutation-verification)).
   A PR whose tests would pass with the fix reverted does not count as done.
3. **Run the full per-PR gate for each PR** — local CI gate, dual local review
   before push, CI green, and `@codex review` on the pushed head — exactly as
   the shared protocol specifies. This runbook does not change those gates; it
   only governs how many run in parallel.

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
4. **Parallelism means more small PRs, never bigger ones.** The size budget
   still applies per PR and is classified/disclosed per
   [protocol §6](pr-review-merge-protocol.md#6-size-gate-is-a-merge-gate-not-an-loc-reduction-mandate)
   — it is a gate, not a ceiling that forces LOC cuts at the expense of quality.

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

For each PR, also track the per-PR ledger facts the protocol already requires
([protocol §7](pr-review-merge-protocol.md#7-pr-body-is-a-living-ledger)) —
current head SHA, local validation commands, CI conclusion/run id,
`@codex review` trigger id + reaction state, unresolved review-thread ids (and
whether each is outdated), size-gate classification, and deferral/follow-up
links — and keep the PR body in sync as the public copy of the same facts.

## Pause, resume, and external merges

Long batches may cross model quota windows or human intervention. Before
pausing, write down the live ledger: issue, branch/worktree, PR, head SHA, CI
state, trigger comment id, unresolved thread ids, size-gate state, and next
action. Schedule the wakeup only after that state exists.

On resume, do not trust the paused snapshot. Re-fetch `origin/main`, refresh
each live issue/PR with `gh`, and reclassify externally changed work:

- If the human merged a PR, mark it `merged-by-user` and move on.
- If the head changed, restart local verification and the review gates for that
  head (protocol §3–§5).
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

## Authorized auto-merge (opt-in, scope-bounded)

The default above (human merges) stands unless the human grants explicit,
scope-bounded authorization for the agent to merge — "you may merge the PRs for
issues #365–#372 once they pass the gate" — never a standing grant. Approving
one merge does not authorize the next.

When auto-merge **is** authorized, apply the full merge gate and hard-stop list
in [protocol §8](pr-review-merge-protocol.md#8-merge): CI green on the head,
fresh `@codex review` clean with zero unresolved non-outdated threads, every
acceptance criterion met or deferred to a tracked issue, classified
`within budget` and changing no `policy.deny_paths`, required reviews
satisfied, agreed squash method. Native auto-merge enforces only CI + required
reviews, so confirm the non-check gates yourself immediately before enabling it,
and re-confirm after any push (a new commit re-opens the `@codex` round).

Stop the moment the human revokes the grant or asks you to stop.
