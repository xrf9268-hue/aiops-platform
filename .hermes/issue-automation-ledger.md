# AIOps Platform Issue Automation Ledger

Updated: 2026-05-16T10:15:00+00:00

## Operating policy
- Global goal: close all open issues in xrf9268-hue/aiops-platform with high-quality implementation.
- Order: P0 first, then P1, then P2.
- Quality gate for every implementation issue: fresh main, isolated branch/worktree, local gofmt/go test ./..., PR, independent review, @codex review completion (no 👀), GitHub Actions green, delayed recheck, squash merge.
- GitHub Claude review is not configured for this repo; do not trigger or wait for @claude review as a quality gate.
- Do not stop after a single PR unless blocked by an external dependency; update this ledger after every issue/PR state transition.

## Current work
- #87 merged via PR #105; #68 prerequisite is unblocked.
- #68 branch `feat/issue-68-startup-reconcile` is in progress locally.
- This tick added/verified startup reconciliation implementation and tests; local `/home/pi/.local/go1.25.10/bin/go test ./...` passes.
- Next: inspect final diff, create commit, push branch, open PR for #68, trigger @codex review, then follow CI/review gates to merge.

## Open issue queue
### P0
- [ ] #13 [M3][P0] Validate Linear poller against real Linear workspace (area:linear, milestone:m3, priority:p0, type:validation)
- [ ] #14 [M3][P0] Add Linear status transitions after task handoff (area:linear, milestone:m3, priority:p0, type:feature)
- [~] #68 [M5][P0][spec-alignment] D3 Reconcile in-flight workspaces with tracker on worker startup — IN PROGRESS; implementation local, tests green, PR pending
### P1
- [ ] #15 [M3][P1] Add Linear workpad comments (area:linear, milestone:m3, priority:p1, type:feature)
- [ ] #16 [M3][P1] Add repo routing for Linear issues (area:linear, milestone:m3, priority:p1, type:feature)
- [ ] #19 [M4][P1] Add Claude analysis-only runner mode (area:runner, milestone:m4, priority:p1, type:feature)
- [ ] #26 [M6][P1] Add lightweight task CLI or dashboard (area:observability, milestone:m6, priority:p1, type:feature)
- [ ] #64 [M5][P1][spec-alignment] D1 Adopt Codex app-server protocol per SPEC §Agent Runner (area:runner, milestone:m5, priority:p1, type:feature, area:spec-alignment)
- [ ] #67 [meta][spec-alignment] SPEC.md deviations tracker (priority:p1, type:docs, area:spec-alignment)
- [ ] #70 [security][P1][spec-alignment] D5 Document and harden sandbox posture per SPEC §safety (priority:p1, type:feature, area:spec-alignment, area:security)
- [ ] #71 [M5][P1][spec-alignment] Adopt aiops/* label state machine for Gitea tracker (area:gitea, milestone:m5, priority:p1, type:feature, area:spec-alignment)
- [ ] #73 [P1][spec-alignment] Replace Postgres queue with SPEC's tracker+filesystem recovery model (priority:p1, area:spec-alignment, area:queue, type:refactor)
- [ ] #74 [P1][spec-alignment] Replace Gitea webhook trigger with Gitea poller (mirror cmd/linear-poller) (area:gitea, priority:p1, area:spec-alignment, type:refactor)
- [ ] #78 [P1][spec-alignment] Per-tick reconciliation: stop active runs when tracker state changes (SPEC §2.1) (area:linear, area:workspace, priority:p1, type:feature, area:spec-alignment)
- [ ] #84 [P1][spec-alignment] D10 Workflow file is per-service, not per-repo-per-task (SPEC §5.1) (area:workflow, priority:p1, area:spec-alignment, type:refactor)
- [ ] #85 [P1][spec-alignment] D11 Dynamic WORKFLOW.md watch/reload (SPEC §6.2) (area:workflow, priority:p1, type:feature, area:spec-alignment)
- [ ] #86 [P1][spec-alignment] D12 Workspace lifecycle hooks (after_create/before_run/after_run/before_remove) (SPEC §5.3.4, §9.4, §18.1) (area:workspace, priority:p1, type:feature, area:spec-alignment)
- [x] #87 [P1][spec-alignment] D13 Workspace key by sanitized issue identifier, not task ID (SPEC §4.2, §9.1) — merged in PR #105
- [ ] #90 [P1][spec-alignment] D16 Exponential backoff and continuation retries (SPEC §7.3, §8.4) (priority:p1, type:feature, area:spec-alignment, area:queue)
- [ ] #93 [P1][spec-alignment] D19 Candidate selection, Todo blocker rule, dispatch sort (SPEC §8.2) (area:linear, priority:p1, type:feature, area:spec-alignment)
- [ ] #94 [P1][spec-alignment] D20 Linear GraphQL: project.slugId filter, pagination, state-refresh query (SPEC §11.2) (area:linear, priority:p1, type:feature, area:spec-alignment)
- [ ] #95 [P1][spec-alignment] D21 Single-source-of-truth orchestrator state (SPEC §3.1, §4.1.8, §7.4) (priority:p1, area:spec-alignment, area:queue, type:refactor)
### P2
- [ ] #72 [P2][spec-alignment] Revert WORKFLOW.md discovery to single-source per SPEC §workflow file (area:workflow, priority:p2, area:spec-alignment, type:cleanup)
- [ ] #88 [P2][spec-alignment] D14 Stall detection + turn/read/stall timeouts (SPEC §5.3.6, §8.5) (area:runner, type:feature, priority:p2, area:spec-alignment)
- [ ] #89 [P2][spec-alignment] D15 Per-state concurrency limits (max_concurrent_agents_by_state) (SPEC §5.3.5, §8.3) (type:feature, priority:p2, area:spec-alignment, area:queue)
- [ ] #91 [P2][spec-alignment] D17 Liquid-strict template rendering + attempt variable (SPEC §5.4, §12.2) (area:workflow, type:feature, priority:p2, area:spec-alignment)
- [ ] #92 [P2][spec-alignment] D18 Issue domain model missing priority/labels/blocked_by/branch_name/timestamps (SPEC §4.1.1) (area:linear, type:feature, priority:p2, area:spec-alignment)
- [ ] #96 [P2][spec-alignment] D22 Run attempt phases and runtime events vocabulary (SPEC §7.2, §10.4) (area:runner, type:feature, priority:p2, area:spec-alignment)
- [ ] #97 [P2][spec-alignment] D23 linear_graphql client-side tool advertisement (SPEC §10.5) (area:linear, area:runner, type:feature, priority:p2, area:spec-alignment)
- [ ] #98 [P2][spec-alignment] D24 Workflow front-matter schema deviations (SPEC §5.3) (area:workflow, priority:p2, area:spec-alignment, type:refactor)
