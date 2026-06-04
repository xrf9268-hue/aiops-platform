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
A process-local latch recording that this worker observed an operator move the
current issue into a configured terminal tracker state. It makes that terminal
observation authoritative for this worker process: later active candidates for
the same issue ID are suppressed, cleanup-time active rechecks do not resume a
continuation, and current-issue `issueUpdate` calls back into active states are
rejected before tracker HTTP dispatch.
_Avoid_: Agent handoff, clean continuation, tracker write ownership
