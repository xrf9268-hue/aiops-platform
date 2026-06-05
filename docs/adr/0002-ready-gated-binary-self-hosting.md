# ADR 0002: Use ready-gated binary self-hosted development

## Status

Accepted

## Date

2026-06-05

## Context

`aiops-platform` now has three overlapping workflow surfaces: Trellis task
memory, project-specific Claude skills and runbooks, and the worker's tracker
polling runtime. GitHub issue mode also has less direct dogfood evidence than
the Linear-backed validation path, so treating every open GitHub issue as
eligible would make binary self-hosted development too easy to mis-schedule.

## Decision

Use Trellis only for task context and batch ledgers. Keep execution authority in
the tracker, repository runbooks, and PR protocol. For GitHub dogfood, use a
dedicated `aiops:ready` label as the unattended queue entry and do not include
`open` as an active state. Start binary self-hosted development only after a
disposable GitHub issue smoke proves that ready-gated dispatch, PR creation, and
follow-through behave as expected.

For other repositories, use external worker mode: `aiops-platform` runs outside
the target repo, while the target repo owns the minimum workflow contract,
readiness rules, and verification commands. Trellis is optional in those repos.

## Consequences

- GitHub dogfood is slower to start, but dependency handling is explicit and the
  worker will not sweep arbitrary open issues into unattended execution.
- Priority labels are triage metadata, not permission to run.
- Dependent issues stay out of the ready queue until blockers are terminal and
  the worker base has refreshed to the merged upstream state.
- Other projects can adopt the worker without copying this repository's full
  Trellis or Claude skill layout.
