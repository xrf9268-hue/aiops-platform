# Architecture

`aiops-platform` is a personal-productivity AI coding orchestrator: it watches a
tracker for issues that are ready for an agent, runs each one through a
deterministic, repo-owned workflow, and hands the result back as a draft pull
request for a human to review. It deliberately keeps the agent — not the
platform — responsible for the irreversible steps (source edits, verification,
push, PR creation, tracker state writes), so the platform stays a thin,
auditable conductor.

This document describes the **current** design. For the rationale behind the
Symphony-style approach see [ADR 0001](adr/0001-symphony-style-personal-orchestrator.md).

## System data flow

A single long-lived process (`cmd/worker`) runs the orchestrator actor. It polls
the tracker, reconciles in-memory state against what the tracker reports, claims
issues that are ready, and dispatches each as a run. There is no external job
queue — the claim/dispatch state lives in the actor and is rebuilt from tracker
polling on startup.

```mermaid
flowchart TD
    subgraph trackers["Trackers"]
        LIN["Linear"]
        GIT["Gitea"]
        GH["GitHub"]
    end

    subgraph proc["cmd/worker process"]
        ORCH["Orchestrator actor<br/>poll · reconcile · claim · dispatch"]
        WORK["Worker run loop<br/>resolve workflow · build prompt · invoke runner"]
        RUN["Runner<br/>mock · codex · claude"]
        API["Status HTTP surface<br/>GET /api/v1/state · POST /api/v1/refresh · /readyz"]
        WEB["Embedded web dashboard<br/>read-only operator UI"]
    end

    WS["Deterministic Git workspace<br/>clone · checkout · task files · artifacts"]
    WF["WORKFLOW.md<br/>config front-matter + prompt template"]
    AGENT["Agent session"]
    TUI["cmd/tui<br/>terminal dashboard"]

    trackers -->|"active issues"| ORCH
    ORCH -->|"claimed task"| WORK
    ORCH -->|"snapshot"| API
    API -->|"refresh request"| ORCH
    WORK --> WS
    WF -->|"resolved config + prompt"| WORK
    WORK --> RUN
    RUN --> AGENT
    API --> WEB
    API --> TUI
    AGENT -.->|"edits · verify · push · draft PR"| WS
    AGENT -.->|"label / state write-back"| trackers

    classDef ext fill:#eef,stroke:#88a;
    class LIN,GIT,GH ext;
```

The dashed edges are **agent-owned**: per SPEC §1 the worker never pushes,
opens PRs, or writes tracker state itself. Verification is appended to the
prompt as a directive the agent runs before handing off, not executed as a
worker phase.

The status HTTP listener exposes separate read and operator-action endpoints.
`GET /api/v1/state` and `GET /api/v1/:issue` are read-only snapshots over the
same in-memory orchestrator state. The embedded dashboard and `cmd/tui` consume
that JSON contract; they do not schedule work, mutate trackers, or participate
in agent handoff. `POST /api/v1/refresh` is the explicit operator action: it
queues an immediate poll/reconcile wake through `orchestrator.RequestRefresh`,
so it can dispatch eligible work, but it still does not write trackers or open
PRs.

This boundary is structural, not just convention. A class-hierarchy callgraph
(Go SSA + CHA) over the module finds no call path from any `worker` or
`orchestrator` function into the Gitea pull-request client — because CHA
over-approximates dynamic dispatch, the absence of an edge means the
irreversible steps are reachable only through the agent's tools in the current
tree. This is separate from package imports: `runner` imports `gitea` to build
the agent-visible tool proxy, while worker/orchestrator code does not call the
PR client directly. It is a property you can re-check, not a gate the CI
enforces today.

## Package layout

The `internal/` packages form an acyclic dependency graph. `buildinfo`,
`stateapi`, `task`, and `workflow` have no internal imports; `orchestrator` is
the coordinating package that wires the worker-facing runtime together behind
small, consumer-defined interfaces.

```mermaid
flowchart BT
    task["task<br/>domain types"]
    buildinfo["buildinfo<br/>binary version resolution"]
    stateapi["stateapi<br/>/api/v1/state wire DTOs"]
    workflow["workflow<br/>WORKFLOW.md load/resolve"]
    envpolicy["envpolicy<br/>child-process env allowlist"]
    tracker["tracker<br/>Linear / GitHub clients"]
    workspace["workspace<br/>deterministic git workspaces"]
    gitea["gitea<br/>REST client + label/state map"]
    runner["runner<br/>mock / codex / claude"]
    worker["worker<br/>RunTask pipeline"]
    doctor["doctor<br/>preflight diagnostics"]
    orchestrator["orchestrator<br/>actor + poller"]

    envpolicy --> workflow
    tracker --> workflow
    workspace --> envpolicy
    workspace --> task
    workspace --> workflow
    gitea --> tracker
    gitea --> workflow
    runner --> envpolicy
    runner --> task
    runner --> tracker
    runner --> workflow
    runner --> workspace
    runner --> gitea
    worker --> runner
    worker --> task
    worker --> tracker
    worker --> workflow
    worker --> workspace
    doctor --> gitea
    doctor --> runner
    doctor --> tracker
    doctor --> workflow
    orchestrator --> runner
    orchestrator --> task
    orchestrator --> tracker
    orchestrator --> worker
    orchestrator --> workflow
    orchestrator --> workspace
```

The command packages sit above this graph. `cmd/worker` maps
`orchestrator.StateView` into `stateapi.StateResponse` and serves the HTTP
surface; `cmd/tui` decodes the same `stateapi` DTOs. Both binaries use
`buildinfo` for the version string, while the scheduler core stays independent
of the dashboard renderer.

## Run-attempt lifecycle

Each claimed task moves through the SPEC §7.2 run-attempt phases. The terminal
phases feed back into the orchestrator's retry/backoff/continuation scheduling.
The diagram keeps terminal edges readable; setup and prompt-building errors are
also emitted as `Failed` from the phase in progress.

```mermaid
stateDiagram-v2
    [*] --> PreparingWorkspace
    PreparingWorkspace --> BuildingPrompt
    BuildingPrompt --> LaunchingAgentProcess
    LaunchingAgentProcess --> InitializingSession
    InitializingSession --> StreamingTurn
    StreamingTurn --> Finishing
    Finishing --> Succeeded
    StreamingTurn --> Failed
    StreamingTurn --> TimedOut
    StreamingTurn --> Stalled
    StreamingTurn --> CanceledByReconciliation
    Succeeded --> [*]
    Failed --> [*]
    TimedOut --> [*]
    Stalled --> [*]
    CanceledByReconciliation --> [*]
```

The internal `task.Status` enum is simpler: `queued → running → {succeeded |
failed}`. Runtime status endpoints expose richer observability buckets such as
`running`, `retrying`, `blocked`, `completed`, and the handoff /
reconcile-stopped rows described in the
[runtime-status runbook](runbooks/runtime-status.md).

## The orchestrator actor

All mutations of the orchestrator's in-memory state run on a single goroutine.
Callers never touch the state directly; they submit an operation onto a channel.
The op's `apply` runs on the actor goroutine and returns an optional `followup`
closure that runs on a fresh goroutine, so side effects (network calls, timers)
never block the actor loop. This serializes state access without a web of
mutexes.

```mermaid
sequenceDiagram
    participant C as Caller
    participant Ch as ops channel
    participant A as Actor goroutine
    participant S as OrchestratorState
    participant F as followup goroutine

    C->>Ch: submit(ctx, op)
    Note over C,Ch: returns ctx.Err() if the actor is shutting down
    Ch->>A: op
    A->>S: op.apply(state)
    S-->>A: followup func()
    A->>F: go followup()
    Note over F: network calls, timers,<br/>re-submitted ops
```

A small number of late-bound dependencies (the candidate lister, the terminal
resolver) are swapped under a mutex rather than through the actor, which is why
the design is a hybrid rather than a pure actor.
