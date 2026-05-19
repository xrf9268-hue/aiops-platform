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

## JSON shape

The initial implementation exposes a reusable JSON writer used by CLI or
read-only endpoint wrappers:

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
    "InputTokens": 0,
    "OutputTokens": 0,
    "TotalTokens": 0,
    "SecondsRunning": 0
  }
}
```
