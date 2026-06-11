# Issue #750: Dispatch revalidation does not re-check Todo blockers (upstream retry_candidate_issue? does)

(PRD = issue body)

## Gap (found during #749 review)

PR #749 ports upstream's dispatch-time candidate revalidation (`revalidate_issue_for_dispatch`, `elixir/lib/symphony_elixir/orchestrator.ex:909-924`, `:995-1013`). Upstream's acceptance predicate is `retry_candidate_issue?` = `candidate_issue?` **AND** `!todo_issue_blocked_by_non_terminal?` (`elixir/lib/symphony_elixir/orchestrator.ex:1602-1604`) — i.e. it re-checks the Todo blocker gate against the **refreshed** issue, because Linear's `fetch_issue_states_by_ids` returns fully normalized issues including `blocked_by`.

aiops-platform's narrow refresh contract (`tracker.IssueState`) carries only `State` + `Labels`, so `revalidatedCandidate` (`internal/orchestrator/poller_candidates.go`) re-applies the active-state and required-labels gates on refreshed data but keeps the **listing-time** blocker verdict. A Todo issue whose blocker flips from terminal to non-terminal inside the tick's staleness window still dispatches.

## Bounded impact

- The window is one tick's reconcile wait (the same window #749 closed for state/labels).
- The blocker gate still applies at listing time every tick, and the retry-fire path's `eligibleActiveIssueLister` re-applies it at fire time (it re-lists full issues, not the narrow shape).
- The Gitea blocker mapping is independently broken (lifecycle-test F1), so on Gitea this gate currently has no effect regardless.

## Why deferred from #749

Closing it needs `BlockedBy` in the narrow-refresh contract: `tracker.IssueState` + all three adapters (Linear GraphQL selection, GitHub, Gitea) + fakes — a separate mechanism change beyond #749's already size-gated diff.

## Acceptance criteria

- [ ] The narrow refresh surface exposes blocker data (extend `tracker.IssueState` with `BlockedBy`, or an equivalent SPEC-aligned read) for Linear at minimum; adapters that cannot supply it document the gap.
- [ ] `revalidatedCandidate` re-applies `todoIssueBlockedByOpenDependency` on refreshed blocker data, matching upstream `retry_candidate_issue?` (`orchestrator.ex:1602-1604`).
- [ ] Regression test: a Todo candidate whose blocker reopens between listing and dispatch is not dispatched; mutation-verified per clean-code rule 6.

Umbrella: #67.
