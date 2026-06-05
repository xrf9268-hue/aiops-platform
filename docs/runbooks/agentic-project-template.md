# Agentic project template

This template is for using `aiops-platform` as an external worker for another
repository. The goal is to give the worker enough project-owned context to
dispatch safe, reviewable tasks without copying this repository's full local
tooling setup.

## Minimum repository contract

A target repository needs:

- `WORKFLOW.md` with repo clone URL, tracker kind, active states or labels,
  terminal states, agent runner, workspace root, and verification commands.
- A prompt body that tells the agent how to open or update one draft PR for one
  tracker issue.
- A ready gate that means "safe for unattended dispatch".
- Issue text with goal, acceptance criteria, scope hints, off-limits paths, and
  dependency notes.
- A PR handoff convention: explicit issue claim, verification commands, risk,
  and any deferrals in the PR body.

Optional but useful:

- `CONTEXT.md` for project-specific domain language.
- `docs/adr/` for durable architecture decisions.
- A short project runbook for local gates and release-specific caveats.
- Trellis tasks for planning memory and batch ledgers.

## Ready gates by tracker

Use one clear ready signal per tracker:

| Tracker | Recommended ready gate |
| --- | --- |
| Linear | A dedicated active state such as `AI Ready`, with native blocked-by relations for dependencies |
| Gitea | A dedicated active label/state plus `Depends on #N` for dependencies |
| GitHub | A dedicated `aiops:ready` label; do not use `open` as an unattended active state |

Priority is not readiness. Treat priority as human triage metadata unless the
target workflow explicitly implements priority-aware dispatch.

## Issue dependency policy

Classify candidate issues before applying the ready gate:

- `hard dependency`: serialize until blocker PRs merge and the worker base is
  refreshed.
- `soft overlap`: serialize in unattended mode; use manual branch chaining only
  when the dependency is explicit in the ledger and PR body.
- `independent issue`: may run in parallel if review bandwidth and worker
  capacity allow it.

If the target tracker cannot enforce dependencies natively, keep blocked issues
out of the ready gate. Do not rely on the agent to notice dependency notes after
dispatch.

## Minimal issue shape

```markdown
## Goal
<one sentence>

## Acceptance criteria
- ...

## Scope hints
- touch: <paths>
- do not touch: <paths>

## Dependencies
- blocks: <issue/pr or none>
- blocked by: <issue/pr or none>

## Verification
- <commands or expected checks>
```

## Minimal PR handoff

Each agent-created PR should include:

- `Closes #N` or the tracker-specific issue claim.
- Summary of the change.
- Acceptance criteria checklist.
- Verification commands and results.
- Dependency or deferral notes.
- Review-size classification.
- Current head SHA when a follow-through automation uses it as a gate.

## Worker setup order

1. Run with `agent.default: mock` to prove tracker polling and workspace
   creation.
2. Run a disposable issue with the intended real runner.
3. Add the ready gate to exactly one low-risk issue.
4. Watch `/api/v1/state`, logs, tracker state, and PR state.
5. Increase concurrency only after the operator can keep PRs reviewed and green.

## What not to copy

- Do not copy `aiops-platform`'s `.claude/skills` unless the target repository
  has the same SPEC-alignment workflow.
- Do not require Trellis in the target repository. Use it when it helps planning
  memory; otherwise keep the contract in `WORKFLOW.md`, issues, PRs, and
  runbooks.
- Do not use `open` issues as an unattended queue.
- Do not enable standing auto-merge.
