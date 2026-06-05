# Dogfood development

This runbook describes how to use `aiops-platform` to develop
`aiops-platform` itself without letting local automation outrun the repository's
review and dependency rules.

Use this together with:

- [`personal-daily-workflow.md`](personal-daily-workflow.md) for the ordinary
  operator loop.
- [`github-local-automation.md`](github-local-automation.md) for the local
  GitHub worker and PR follow-through services.
- [`binary-deployment.md`](binary-deployment.md) for the plain binary and
  launchd path.
- [`batch-issue-processing.md`](batch-issue-processing.md) for issue batches.
- [`agentic-project-template.md`](agentic-project-template.md) for applying the
  same model to other repositories.

## Model

This runbook covers one high-control operating mode: using the worker binary to
schedule development of this repository. It is not the universal development
process for every change. `aiops-platform` is workflow-defined: `WORKFLOW.md`
chooses the tracker, ready gate, agent runner (`codex-app-server`, `claude`,
`mock`, or another supported runner), prompt contract, verification commands,
workspace, and PR handoff expectations.

Binary self-hosted development means an installed `worker` binary dispatches
work against the `aiops-platform` repository itself. It is not a new scheduler
mode. The same tracker, workflow config, deterministic workspace, agent prompt,
verification, PR body, review, and merge gates still apply.

Trellis and `grill-with-docs` sit before execution:

1. Create or update a Trellis task for the work.
2. Write the intended plan, scope, acceptance criteria, dependency class, and
   verification.
3. Use `grill-with-docs` to test the plan against `CONTEXT.md`, ADRs, and the
   relevant runbooks.
4. Only then move the tracker issue through the ready gate.

Runtime truth comes from `/api/v1/state`, worker logs, live tracker state, and
live PR/review-thread state. Trellis notes are planning memory, not dispatch
authority.

## Rollout stages

### 0. Direct agent-driven issue or PR work

Use the project skills directly:

- `.claude/skills/handle-issue/SKILL.md` for one GitHub issue to one draft PR.
- `.claude/skills/handle-pr/SKILL.md` plus the PR protocol for an existing PR.

This stage is the fallback whenever scheduler-managed automation is not the
right fit: requirements are unclear, dependencies are unresolved, a change is
too large, a sensitive path is involved, or you want a direct Claude Code/Codex
session instead of worker dispatch.

### 1. Disposable GitHub smoke

Before using GitHub issue mode on this repository, prove it in a disposable
repository. This smoke validates the GitHub ready-label process, not native
GitHub dependency filtering: the downstream issue stays idle because it does not
carry `aiops:ready`.

1. Configure a GitHub workflow that uses `aiops:ready` as the only active issue
   label.
2. Create two issues: one blocker and one downstream issue that depends on it.
3. Apply `aiops:ready` only to the blocker issue.
4. Run the worker with a safe runner first, then the intended real runner.
5. Confirm only the blocker issue dispatches and the downstream issue remains
   idle while it lacks `aiops:ready`.
6. Merge or otherwise close the blocker path, refresh the worker base, then
   apply `aiops:ready` to the downstream issue.
7. Confirm the agent opens or updates exactly one draft PR for the ready issue.

Do not proceed to current-repo dogfood until this smoke passes.

### 2. Binary service on this repository

Install the binary path from [`binary-deployment.md`](binary-deployment.md) or
the local GitHub LaunchAgent path from
[`github-local-automation.md`](github-local-automation.md).

For dogfood, the GitHub workflow must use:

```yaml
tracker:
  active_states:
    - aiops:ready
```

Do not include `open` in `active_states`. Priority labels such as
`priority:p1` are triage metadata only; they do not make an issue runnable.

Start with `AIOPS_AUTO_MERGE=0` for PR follow-through. Enable auto-merge only
for a named scope and only for small PRs.

### 3. Current-repo unattended dogfood

For each candidate issue:

1. Confirm the issue is small, has acceptance criteria, and names off-limits
   paths.
2. Classify dependencies as `hard dependency`, `soft overlap`, or
   `independent issue`.
3. Keep blocked or overlapping issues out of `aiops:ready`.
4. Apply `aiops:ready` only when the issue is safe for unattended dispatch.
5. Watch `/api/v1/state`, worker logs, the PR body ledger, CI, and review
   threads until the PR reaches a terminal handoff state.

If the agent reports an external dependency, remove `aiops:ready` or move the
tracker issue out of the active set. Do not interpret `/api/v1/state.blocked`
as the dependency backlog; it is local worker state.

### 4. Other repositories

Use external worker mode. The target repository owns its `WORKFLOW.md`, issue
readiness rules, prompt contract, and verification commands. It does not need
to install Trellis or copy this repository's `.claude/skills`.

Follow [`agentic-project-template.md`](agentic-project-template.md) before
running a real agent against another project.

## Dependency handling

Dependency handling is a dispatch gate, not a note-taking exercise:

- Hard dependencies are serialized. The downstream issue gets no ready gate
  until blocker PRs are merged and the worker base has refreshed.
- Soft overlap is serialized in unattended dogfood. Direct agent-driven parallel
  work is allowed only when the branch/base relationship is explicit in the
  ledger and PR body.
- Independent issues may run in parallel, one issue per branch and one PR per
  issue, within worker and review capacity.

Tracker rules:

- Linear: use native blocked-by and keep blocked issues in `Todo`.
- Gitea: write `Depends on #N` in the issue body.
- GitHub: do not apply `aiops:ready` to dependent issues until blockers are
  terminal.

The Trellis parent task should record:

```text
issue -> depends_on -> dependency_type -> ready_gate -> branch/worktree -> PR -> head -> state -> next action
```

## Small PR auto-merge

Auto-merge is never a standing grant. It must be authorized for a named scope.
Even then, it applies only to small PRs:

- within the review budget
- no off-limits paths
- configured verification green
- independent local reviews clean
- GitHub checks green
- review threads resolved
- PR body ledger current
- issue/PR explicitly authorized for the merge scope

If any condition is missing, leave the PR for human merge.

## Stop conditions

Stop unattended dogfood and switch to manual work when:

- a downstream issue needs an unmerged blocker
- the PR exceeds the small-change review budget
- the agent touches sensitive paths or ignores scope
- reviewer output is malformed, non-JSON, timed out, or blocking
- CI or review threads require design judgment
- the worker dispatches anything without the intended ready gate
