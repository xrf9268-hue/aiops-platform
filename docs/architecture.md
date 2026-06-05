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

    WS["Deterministic Git workspace<br/>clone · checkout · verify · policy checks"]
    WF["WORKFLOW.md<br/>policy + prompt template"]
    AGENT["Agent session"]

    trackers -->|"active issues"| ORCH
    ORCH -->|"claimed task"| WORK
    WORK --> WS
    WF -->|"resolved config + prompt"| WORK
    WORK --> RUN
    RUN --> AGENT
    AGENT -->|"edits · verify · push · draft PR"| WS
    AGENT -.->|"label / state write-back"| trackers

    classDef ext fill:#eef,stroke:#88a;
    class LIN,GIT,GH ext;
```

The dashed edges are **agent-owned**: per SPEC §1 the worker never pushes,
opens PRs, or writes tracker state itself. Verification is appended to the
prompt as a directive the agent runs before handing off, not executed as a
worker phase.

## Package layout

The `internal/` packages form an acyclic dependency graph. `task` and
`workflow` are dependency-free leaves; `orchestrator` sits at the top and wires
everything together behind small, consumer-defined interfaces.

```mermaid
flowchart BT
    task["task<br/>domain types"]
    workflow["workflow<br/>WORKFLOW.md load/resolve"]
    tracker["tracker<br/>Linear / GitHub / Gitea clients"]
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

```mermaid
stateDiagram-v2
    [*] --> preparing_workspace
    preparing_workspace --> building_prompt
    building_prompt --> launching_agent_process
    launching_agent_process --> initializing_session
    initializing_session --> streaming_turn
    streaming_turn --> finishing
    finishing --> succeeded
    streaming_turn --> failed
    streaming_turn --> timed_out
    streaming_turn --> stalled
    streaming_turn --> canceled_by_reconciliation
    succeeded --> [*]
    failed --> [*]
    timed_out --> [*]
    stalled --> [*]
    canceled_by_reconciliation --> [*]
```

The coarse task status reported externally is simpler: `queued → running →
{succeeded | failed}`.

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
