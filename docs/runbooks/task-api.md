# Runtime Debugging API

The worker exposes a lightweight runtime debugging API on the same listener as
the web dashboard. It reads the in-memory orchestrator state; it is not a
scheduler database and does not replace tracker polling or workspace
reconciliation.

By default the listener binds to `127.0.0.1:4000`. When
`AIOPS_STATE_API_TOKEN` is set, include either `Authorization: Bearer <token>`
or HTTP Basic auth user `aiops` with the token as the password.

## State Snapshot

```bash
curl 'http://127.0.0.1:4000/api/v1/state'
```

Returns the process-wide runtime snapshot: running rows, blocked rows, retry
rows, completed issue IDs, aggregate token/runtime totals, and the
current poll/concurrency metadata. See `docs/runbooks/runtime-status.md` for
the full JSON shape.

## Issue Snapshot

```bash
curl 'http://127.0.0.1:4000/api/v1/ENG-123'
```

Returns one issue's current runtime row. Lookup is case-insensitive and matches
either the tracker issue identifier or issue ID. Unknown issues return
`issue_not_found` with HTTP 404.

## Refresh

```bash
curl -X POST -H 'X-AIOPS-Refresh: true' \
  'http://127.0.0.1:4000/api/v1/refresh'
```

Queues an immediate tracker poll and reconciliation cycle. Repeated refresh
requests before the poll loop consumes the wake signal are coalesced into one
extra poll cycle.

## Events And Artifacts

The API exposes runtime rows, not the old task-event SQL stream. For per-run
details, use process logs and workspace artifacts:

- `.aiops/PROMPT.md` — rendered prompt sent to the runner
- `.aiops/TASK.md` — task description
- `.aiops/PLAN.md` — agent's assessment handoff (analysis-only mode)

Failure reasons are not persisted as a workspace file: the worker emits them in
the structured event log (`runner_end` with an `error` payload,
`run_phase_transition`). The `.aiops/FAILURE.md` post-mortem and
`.aiops/CHANGED_FILES.txt` snapshot were removed in #575 as duplicates of that
log.

Important task event kinds emitted into the worker log/event emitter include:

- `run_phase_transition` — SPEC run-attempt phase transitions
- `runner_start`, `runner_end`, `runner_timeout`
- `workflow_resolved`, `hook_start`, `hook_end`

Push, PR creation, and tracker writes are agent-side responsibilities per SPEC
section 1, so current worker success paths must not emit worker-owned
`push`/`pr_created` handoff events.
