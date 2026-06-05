# Agent Workflow Guidelines

> Routing index and invariants for AI-assisted development in this repository.

This file does not replace the repository runbooks or Claude skills. It gives
Trellis tasks the minimum workflow context needed to route work to the right
source of truth.

---

## Primary Sources

| Work type | Source of truth |
| --- | --- |
| Daily operator loop | [`docs/runbooks/personal-daily-workflow.md`](../../../docs/runbooks/personal-daily-workflow.md) |
| Issue to PR work in this repo | [`.claude/skills/handle-issue/SKILL.md`](../../../.claude/skills/handle-issue/SKILL.md) |
| Existing PR follow-through | [`.claude/skills/handle-pr/SKILL.md`](../../../.claude/skills/handle-pr/SKILL.md) and [`docs/runbooks/pr-review-merge-protocol.md`](../../../docs/runbooks/pr-review-merge-protocol.md) |
| Fixed issue batches | [`docs/runbooks/batch-issue-processing.md`](../../../docs/runbooks/batch-issue-processing.md) |
| Binary self-hosted development | [`docs/runbooks/dogfood-development.md`](../../../docs/runbooks/dogfood-development.md) |
| Other repositories | [`docs/runbooks/agentic-project-template.md`](../../../docs/runbooks/agentic-project-template.md) |
| Runtime status interpretation | [`docs/runbooks/runtime-status.md`](../../../docs/runbooks/runtime-status.md) |

---

## Trellis Role

Use Trellis for planning memory, task context, and batch ledgers:

- Create a parent Trellis task for a multi-issue batch or dogfood rollout.
- Create one child Trellis task per tracker issue.
- Record issue dependencies, branch/PR state, and next action in the parent
  task.
- Keep implementation instructions in the tracker issue and PR body, not only in
  Trellis.

Trellis is not a scheduler lock and is not a replacement for tracker state.
The worker dispatches from the configured tracker and the runbook-defined ready
gate.

The selected `WORKFLOW.md` defines the worker's operating mode. Dogfood is one
mode, not a global rule that every future AI-assisted development session must
use.

---

## Pre-Implementation Gate

Before changing code or workflow behavior:

1. Open or update the relevant Trellis task.
2. Write a short plan with goal, scope, acceptance criteria, dependencies, and
   verification.
3. Run a `grill-with-docs` review of the plan against `CONTEXT.md`, ADRs, and
   the relevant runbooks.
4. Capture settled terminology in `CONTEXT.md` only when it is domain language,
   and capture durable trade-off decisions in `docs/adr/` only when they meet
   the ADR bar.
5. Before pre-push review, follow
   [`docs/runbooks/pr-review-merge-protocol.md`](../../../docs/runbooks/pr-review-merge-protocol.md)
   as the single source of truth. Do not restate reviewer-routing mechanics in
   Trellis specs or task notes.

For small documentation-only edits, the Trellis task may be lightweight, but the
source-of-truth runbook still wins over task notes.

---

## Issue Dependency Invariants

Classify every batch issue before dispatch:

- `hard dependency`: downstream work needs an upstream merge, API, schema,
  migration, branch base, or atomic refactor. Serialize it.
- `soft overlap`: shared files, package surface, generated artifacts, or
  dependency manifests create review/merge risk. Serialize in unattended mode.
- `independent issue`: no branch, contract, path, or review dependency. May run
  in parallel within worker and review capacity.

Tracker-specific gates:

- Linear: use native blocked-by relationships and keep blocked issues in `Todo`
  until blockers are terminal.
- Gitea: express issue dependencies with `Depends on #N`, but keep dependent
  issues in `Todo` or out of active labels until blockers are terminal.
- GitHub: use `aiops:ready` as the unattended queue label. Do not use `open` as
  an active state for dogfood work.

`/api/v1/state.blocked` is not a dependency backlog. It reports local blocked
claims such as input-required or continuation-budget stops.

---

## Non-Goals

- Do not duplicate the `handle-issue`, `handle-pr`, batch, or PR merge protocol
  inside Trellis specs.
- Do not use Trellis parent/child links as evidence that the worker will avoid
  dispatching a blocked issue.
- Do not treat priority labels as readiness. Priority is human triage metadata;
  it does not grant permission to run work.
