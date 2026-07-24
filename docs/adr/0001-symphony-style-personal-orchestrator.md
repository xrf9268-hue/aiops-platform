# ADR 0001: Adopt a Symphony-style personal orchestrator

## Status

Accepted

## Date

2026-04-28

> [!NOTE]
> This ADR records the architectural decision and its rationale. See
> the [architecture overview](../architecture.md) for the current runtime and
> the [deviation ledger](../../DEVIATIONS.md) for current alignment status.

## Context

The project started as a Gitea-first automation pipeline:

```text
Gitea issue comment
  -> trigger API
  -> Postgres queue
  -> worker
  -> branch
  -> pull request
```

That early pipeline is historical. Current runtime behavior is tracker polling
owned by `cmd/worker`, with scheduler state held in memory and branch/PR
handoff performed by the agent.

The original enterprise-oriented design included many concerns such as team governance, RBAC, audit, multi-repo policy, and enterprise deployment.

The product goal has since been narrowed:

- primary target is personal productivity
- the user has both personal projects and company projects
- Linear can be used as a task board
- Codex and Claude Code are available
- Gitea integration is still useful
- the system should stay locally hackable and easy to extend

OpenAI Symphony provides a useful reference model for this kind of workflow: issue tracker tasks become the control surface for coding agents, each task gets a workspace, the repo owns its workflow contract, and the agent produces a reviewable handoff.

## Decision

Adopt a Symphony-style architecture while keeping `aiops-platform` as a small Go implementation focused on personal productivity.

Use two tracks:

1. Run OpenAI Symphony directly for fast Linear plus Codex experiments.
2. Continue evolving `aiops-platform` as a local Go orchestrator for Gitea, Linear, custom policy, and runner experiments.

The core loop for `aiops-platform` is:

```text
Linear or Gitea task
  -> worker poll tick
  -> in-memory orchestrator state
  -> deterministic workspace
  -> WORKFLOW.md
  -> agent runner
  -> agent-owned verification + pull request handoff
```

## Consequences

### Positive

- Faster path to personal productivity.
- Keeps the system simple enough to run locally.
- Allows direct experimentation with OpenAI Symphony without blocking the custom Go implementation.
- Preserves Gitea integration for personal and company repositories.
- Makes repo-specific behavior explicit through `WORKFLOW.md`.
- Creates a clean path to add Codex, Claude Code, and other runners.

### Negative

- The project is not yet a full enterprise platform.
- There is some conceptual overlap with OpenAI Symphony.
- Linear and Gitea paths may diverge unless adapter boundaries stay clean.
- More safety and reconciliation work is needed before running against important company code.

## Safety posture

Default operation should be conservative:

- start with the mock runner
- use draft pull requests
- keep human review in the loop
- state sensitive paths as prompt scope and enforce landing policy through
  repository permissions, branch protection, review, and CI
- do not automatically merge
- use planning-only workflows for unclear or high-risk tasks

## Alternatives considered

### Use only OpenAI Symphony

Rejected as the only path because Gitea and custom local behavior remain important. Symphony is still useful as a reference and a direct productivity tool.

### Continue only with custom Gitea platform

Rejected as the only path because it delays real productivity. Linear plus Symphony-style issue orchestration is a faster way to validate the workflow.

### Build a full enterprise platform first

Rejected for now. The current target is personal productivity, not a multi-team internal developer platform.
