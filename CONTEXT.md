# aiops-platform Context

aiops-platform is a local Symphony-style coding orchestrator. This glossary names
the domain concepts used by its scheduler, tracker loop, and agent runtime.

## Language

**Issue claim**:
A local orchestrator reservation for a tracker issue that prevents duplicate
dispatch while the issue is running, retry-queued, blocked, or between those
states.
_Avoid_: Worker session, queue row, tracker ownership

**Blocked claim**:
An issue claim that is no longer executing but remains locally held until an
operator decision or tracker-state change resolves it.
_Avoid_: External gate, failed issue, tracker blocked

**Clean continuation turn**:
A completed agent turn that exits cleanly while the tracker issue remains
active, meaning the orchestrator would otherwise continue the same claim.
_Avoid_: Continuation attempt, worker session, spawn count

**Clean turn budget**:
The remaining issue-level clean continuation turns passed into one fresh or
continuation dispatch. Hitting this budget is a clean runner stop so the
orchestrator can park the claim in `blocked` with `method=continuation_budget`.
Failure and quota retries do not consume or receive this budget.
_Avoid_: Max-turns override, failure retry cap, quota budget

**Operator Terminal Stop**:
A process-local latch recording that this worker observed the current issue in a
configured terminal tracker state without a structured agent-owned current-issue
handoff fact. It makes that terminal observation authoritative for this worker
process: later active candidates for the same issue ID are suppressed,
cleanup-time active rechecks do not resume a continuation, and current-issue
`issueUpdate` calls back into active states (or unsupported current-issue
`issueUpdate` shapes) are rejected before tracker HTTP dispatch.
_Avoid_: Agent handoff, clean continuation, tracker write ownership

**Binary self-hosted development**:
Using an installed `aiops-platform` worker binary as the orchestrator for work on
the `aiops-platform` repository itself, after the same workflow has passed a
disposable repository smoke test.
_Avoid_: Dogfood, local automation, self-runner

**Ready gate**:
The tracker-side signal that an issue is eligible for unattended dispatch.
Issues without the ready gate remain planning or review material, even if they
are open and well described.
_Avoid_: Open issue, priority, backlog

**Hard dependency**:
An issue dependency where the downstream issue needs an upstream merge, API,
schema, migration, branch base, or atomic refactor before it can be attempted
safely.
_Avoid_: Ordering preference, related issue

**Soft overlap**:
A batch relationship where two issues share review or merge risk through common
files, packages, modules, generated artifacts, or dependency manifests, even if
one does not strictly require the other.
_Avoid_: Independent work, harmless overlap

**Independent issue**:
A tracker issue whose implementation and review can proceed without another
issue's branch, contract, files, or merge result.
_Avoid_: Parallel task, unrelated ticket

**External worker mode**:
Running `aiops-platform` outside a target repository while that repository owns
only its workflow contract, issue readiness rules, and verification commands.
_Avoid_: Embedded worker, project-local platform

**Small PR auto-merge**:
Scope-bounded authorization for automation to merge only PRs that remain within
the review budget and have passed every code, CI, review-thread, and issue
authorization gate.
_Avoid_: Standing auto-merge, merge on green
