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
- `failed` — run failed and may retry or be suppressed by deterministic failure rules.
- `blocked` — runtime observed an issue that cannot proceed because a dependency or policy gate is blocking it.
- `input_blocked` — Codex requested operator input or MCP elicitation; the run stopped, remains claimed, and is listed in the top-level `blocked` rows until tracker reconciliation observes the issue outside active states.

These are observability events. They do not imply the worker changed tracker
state, pushed a branch, opened a pull request, or posted a comment. Those writes
belong to the agent/tool workflow boundary from Symphony SPEC section 1.

Input-blocked rows are in-memory runtime state, not durable queue records. A
process restart clears them; if the tracker issue is still active after restart,
the next poll can dispatch it again. Operators should resolve the underlying
request by moving the tracker issue out of active states or by changing the
workflow/prompt so the agent no longer needs unavailable input. The read-only
`/api/v1/state` endpoint also includes top-level `blocked` rows and a
`counts.blocked` value so input-blocked sessions are visible from the HTTP
state surface.

## Branches and pull requests

`branch` and `pr_url` fields are optional. The status surface may display them
when they are discoverable from agent output or runtime events. Their presence
means only that the runtime observed those links; it does not assume the worker
created or owns them.

## HTTP endpoints

When `server.port` is enabled, the worker binds the status API on
`127.0.0.1:<port>` and accepts only `localhost` or `127.0.0.1` Host headers.

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
  retry rows, completed and failed issue IDs, aggregate token/runtime totals,
  and the current poll/concurrency metadata.
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
    "failed": 0,
    "completed_total": 12,
    "failed_total": 3
  },
  "running": [
    {
      "issue_id": "issue-1",
      "issue_identifier": "ENG-1",
      "started_at": "2026-05-21T09:09:55Z",
      "workspace_path": "/var/aiops/workspaces/acme/repo/issue-1"
    }
  ],
  "blocked": [
    {
      "issue_id": "issue-2",
      "issue_identifier": "ENG-2",
      "state": "AI Ready",
      "blocked_at": "2026-05-20T06:05:38Z",
      "session_id": "thread-1-turn-1",
      "method": "mcpServer/elicitation/request",
      "error": "input required: mcpServer/elicitation/request"
    }
  ],
  "retrying": [],
  "completed": [],
  "failed": [],
  "codex_totals": {
    "input_tokens": 0,
    "output_tokens": 0,
    "total_tokens": 0,
    "seconds_running": 0
  }
}
```

### Counts semantics

| Field             | Meaning                                                                       |
| ----------------- | ----------------------------------------------------------------------------- |
| `running`         | Live count of dispatched workers.                                             |
| `blocked`         | Live count of input-blocked rows (Codex elicitation / approval pending).      |
| `retrying`        | Current retry-backoff queue depth.                                            |
| `completed`       | Size of the FIFO-bounded recent-completed set published as `completed`.       |
| `failed`          | Size of the dispatch-suppression set (uncapped; not bounded by the FIFO cap). |
| `completed_total` | Monotonic counter of Succeeded transitions since process start (#234).        |
| `failed_total`    | Monotonic counter of NonRetryableFailed transitions since process start.      |

`completed` and `failed` arrays at the top level publish the recent N issue
IDs in those sets; for lifetime totals across FIFO eviction use the `_total`
counters.

### Top-level metadata fields

- `generated_at` — RFC3339 timestamp the handler stamped when materializing the snapshot.
- `poll_interval_ms` — current tracker poll interval (SPEC §13.7).
- `max_concurrent_agents` — global concurrency cap.
- `max_concurrent_agents_by_state` — optional per-tracker-state cap map; absent when no overrides set.
- `rate_limits` — optional Codex rate-limit snapshot when one has been observed (omitted otherwise).

## Tracker pagination overflow

Both the GitHub adapter (`internal/tracker/github.go`) and the Gitea adapter
(`internal/gitea/tracker_client.go`) cap label-scoped issue listing at a
small number of pages so a pathological repository cannot spend the worker
on a single tracker call. When the cap is reached and the next page is
still non-empty (or carries a `Link: rel="next"` header), the adapter:

1. increments `PaginationCapHits()` so the metric surfaces in operator
   dashboards;
2. logs `… issue pagination exceeded N pages for label "<label>" …`;
3. returns an error from `ListIssuesByStates` / `ListActiveIssues`.

The worker's multi-tracker aggregator (`cmd/worker/main.go`,
`multiTrackerRuntimeClient`) joins per-tracker errors via `errors.Join` and
continues with the other trackers' results, so an overflow on one tracker
does not stop a Linear/GitHub tracker on the same poll tick — but the
per-tick error is still reported and the affected tracker's candidate set
is empty for that tick.

### Triage

If you see this error in a poll tick:

- Identify the label from the error message (`label "<label>"`).
- Check the tracker for the count of issues currently carrying that label.
  If it exceeds the cap (Gitea: 1000 = 20 pages × 50/page; GitHub:
  similarly bounded), the project genuinely has too many active issues for
  the worker's cap to enumerate in one tick.
- Either reduce the active set on the tracker (move terminal issues out of
  active states) or, if the cap is wrong for your scale, raise the
  constant (`listIssuesMaxPages` / `githubMaxIssuePages`) in a follow-up
  PR — do not silence the error.

Gitea previously returned a silently capped slice in this scenario, so
workers missed dispatchable issues beyond the cap. #225 aligned Gitea with
the GitHub adapter's fail-loud semantics.
