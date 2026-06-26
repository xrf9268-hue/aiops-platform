# Merge Mechanism Retrospective Note

This GitHub E2E deliberately uses a single worker with agent-side merge:

- Worker role: poll GitHub issues labeled `aiops:ready`, prepare the workspace,
  run `codex app-server`, reconcile cancellation/terminal state, and expose
  dashboard state.
- Agent role: edit code, run local verification, push the branch, create the PR,
  wait for GitHub Actions, and run `gh pr merge --squash --delete-branch`.

This differs from the Gitea maker/reviewer examples:

- `examples/maker-WORKFLOW.md` says the maker opens a PR and hands off to
  `Human Review`; it explicitly says not to review or merge its own work.
- `examples/reviewer-WORKFLOW.md` is review-only; it comments a verdict and
  flips labels, but does not land code.
- `examples/reviewer-automerge-WORKFLOW.md` uses a separate reviewer bot. On
  PASS, that reviewer approves and enables CI-gated forge auto-merge, then
  confirms `merged:true` before setting `aiops/done` and closing the issue.

Why this run is still SPEC-aligned:

- The worker did not call GitHub merge APIs or mutate PRs directly.
- The merge was requested by the coding agent through runtime GitHub tooling,
  which matches the project boundary: worker as scheduler/runner/tracker reader;
  agent as ticket/PR writer.

Evidence to include in the final report:

- `logs/worker.log` shows `runner_start`, `runner_stopped`, and reconciliation
  events, not worker-owned merge calls.
- `workspaces/.../github_issue/_1/.aiops/CODEX_APP_SERVER_OUTPUT.txt` records
  the agent opening PR #15 and entering PR follow-through.
- `gh pr view 15` reports `mergedBy.login == "zjlgdx"` and merge commit
  `1ab9bdd3693cd02c3053bae491d7c7002383d33a`.
