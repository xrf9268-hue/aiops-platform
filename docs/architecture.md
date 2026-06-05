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
    end

    WS["Deterministic Git workspace<br/>clone · checkout · task files · artifacts"]
    WF["WORKFLOW.md<br/>config front-matter + prompt template"]
    AGENT["Agent session"]

    trackers -->|"active issues"| ORCH
    ORCH -->|"claimed task"| WORK
    WORK --> WS
    WF -->|"resolved config + prompt"| WORK
    WORK --> RUN
    RUN --> AGENT
    AGENT -.->|"edits · verify · push · draft PR"| WS
    AGENT -.->|"label / state write-back"| trackers

    classDef ext fill:#eef,stroke:#88a;
    class LIN,GIT,GH ext;
```

The dashed edges are **agent-owned**: per SPEC §1 the worker never pushes,
opens PRs, or writes tracker state itself. Verification is appended to the
prompt as a directive the agent runs before handing off, not executed as a
worker phase.

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

The `internal/` packages form an acyclic dependency graph. `task` and
`workflow` are dependency-free leaves; `orchestrator` is the coordinating
package that wires everything together behind small, consumer-defined
interfaces.

```mermaid
flowchart BT
    task["task<br/>domain types"]
    workflow["workflow<br/>WORKFLOW.md load/resolve"]
    tracker["tracker<br/>Linear / GitHub clients"]
    workspace["workspace<br/>deterministic git workspaces"]
    gitea["gitea<br/>REST client + label/state map"]
    runner["runner<br/>mock / codex / claude"]
    worker["worker<br/>RunTask pipeline"]
    doctor["doctor<br/>preflight diagnostics"]
    orchestrator["orchestrator<br/>actor + poller"]

    tracker --> workflow
    workspace --> task
    workspace --> workflow
    gitea --> tracker
    gitea --> workflow
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
