# Runtime status surface

The runtime status surface is a lightweight observability view over the
orchestrator's in-process state. It is intentionally not a scheduler database,
not a queue replacement, and not an authority for ticket writes.

## Source of truth

The status payload is produced from `orchestrator.OrchestratorState`, the
single in-memory authority described by Symphony SPEC sections 3.1, 4.1.8, and
7.4. A process restart resets exact scheduler state; recovery comes from the
tracker plus filesystem reconciliation, not from this status payload.

## Event vocabulary

Recent runtime events use the SPEC-aligned operator vocabulary:

- `candidate` — tracker issue was observed as an eligible candidate.
- `running` — candidate was dispatched to an agent run.
- `completed` — run exited successfully or reached workflow handoff.
- `failed` — run exited abnormally; it is rescheduled on the SPEC §8.4 backoff (no deterministic-failure suppression — removed in #584).
- `input_blocked` — Codex requested operator input or MCP elicitation; the run stopped, remains claimed, and is listed in the top-level `blocked` rows until tracker reconciliation observes the issue outside active states.
- `continuation_budget_blocked` — the clean-continuation budget for a still-active issue was exhausted; the run stopped, remains claimed, and is listed in the top-level `blocked` rows with `method: "continuation_budget"`.
- `operator_terminal_stop` — this worker observed a terminal tracker state for the issue without a structured agent-owned current-issue handoff fact and latched that stop locally (D35 / #622).
- `operator_terminal_stop_dispatch_suppressed` — first later active candidate for a latched issue was suppressed instead of being re-dispatched.

These are observability events. They do not imply the worker changed tracker
state, pushed a branch, opened a pull request, or posted a comment. Those writes
belong to the agent/tool workflow boundary from Symphony SPEC section 1.

Blocked rows are in-memory blocked claims, not durable queue records. A blocked
claim has stopped executing and does not consume running-agent concurrency, but
the orchestrator keeps the issue locally claimed until tracker reconciliation
observes it outside active states.

For input-required rows, operators should resolve the underlying request by
moving the tracker issue out of active states or by changing the workflow/prompt
so the agent no longer needs unavailable input.

For `method: "continuation_budget"`, the agent kept cleanly exiting while the
tracker issue remained active until `agent.max_continuation_turns` was exhausted.
The orchestrator passes the remaining clean-turn budget into each fresh or
continuation dispatch, so a single codex app-server session cannot overspend the
issue budget; `agent.max_turns` still caps the session and is never expanded.
Inspect the issue, PR/logs, and workflow prompt, then either move the tracker
issue out of active states or intentionally reissue the work after changing the
task/workflow. Raising `agent.max_continuation_turns` later does not
automatically redrive existing blocked claims; it only affects queued and future
continuation checks. A process restart clears in-memory blocked claims, so if the
tracker issue is still active after restart the next poll can dispatch it again.

The read-only `/api/v1/state` endpoint includes top-level `blocked` rows and a
`counts.blocked` value so blocked claims are visible from the HTTP state surface.

An agent blocked by an external dependency reports it agent-side in its
PR/tracker comment (SPEC section 1); the worker has no blocker artifact and does
not park a dedicated cooldown for it (the `.aiops/BLOCKED.json` →
`kind: "external_blocker"` cooldown was removed as over-design in #572). A
still-active issue rides the normal continuation / §8.4 backoff retry cycle,
re-checking tracker state each poll.

## Branches and pull requests

`branch` and `pr_url` fields are optional. The status surface may display them
when they are discoverable from agent output or runtime events. Their presence
means only that the runtime observed those links; it does not assume the worker
created or owns them.

## HTTP endpoints

When `server.port` is enabled, the worker binds the status API on
`127.0.0.1:<port>` by default. When `AIOPS_STATE_API_TOKEN` is set, every
request must present it as `Authorization: Bearer <token>` or HTTP Basic auth
user `aiops` with the token as the password. Without a token, unauthenticated
requests must have both a loopback Host header and a loopback TCP peer;
non-loopback peers fail closed.

`GET /livez` and `GET /readyz` are the only unauthenticated endpoints on this
listener. They bypass the state API guard and return only `ok`, with no runtime
state or agent text, so container probes can use them without
`AIOPS_STATE_API_TOKEN`. `/livez` proves the HTTP listener is serving requests.
`/readyz` returns `503` until startup has loaded the workflow, constructed the
tracker client, and completed startup reconciliation.

The effective port is resolved in this order, per SPEC §13.7:

1. The CLI flag `--port` if provided. `--port -1` disables the server,
   `--port 0` requests an ephemeral port (useful for tests and scratch
   sessions; the actual bound port is logged at startup), `--port N`
   (1..65535) binds explicitly. The CLI override applies for the
   process lifetime — even across workflow reloads — so an operator
   can pin the listen address without editing the version-controlled
   `WORKFLOW.md`.
2. Otherwise the `server.port` value from the loaded `WORKFLOW.md`.
   The loader rejects `server.port: 0` in a workflow file; if you
   need an ephemeral bind, use `--port 0` instead.
3. Otherwise the schema default of `4000`.

- `GET /api/v1/state` returns the process-wide runtime snapshot: running rows,
  retry rows, completed issue IDs, operator terminal stops, aggregate
  token/runtime totals, and the current poll/concurrency metadata.
- `GET /api/v1/<issue_identifier>` returns one issue's current runtime row.
  Lookup is case-insensitive and matches either the tracker issue identifier or
  issue ID. Unknown issues return
  `{"error":{"code":"issue_not_found","message":"..."}}` with HTTP 404.
- `POST /api/v1/refresh` queues an immediate tracker poll and reconciliation
  cycle. Send `X-AIOPS-Refresh: true`; the non-simple header prevents ordinary
  cross-origin browser posts from triggering local refreshes. Empty bodies and
  `{}` are accepted. The response is HTTP 202:

```json
{
  "queued": true,
  "coalesced": false,
  "requested_at": "2026-05-21T09:10:00Z",
  "operations": ["poll", "reconcile"]
}
```

Repeated refresh requests before the poll loop consumes the wake signal are
coalesced into one extra poll cycle. Unsupported methods on defined endpoints
return HTTP 405 with a JSON error envelope.

## JSON shape

`GET /api/v1/state` returns the wire shape produced by
`cmd/worker.apiStateFromView`. Field names use snake_case; null/zero fields
that have `omitempty` set on the Go struct may be absent. The drift-detection
test (`cmd/worker.TestRuntimeStatusRunbookExampleMatchesHandler`) parses every
JSON code block in this section against the live handler output, so updating
the shape here without updating the handler — or vice versa — fails the build.

```json
{
  "generated_at": "2026-05-21T09:10:00Z",
  "poll_interval_ms": 15000,
  "max_concurrent_agents": 4,
  "max_concurrent_agents_by_state": {"In Progress": 2},
  "counts": {
    "running": 1,
    "blocked": 1,
    "retrying": 0,
    "completed": 0,
    "completed_total": 12,
    "reconcile_stopped_with_progress": 1,
    "reconcile_stopped_with_progress_total": 2,
    "agent_handoff_reconcile_stopped": 3,
    "agent_handoff_reconcile_stopped_total": 4,
    "operator_terminal_stops": 1
  },
  "running": [
    {
      "issue_id": "issue-1",
      "issue_identifier": "ENG-1",
      "state": "In Progress",
      "session_id": "thread-1-turn-1",
      "turn_count": 7,
      "last_event": "turn_completed",
      "last_message": "Working on it...",
      "started_at": "2026-05-21T09:09:55Z",
      "last_event_at": "2026-05-21T09:10:00Z",
      "retry_attempt": 1,
      "workspace_path": "/var/aiops/workspaces/acme/repo/issue-1",
      "tokens": {
        "input_tokens": 1200,
        "output_tokens": 800,
        "total_tokens": 2000
      },
      "codex_app_server_pid": 12345
    }
  ],
  "blocked": [
    {
      "issue_id": "issue-2",
      "issue_identifier": "ENG-2",
      "state": "AI Ready",
      "blocked_at": "2026-05-20T06:05:38Z",
      "workspace_path": "/var/aiops/workspaces/acme/repo/issue-2",
      "session_id": "thread-1-turn-1",
      "last_event_at": "2026-05-20T06:05:30Z",
      "method": "mcpServer/elicitation/request",
      "error": "input required: mcpServer/elicitation/request",
      "codex_app_server_pid": 67890
    }
  ],
  "retrying": [
    {
      "issue_id": "issue-3",
      "issue_identifier": "ENG-3",
      "attempt": 2,
      "kind": "failure",
      "due_at": "2026-05-21T09:11:00Z",
      "error": "retry soon"
    }
  ],
  "completed": [],
  "reconcile_stopped_with_progress": [],
  "agent_handoff_reconcile_stopped": [],
  "operator_terminal_stops": [
    {
      "issue_id": "issue-4",
      "issue_identifier": "ENG-4",
      "state": "Canceled",
      "stopped_at": "2026-05-21T09:12:00Z",
      "suppressed_dispatches": 1,
      "first_suppressed_at": "2026-05-21T09:13:00Z",
      "first_suppressed_state": "In Progress",
      "first_suppressed_reason": "active_candidate_after_operator_terminal_stop"
    }
  ],
  "codex_totals": {
    "input_tokens": 0,
    "output_tokens": 0,
    "total_tokens": 0,
    "seconds_running": 0
  },
  "rate_limits": null
}
```

### Counts semantics

| Field             | Meaning                                                                       |
| ----------------- | ----------------------------------------------------------------------------- |
| `running`         | Live count of dispatched workers.                                             |
| `blocked`         | Live count of input-blocked rows (Codex elicitation / approval pending).      |
| `retrying`        | Current retry-backoff queue depth.                                            |
| `completed`       | Size of the FIFO-bounded recent-completed set published as `completed`.       |
| `completed_total` | Monotonic counter of Succeeded transitions since process start (#234).        |
| `reconcile_stopped_with_progress` | Size of the FIFO-bounded recent set of reconcile-stopped runs that had completed ≥1 agent turn (made progress) before the per-tick reconcile reaped them mid-finalization. Usually the agent's own handoff, but `turn_completed` fires after every turn, so it can also be a run stopped after an intermediate turn — treat it as "reaped after progress, worth inspecting," not a guaranteed success. Surfaced so such a run is visible rather than absent from `completed` (#557). Does not overlap `completed`: a reconcile-stopped run is not a clean exit, so `completed` is unchanged. |
| `reconcile_stopped_with_progress_total` | Monotonic counter of reconcile-stopped-with-progress transitions since process start. |
| `agent_handoff_reconcile_stopped` | Size of the FIFO-bounded recent set of reconcile-stopped runs that observed the guarded `linear_graphql` current issue move to a non-active state before reconcile made the issue ineligible. This covers the successful delivery visibility gap where the agent moved the issue and the worker was stopped instead of exiting cleanly; generic Linear comments, workpad writes, and unrelated mutations do not count as delivery. It does not overlap `completed`, but it may overlap `reconcile_stopped_with_progress` when the handoff also completed a turn before reconcile reaped it. |
| `agent_handoff_reconcile_stopped_total` | Monotonic counter of agent-handoff reconcile-stop transitions since process start. |
| `operator_terminal_stops` | Number of process-local Operator Terminal Stop latches recorded since this worker observed terminal tracker state for the issue without a structured agent-owned current-issue handoff fact. A latched issue is not re-dispatched by this process even if the tracker later reads active. |

There is no `failed` set: per SPEC §8.4/§16.6 a failed run is retried with
backoff (visible under `retrying`), not parked in a suppression bucket — the
former deterministic-non-retryable suppression was removed in #584 (D29).

`completed`, `reconcile_stopped_with_progress`, and
`agent_handoff_reconcile_stopped` arrays at the top level publish the recent N
issue IDs in those sets; for lifetime totals across FIFO eviction use the
`_total` counters. `operator_terminal_stops` publishes full rows instead of
only IDs so operators can see the terminal state, stop time, and first
suppressed active-candidate evidence.

### `codex_totals.seconds_running` semantics

`seconds_running` is a **live aggregate** per SPEC §13.5 Runtime
accounting (#253): the snapshot folds the elapsed time of every
currently-running entry into the cumulative ended-session counter. The
math uses `generated_at` so all running entries are measured against
the same instant. Dashboards polling `/api/v1/state` every few seconds
see a smoothly increasing counter while a long Codex turn works,
rather than a flat number followed by a sudden jump on session end.

Two consequences for dashboard authors:

- Do **not** treat consecutive snapshots' `seconds_running` as a
  delta-encoded stream: snapshot N+1 already includes the elapsed
  time between the two snapshots for any still-running entries.
  Subtracting snapshot N from snapshot N+1 would double-count.
- A run that ends between two snapshots adds its elapsed time
  exactly once. The finished entry is removed from the running set
  before the ended-session counter increments, so the live aggregate
  for that entry stops contributing at the same instant the
  cumulative counter starts including it.

### Top-level metadata fields

- `generated_at` — RFC3339 timestamp the handler stamped when materializing the snapshot.
- `poll_interval_ms` — current tracker poll interval (SPEC §13.7).
- `max_concurrent_agents` — global concurrency cap.
- `max_concurrent_agents_by_state` — optional per-tracker-state cap map; absent when no overrides set.
- `rate_limits` — latest Codex rate-limit snapshot (SPEC §13.7.2). Always
  present: `null` until a `rate_limit_updated` notification is observed, then
  the payload verbatim. The key is never omitted, so dashboards can bind to it
  unconditionally.

### Per-issue running row fields (SPEC §13.7.2)

Each entry in the `running` array follows SPEC §13.7.2:

- `issue_id` / `issue_identifier` — the tracker identity.
- `state` — the tracker state at dispatch (e.g. `In Progress`).
- `session_id` — the live Codex session id (SPEC §4.1.6); absent until the
  runner emits a `session_started` event.
- `turn_count` — running count of `turn_completed` events observed in the
  session.
- `last_event` — the most-recent runner runtime event kind, including
  app-server events from SPEC §10.4 (for example `notification` and
  `turn_completed`) plus runner-synthesized markers. `turn_started` means Codex
  accepted `turn/start` and the runner is waiting for the first streaming
  app-server update.
- `last_message` — the most-recent `payload.message` string from a runtime
  event; sticky across later events that do not include one.
- `started_at` — RFC3339 timestamp the worker spawned at.
- `last_event_at` — RFC3339 timestamp of the last observed runtime event
  (SPEC §13.7.2). Absent until the runner emits its first event.
- `retry_attempt` — retry attempt number when the dispatch is a retry; absent
  on the first run (SPEC §4.1.5 first-run semantic).
- `workspace_path` — absolute path of the per-issue workspace.
- `tokens` — `{ input_tokens, output_tokens, total_tokens }` cumulative for
  the active session.
- `codex_app_server_pid` — OS pid of the Codex subprocess, populated from
  `session_started`; absent when the runner did not emit a pid.

## Tracker pagination overflow

Both the GitHub adapter (`internal/tracker/github.go`) and the Gitea adapter
(`internal/gitea/tracker_client.go`) cap label-scoped issue listing at a
small number of pages so a pathological repository cannot spend the worker
on a single tracker call. The cap is configurable with
`tracker.pagination_max_pages`; `0` or an omitted value keeps the adapter
default. When the cap is reached and the next page is still non-empty (or
carries a `Link: rel="next"` header), the adapter:

1. increments `PaginationCapHits()` so the metric surfaces in operator
   dashboards;
2. logs `… issue pagination exceeded N pages for label "<label>" …`;
3. skips the overflowing label/state collection and continues the rest of the
   tracker poll.

For GitHub, an overflowing open-PR claim scan makes open/all issue collections
unsafe for that tick because later PR pages may already claim those issues. The
adapter logs the cap hit, skips those open/all collections, and still returns
any complete collections that do not depend on open-PR claim suppression.

The worker's multi-tracker aggregator (`cmd/worker/main.go`,
`multiTrackerRuntimeClient`) joins per-tracker errors via `errors.Join` and
continues with the other trackers' results. Pagination cap hits are now
handled inside the GitHub/Gitea adapters before they reach the aggregator, so
one oversized state label no longer empties the whole tracker result set.

### Triage

If you see this diagnostic in a poll tick log:

- Identify the label from the log line (`label "<label>"`).
- Check the tracker for the count of issues currently carrying that label.
  If it exceeds the cap (Gitea: 1000 = 20 pages × 50/page; GitHub:
  similarly bounded), the project genuinely has too many active issues for
  the worker's cap to enumerate in one tick.
- Either reduce the active set on the tracker (move terminal issues out of
  active states) or raise `tracker.pagination_max_pages` after estimating the
  API cost for your repository size.

Gitea previously returned a silently capped slice in this scenario, so
workers missed dispatchable issues beyond the cap. #225 aligned Gitea with
the GitHub adapter's observable cap-hit semantics; #387 keeps the signal but
limits the blast radius to the overflowing label/state.
