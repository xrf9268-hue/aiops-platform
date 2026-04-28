# Research: Symphony-style personal productivity

This note records the product and architecture direction for using Symphony-style coding automation for personal productivity.

## Core conclusion

For personal productivity, the fastest useful path is not to build a full enterprise platform first.

Use two tracks:

1. Use OpenAI Symphony directly with Linear and Codex to validate the daily workflow.
2. Continue evolving `aiops-platform` as a smaller Go implementation that can integrate Gitea, Linear, local policy, and custom runners.

## Why Symphony matters

Symphony changes the unit of work from a prompt to an issue.

The practical model is:

```text
Issue tracker task
  -> isolated workspace
  -> coding agent run
  -> verification
  -> review handoff
```

For personal work, this means the human focuses on:

- writing clear issues
- choosing which tasks are ready for automation
- reviewing pull requests
- deciding when to rework or merge

The tool focuses on:

- polling ready tasks
- preparing workspace state
- running the coding agent
- preserving logs and summaries
- creating a reviewable handoff

## Recommended personal workflow

Use Linear states:

```text
Backlog
AI Ready
In Progress
Human Review
Rework
Done
```

Only allow automation for:

```text
AI Ready
In Progress
Rework
```

Do not let automation consume every backlog item automatically.

## Task classes

Good first tasks:

- small bug fixes
- tests
- docs
- scripts
- low-risk cleanup
- small dependency updates

Use planning-only tasks for:

- architecture changes
- unclear requirements
- company core modules
- security-sensitive work
- data migration work

## aiops-platform role

`aiops-platform` should not try to clone every Symphony feature immediately.

It should focus on:

- Gitea first workflow
- Linear optional workflow
- repo-owned `WORKFLOW.md`
- local deterministic workspaces
- simple runner abstraction
- safe draft PR handoff
- personal customization

## Boundary with OpenAI Symphony

Use OpenAI Symphony directly when:

- working with Linear and Codex
- exploring task orchestration behavior
- validating whether agent-driven issue work improves daily flow

Use `aiops-platform` when:

- you need Gitea integration
- you want Go code you can modify quickly
- you need custom local policies
- you want to integrate Claude Code later
- you want a minimal personal orchestrator instead of a larger framework

## Near-term roadmap

### Phase 1: Mock loop

Goal:

```text
Linear or Gitea task -> queue -> worker -> workspace -> mock change -> PR
```

Success condition:

- task appears in DB
- worker creates branch
- worker creates PR
- human can review result

### Phase 2: Codex runner

Goal:

```text
Linear task -> Codex -> verification -> draft PR
```

Success condition:

- Codex can complete small tasks
- verification commands run
- changed paths are checked
- draft PR is created

### Phase 3: Personal daily use

Goal:

Use Linear as the task board for personal projects.

Success condition:

- daily small tasks can be moved to AI Ready
- agent produces reviewable PRs
- failed tasks produce useful summaries

### Phase 4: Company cautious mode

Goal:

Use the system for low-risk company tasks.

Success condition:

- draft PR only
- deny paths enabled
- human review required
- no automatic merge

## Implementation priorities

Next implementation items:

1. Move from direct `main` updates to pull request based development.
2. Add Linear status updates after task completion.
3. Add PR labels and reviewers.
4. Add `RUN_SUMMARY.md` enforcement.
5. Add better diff stats and policy checks.
6. Add Codex runner validation with a personal demo repo.
7. Add optional Claude runner for analysis-only tasks.

## Non-goals for now

Do not prioritize:

- multi-tenant admin UI
- Kubernetes deployment
- complex distributed scheduling
- enterprise RBAC
- automatic merge
- dashboard polish

Those are useful later, but not required for personal productivity.
