# Design: D21 + D6 — single-source-of-truth orchestrator state

**Status:** Draft for review
**Tracks:** #95 (D21), #73 (D6)
**Phase:** 1 of 6 in the SPEC alignment plan
**SPEC sections:** §3.1, §4.1.7, §4.1.8, §7.4, §8, §14.3, §16
**Source for review target:** SPEC.md only; Elixir code is a disambiguation oracle, not a porting target

## Goal

Replace the Postgres-backed task queue (D6) with a single in-memory
`OrchestratorState` (D21) that owns every scheduling decision, mutates state
through one serialized authority, and rebuilds itself on restart from the
tracker + workspace filesystem rather than from a database.

## Scope

In scope:

- New `internal/orchestrator` package owning `OrchestratorState` (§4.1.8) and
  serializing every mutation through one goroutine (§7.4).
- Move claim / dispatch / retry / reconciliation out of the SQL queue and into
  that package.
- Delete `internal/queue/`, `migrations/`, and the `database/sql` + `pgx`
  dependency from the worker path.
- Implement the §14.3 + §8.6 restart recovery path.

Deferred to other deviations (full list under "What this design does NOT do"
below). The interesting coordination cases: **D13** (workspace rekey) is
required for reconciliation cleanup to work correctly; **D7** (Gitea
webhook retirement) gates final queue-package deletion.

## SPEC requirements

Direct quotes the design must satisfy:

- **§3.1 item 4:** the Orchestrator "Owns the poll tick. Owns the in-memory
  runtime state. Decides which issues to dispatch, retry, stop, or release.
  Tracks session metrics and retry queue state."
- **§4.1.8 (Orchestrator Runtime State):** "Single authoritative in-memory
  state owned by the orchestrator. Fields: `poll_interval_ms`,
  `max_concurrent_agents`, `running` (map `issue_id -> running entry`),
  `claimed` (set of issue IDs reserved/running/retrying), `retry_attempts`
  (map `issue_id -> RetryEntry`), `completed` (set of issue IDs;
  bookkeeping only, not dispatch gating), `codex_totals`, `codex_rate_limits`."
- **§4.1.7 (RetryEntry):** `issue_id`, `identifier`, `attempt`, `due_at_ms`
  (monotonic), `timer_handle`, `error`.
- **§7.4:** "The orchestrator serializes state mutations through one
  authority to avoid duplicate dispatch. `claimed` and `running` checks
  are REQUIRED before launching any worker. Reconciliation runs before
  dispatch on every tick. Restart recovery is tracker-driven and
  filesystem-driven (without a durable orchestrator DB)."
- **§14.3:** "No retry timers are restored from prior process memory.
  No running sessions are assumed recoverable. Service recovers by:
  startup terminal workspace cleanup; fresh polling of active issues;
  re-dispatching eligible work."
- **§16.1 reference algorithm:** the `state = { running: {}, claimed: set(),
  retry_attempts: {}, completed: set(), codex_totals: {...},
  codex_rate_limits: null }` initializer, followed by
  `startup_terminal_workspace_cleanup()` and `schedule_tick(delay_ms=0)`.

§7.4 says "MUST serialize through one authority" indirectly via "to avoid
duplicate dispatch" + §4.1.8 "Single authoritative in-memory state". The
Elixir reference disambiguates *how*: one GenServer owns the state and every
mutation flows through `handle_call` / `handle_cast` / `handle_info`
(`/tmp/symphony-spec/orchestrator.ex:6,52,74-217`). The Go analog is the
single-goroutine actor selected below.

## Current Go state (what we are replacing)

- **Worker entry point.** `cmd/worker/main.go:25-35` opens a `pgxpool`,
  constructs `queue.Store`, hands it to `worker.Run`. There is no
  orchestrator type; "the orchestrator" is `worker.Run` + a SQL query.
- **Worker loop.** `internal/worker/run.go:89-138` is a single-goroutine
  loop calling `store.Claim` → `runTask` → `store.Complete` (or
  `handleTaskFailure`) per iteration. Cross-worker concurrency is enforced
  entirely by `FOR UPDATE SKIP LOCKED` (`internal/queue/postgres.go:50-71`),
  not by any in-process authority.
- **Postgres schema.** `migrations/001_init.sql` defines `tasks`
  (status, attempts, available_at) and `task_events` (event_type, message,
  payload). SPEC has neither.
- **Retry encoding.** `internal/queue/postgres.go:146-184` encodes retries
  as `available_at = now() + interval '60 seconds'` plus an `attempts`
  counter. SPEC §4.1.7 wants a typed `RetryEntry` with `due_at_ms`,
  `timer_handle`, `attempt`, `error`. The 60-second fixed delay is also
  wrong (D16, deferred).
- **No aggregate state.** No `running` map, no `claimed` set, no
  `retry_attempts` map, no `codex_totals`. Status is split across
  `tasks.status`, the `task_events` log, and ad-hoc fields on Linear
  (`internal/worker/transitions.go`).

## Proposed Go state (what we are building)

### Types

New package `internal/orchestrator`. Field names mirror SPEC §4.1.8 exactly:

```go
package orchestrator

type IssueID string // == tracker.Issue.ID

type OrchestratorState struct {
    PollIntervalMs      int64
    MaxConcurrentAgents int
    Running             map[IssueID]*RunningEntry
    Claimed             map[IssueID]struct{}           // §4.1.8 "set"
    RetryAttempts       map[IssueID]*RetryEntry
    Completed           map[IssueID]struct{}           // bookkeeping only
    CodexTotals         CodexTotals
    CodexRateLimits     *RateLimitSnapshot             // nil-able
}

type RunningEntry struct {
    Issue        tracker.Issue
    Identifier   string
    StartedAt    time.Time
    RetryAttempt *int                                  // null on first run (§4.1.5)
    Workspace    workspace.Workspace                   // path + key + created_now

    // §4.1.6 live-session fields, populated by D1's app-server client.
    Session     LiveSession
    LastCodexAt time.Time                              // §8.5 Part A input (D14)

    // Handles for reconciliation termination (§8.5 Part B).
    CancelWorker context.CancelFunc
    Done         <-chan struct{}                       // closed when worker exits
}

type RetryEntry struct { // §4.1.7
    IssueID    IssueID
    Identifier string
    Attempt    int                                     // 1-based
    DueAt      time.Time                               // Go time.Time is monotonic via runtime
    Timer      *time.Timer                             // == "timer_handle"
    Error      string
}

type CodexTotals struct { // §4.1.8 + §13.3
    InputTokens, OutputTokens, TotalTokens int64
    SecondsRunning                         float64
}
```

`LiveSession` and `RateLimitSnapshot` are deliberate stubs so D1's app-server
client can fill them without another type churn.

### State transitions

| Trigger | Effect on state |
|---------|-----------------|
| Poll tick, candidate eligible (§8.2) | `Claimed[id]={}`; `Running[id]=…`; `RetryAttempts[id]` deleted; worker goroutine spawned (§16.4) |
| Worker exit, normal (§7.3) | `Running[id]` removed; `Completed[id]={}`; `CodexTotals.SecondsRunning += elapsed`; continuation retry scheduled with attempt=1, delay=1000ms (§8.4) |
| Worker exit, abnormal (§7.3) | `Running[id]` removed; `CodexTotals` updated; exponential-backoff retry scheduled (§8.4; D16 fills the formula) |
| `RetryEntry.Timer` fires (§7.3, §16.6) | Re-fetch candidates; if found+eligible+slot free → dispatch (row 1); else re-queue with `attempt+1`; else release claim |
| Reconciliation: tracker state terminal (§8.5 Part B) | `CancelWorker()`; workspace cleanup queued; `Claimed[id]` + `RetryAttempts[id]` removed |
| Reconciliation: tracker state non-active, non-terminal (§8.5 Part B) | `CancelWorker()` without workspace cleanup; `Claimed[id]` removed |
| Stall detected (§8.5 Part A) | `CancelWorker()`; treated as abnormal exit. **Deferred to D14**; hook is no-op here. |
| Codex update event (§7.3) | `Running[id].LastCodexAt = now()`; tokens / rate-limit updated. **Deferred to D1**; channel exists, no producer yet. |

`Completed` is bookkeeping only per §4.1.8 — never read for dispatch gating.

### Mutation discipline

**Decision: single-goroutine actor pattern.**

```go
type stateOp interface{ apply(*OrchestratorState) }

type Orchestrator struct {
    ops      chan stateOp
    tracker  tracker.Client
    spawn    func(ctx context.Context, issue tracker.Issue, attempt *int)
    sched    Scheduler              // backoff seam; D16 swaps the impl
    cfgWatch <-chan workflow.Config // future: D11
}

func (o *Orchestrator) run(ctx context.Context, st OrchestratorState) {
    for {
        select {
        case <-ctx.Done():
            return
        case op := <-o.ops:
            op.apply(&st)
        }
    }
}
```

Justification: SPEC §7.4 says "serialize through one authority". The actor
pattern is the smallest Go construct that makes the authority visible: the
goroutine reading `ops` is the only authority, by construction. A
`sync.Mutex` satisfies "serialize" but any goroutine that takes the lock is
*an* authority — there is no single one. Elixir uses GenServer for the same
reason (`orchestrator.ex:6,52,74-217`); the Go actor is the direct analog.

**Invariant:** `stateOp.apply` must never send on `o.ops` synchronously (it
would deadlock against a full buffer). Long operations (worker spawn,
tracker fetch, timer scheduling) return a follow-up action that the actor
fires *after* `apply` returns. The actor test in PR 2 enforces this.

### Recovery model

§14.3 forbids restoring retry timers or running sessions from prior
memory. Restart sequence:

```text
1. Build fresh OrchestratorState{} with PollIntervalMs, MaxConcurrentAgents
   from workflow config (§16.1).
2. tracker.FetchIssuesByStates(terminal_states); for each, best-effort
   workspace.RemoveWorkspace(issue). Fetch failure logged + ignored (§8.6).
3. schedule_tick(delay_ms=0). The first tick fetches active candidates and
   dispatches up to MaxConcurrentAgents.
4. Workspaces on disk for active-state issues are left alone; the next
   dispatch reuses them via workspace.PrepareGitWorkspace (§9.1
   "Workspaces are reused across runs for the same issue").
```

No filesystem scan repopulates `Running`. §14.3 is explicit: "No running
sessions are assumed recoverable."

### HTTP surface (deferred? or in scope?)

**Deferred.** §13.7 is OPTIONAL ("not REQUIRED for conformance", line
1350); endpoints all read from `OrchestratorState`. Bundling the HTTP
server here would add ~300 LOC of handlers/tests without changing
what's under review. In scope: a programmatic `Orchestrator.Snapshot()
StateView` (shape per §13.3: `running`, `retrying`, `codex_totals`,
`rate_limits`) that the later HTTP handler, CLI status, and tests all
consume.

## Migration plan

Six PRs. Each leaves the worker test suite green; the only intentionally-red
test set is `internal/queue/` itself in step 6, because the package is
being deleted.

1. **Introduce `internal/orchestrator` with state types, no callers.**
   Adds `state.go`, `retry.go`, transition-table unit tests. Worker still
   on Postgres. Reviewer sees: a new package, no production wiring yet.
2. **Add the actor + a fake `Dispatcher` seam.** Adds `actor.go`,
   concurrency tests (`-race`) for: concurrent claim attempts on the same
   issue produce exactly one `Running` entry; retry-timer races produce
   exactly one re-dispatch.
3. **Migrate the poll tick from `cmd/linear-poller` + queue to the
   orchestrator.** `cmd/linear-poller` becomes a tick scheduler calling
   `Orchestrator.Tick(ctx)` (implements §16.2: reconcile → validate →
   fetch → sort → dispatch). The `queue.Store.Enqueue` path is removed
   from this binary. Worker still claims from Postgres — two parallel
   paths coexist for one PR.
4. **Move the worker loop under the orchestrator.** `internal/worker/run.go`
   loses the `Claim → Complete` loop; the worker becomes the function the
   orchestrator's `spawn` callback invokes. `runTask` body is preserved;
   only the outer plumbing changes. `cmd/worker/main.go` opens an
   orchestrator instead of a Postgres pool. Existing `runStore`
   interface (`internal/worker/run.go:20-25`) was shaped for fakeability
   — tests swap their fake for the spawn callback with one rename.
5. **Rip Postgres from the worker path.** `internal/queue` stays alive
   for `cmd/trigger-api` (D7's cleanup, separate PR). The `EventEmitter`
   interface (`internal/worker/runtask.go:24-27`) is retained but its
   production impl switches from `*queue.Store` to an in-memory
   `orchestrator.Recorder` (ring-buffered per `RunningEntry`).
6. **Delete `internal/queue/`, `migrations/`, Postgres imports.**
   Requires D7 (#74) to have retired `cmd/trigger-api`. If D7 slips,
   PR 6 splits: `cmd/worker` is Postgres-free here; queue package
   deletion waits.

### Test discipline between steps

- Step 1–2: new package tests stand alone; no production-code churn.
- Step 3: `cmd/linear-poller`'s SQL-touching tests get replaced by
  orchestrator-tick tests. Original keeps running under a build tag for
  one step (existing `-tags e2e` precedent in `test/e2e/`).
- Step 4: `internal/worker/run_test.go` translates "queue saw event X"
  assertions to "orchestrator saw event X". Per AGENTS.md §test
  discipline, deleted tests carry a rationale in the PR body.

### Should Postgres support live behind a flag during migration?

**No.** §14.3 says scheduler state is intentionally in-memory; a
`--use-postgres` flag would be a permanent extension surface (AGENTS.md
§Behavior first) no SPEC clause asks for. Data "lost" on cutover is
in-flight tasks; §14.3 says "No running sessions are assumed
recoverable" — losing them is conformant. Reconciliation rebuilds from
the tracker on the next tick.

## What this design does NOT do

- **D1 (app-server protocol):** `RunningEntry.Session` is a stub.
- **D11 (fsnotify reload):** `cfgWatch` is a sketched channel; the
  watcher itself lands with #85.
- **D13 (workspace rekey):** uses `workspace.Manager.PathFor(task)` as
  today. **Coordination flag:** D13 changes what counts as "the
  workspace for issue X". Without it, reconciliation's
  `workspace.RemoveWorkspace(issue)` cannot locate the directory a
  previous run created (which lives at `<root>/<repo>_<owner>/<task_id>`).
  PR 4's body must call out the D13 dependency; ideally D13 lands first.
- **D9 / D14 (per-tick reconciliation, stall detection):** hooks exist;
  the bodies remain no-ops or skeletons.
- **D16 (exponential backoff + continuation retries):** ships a
  `FixedDelayScheduler{Delay: 60s}` matching today's behavior.
- **D22 event vocabulary:** legacy events from `internal/task/task.go:23-40`
  keep flowing through the in-memory ring buffer; D22 replaces them.
- **§13.7 HTTP server:** `Snapshot()` only; handlers later.

## Open design questions

**Q1: Collapse `Claimed` into `Running`, or preserve both?**

SPEC §4.1.8 lists them as distinct. §7.1 clarifies: "In practice, claimed
issues are either `Running` or `RetryQueued`." The "claimed but not yet
running" window exists between dispatch decision and goroutine spawn.

- *Default:* preserve both, literal to §4.1.8. `Claimed[id]` is added at
  the start of dispatch, removed only on retry release or termination.
- *Alternative:* collapse into `Running`.
- **Recommendation: preserve both.** Cost is one map; benefit is that
  §7.1's "either Running or RetryQueued" answers from state alone,
  making §13.7's `/api/v1/state` straightforward.

**Q2: A workspace exists on disk but the issue has no `Running`, no
`Retry`, no `Claimed` entry. What happens?**

- *Default:* leave the directory; the next dispatch reuses it via
  `workspace.PrepareGitWorkspace`. §14.3 says recovery is "fresh polling
  of active issues, re-dispatching eligible work" — not "rebuild Running
  from the filesystem". §9.1 says workspaces are reused.
- *Alternative:* walk `workspace.Root` and reconcile orphaned
  directories against tracker state.
- **Recommendation: default.** The alternative re-introduces
  persistent-state semantics through the filesystem, which §14.3
  explicitly rejects.

**Q3: When reconciliation terminates a `Running` entry, signal or
ctx-cancel only?**

SPEC §16.3 says `terminate_running_issue(state, id, …)`. Elixir
(`orchestrator.ex:415`) calls `terminate_task(pid)` which gracefully
attempts then `:kill`s; the Erlang process model differs from Unix.

- *Default:* `ctx.Done()` only. The worker reads `runCtx.Err()` after
  each blocking call.
- *Alternative:* typed message on a per-worker reply channel.
- **Recommendation:** ctx + `context.WithValue(runCtx, reasonKey,
  ReasonCanceledByReconciliation)`. The worker stamps the exit event
  with §7.2's `CanceledByReconciliation` phase without a second
  channel.

## Risks

- **Actor pattern is alien to most existing Go code in this repo.** The
  worker today is straight-line procedural. Reviewers will need to
  follow message handlers where they previously read a function. Each
  `stateOp` type lives next to its handler; the actor file header
  cross-references §7.4 and `orchestrator.ex:74-217`.
- **Test rewrites are unavoidable.** `internal/worker/run_test.go` and
  `internal/queue/postgres_test.go` are heaviest. PR 4 absorbs the
  worker churn; PR 6 deletes the queue tests with stated rationale.
- **Postgres rip is irreversible for in-flight tasks at cutover.**
  Per §14.3 this is conformant — reconciliation rebuilds from the
  tracker — but operators should drain before deploying. PR 6 body
  includes a drain checklist.
- **D7 coupling.** PR 6 cannot delete `internal/queue/` until
  `cmd/trigger-api` is retired by #74. If it slips, PR 6 splits.
- **In-memory event log loses history on restart.** `task_events`
  persists today; the new design does not. Operators using it for
  ad-hoc forensics lose that audit trail; structured logs (§3.1) remain
  on disk via the existing sink.
- **Actor deadlock from re-entrant sends.** If `stateOp.apply`
  synchronously sends back on `o.ops`, the actor deadlocks. Convention
  is "return a follow-up action, fire after apply"; PR 2 ships an
  explicit unit test for it.
- **D13 coupling.** Reconciliation's `RemoveWorkspace(issue)` only
  works if workspaces are keyed by issue identifier, not task ID.
  Without D13, this PR's cleanup is best-effort.

## Success criteria

Observable, end-to-end:

- `cmd/worker/main.go` boots without `database/sql` / `pgx` imports
  (`go list -deps ./cmd/worker | grep pgx` is empty).
- `migrations/` directory is gone.
- `internal/queue/` package is gone (after PR 6).
- `kill -9 <worker-pid>` + restart reconstructs scheduling state by
  polling the tracker within `poll_interval_ms`; no `tasks` table is
  consulted because none exists.
- A Linear issue moved to a terminal state while a worker is running
  has its `RunningEntry` removed within one tick (§8.5 Part B). Full
  Part A stall detection lands with D14, but the hook is exercised.
- `Orchestrator.Snapshot()` returns a `StateView` matching the §13.3
  shape, so the §13.7 HTTP server is a thin wrapper.
- All existing tests pass; deleted tests have a rationale per
  AGENTS.md §test discipline. Bounded set:
  `internal/queue/postgres_test.go` (intentional); SQL-touching
  subset of `cmd/linear-poller` tests (replaced in PR 3).
- `go test -race ./...` passes for 10 consecutive runs against the
  new orchestrator package.
- DEVIATIONS.md: D6 moves `Reverting → Removed`; D21 moves `Open →
  Removed`; D9 / D14 / D15 / D16 / D19 are unblocked (dependency
  arrows already in DEVIATIONS.md).
