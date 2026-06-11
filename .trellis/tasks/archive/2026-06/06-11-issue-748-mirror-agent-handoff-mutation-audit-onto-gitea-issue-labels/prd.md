# Issue #748: Gitea agent handoff is invisible to the handoff counter taxonomy: gitea_issue_labels emits no mutation audit

(Trellis PRD = issue body; autonomous batch run, ACs are definition-of-done)

## Observed (v0.1.0 packaged binary, live lifecycle test 2026-06-11; #740 F7)

After 12 successful agent handoffs (PR opened + `aiops/human-review` label flip each), the state API showed `completed_total=0` and `agent_handoff_reconcile_stopped_total=0` — every run was classified as a plain `reconcile_stopped_with_progress`. A dashboard reader sees "zero completions, zero handoffs" on a fully successful run.

## Root cause (investigated under #740)

The agent-handoff classification pipeline exists only for Linear tools:

- `internal/runner/gitea_tools.go` — the `gitea_issue_labels` proxy fires **no mutation-audit sink at all**: no `task.EventToolCallMutation` runtime event, no current-issue handoff classification. The Linear path (`linear_graphql` / `linear_ai_workpad`) classifies current-issue non-active/terminal state updates in `linearGraphQLProxy.dispatch` (`internal/runner/tools.go:595-605`) and emits the audit via `withMutationAuditSink` (`internal/runner/codex_app_server_approval.go:292-301`).
- `internal/orchestrator/runtime_events.go:86-100` — `recordAgentHandoffFields` only accepts `isLinearMutationTool` (`linear_graphql`, `linear_ai_workpad`), so `RunningEntry.AgentCurrentIssueHandoff` is never set on a Gitea run and `finalizeRunOp` (`actor_finalize.go:218-220, 258-263`) never records `agent_handoff_reconcile_stopped`.

`completed_total=0` follows from the Gitea handoff flow's designed terminal form (label flips to an inactive state → reconcile stops the streaming tail → `FinishRunReconciledCancelled`, never a clean `Err==nil` exit), so the handoff counter is the piece that must carry the signal.

This is AGENTS.md cross-cutting checklist item 1: an aiops extension (agent-handoff classification) applied on the Linear tool path but not on the sibling Gitea tool path.

## Acceptance criteria

- [ ] A successful `gitea_issue_labels` mutation that moves the **current issue** out of the configured active states emits `task.EventToolCallMutation` with the same classification payload keys the Linear path uses (`current_issue_non_active_state_update`, and `current_issue_terminal_state_update` + `current_issue_terminal_state` when the target state is terminal) — one source of truth with the Linear payload shape (clean-code rule 3).
- [ ] `recordAgentHandoffFields` recognizes the Gitea tool's mutation events so `RunningEntry.AgentCurrentIssueHandoff` is set.
- [ ] A Gitea run reconcile-stopped after the agent's handoff label flip increments `agent_handoff_reconcile_stopped_total` (regression test driving the finalize path, mutation-verified per clean-code rule 6/11).
- [ ] Non-handoff label mutations (e.g. flip to another active state) do not set the classification.

## References

- #740 (related observation folded into its investigation; dispatch-revalidation fix landed separately)
- Test report: `.trellis/tasks/archive/2026-06/06-11-web-todo-app-full-lifecycle-test-with-packaged-release-binary/report.md` (F7)
