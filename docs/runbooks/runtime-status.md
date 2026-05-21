# Runtime status surface

The runtime status surface is a lightweight observability view over the
orchestrator's in-process state. It is intentionally not a scheduler database,
not a queue replacement, and not an authority for ticket writes.

## Source of truth

The status payload is produced from `orchestrator.OrchestratorState`, the
single in-memory authority described by Symphony SPEC sections 3.1, 4.1.8, and
7.4. A process restart resets exact scheduler state; recovery comes from the
tracker plus filesystem reconciliation, not from this status payload.

The payload reports `source: "orchestrator_runtime"` so operators can tell it
is not derived from the transitional Postgres queue.

## Event vocabulary

Recent runtime events use the SPEC-aligned operator vocabulary:

- `candidate` — tracker issue was observed as an eligible candidate.
- `running` — candidate was dispatched to an agent run.
- `completed` — run exited successfully or reached workflow handoff.
- `failed` — run failed and may retry or be suppressed by deterministic failure rules.
- `blocked` — runtime observed an issue that cannot proceed because a dependency or policy gate is blocking it.

These are observability events. They do not imply the worker changed tracker
state, pushed a branch, opened a pull request, or posted a comment. Those writes
belong to the agent/tool workflow boundary from Symphony SPEC section 1.

## Branches and pull requests

`branch` and `pr_url` fields are optional. The status surface may display them
when they are discoverable from agent output or runtime events. Their presence
means only that the runtime observed those links; it does not assume the worker
created or owns them.

## HTTP endpoints

When `server.port` is enabled, the worker binds the status API on
`127.0.0.1:<port>` and accepts only `localhost` or `127.0.0.1` Host headers.

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

The reusable runtime-status JSON writer uses the same queue-independent source:

```json
{
  "source": "orchestrator_runtime",
  "summary": {
    "candidate": 1,
    "running": 1,
    "completed": 0,
    "failed": 0,
    "blocked": 0,
    "retrying": 0
  },
  "running": [],
  "retrying": [],
  "completed": [],
  "recent_events": [],
  "codex_totals": {
    "input_tokens": 0,
    "output_tokens": 0,
    "total_tokens": 0,
    "seconds_running": 0
  }
}
```
