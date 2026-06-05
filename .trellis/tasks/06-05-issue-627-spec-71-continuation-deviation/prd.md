# Issue #627: Re-audit SPEC §7.1 Continuation Hardening

## Goal

Close GitHub issue #627 with a focused PR that records the lesson from #621/#628: upstream/SPEC alignment remains the default bar, but validated upstream under-hardening can justify a narrow, tracked harness-hardening deviation.

## What I Already Know

- Issue #627 is open with labels `type:hardening`, `priority:p2`, and `area:spec-alignment`.
- GitHub dependency fields are clear: `blockedBy=0`, `blocking=0`, no parent, no sub-issues, and no tracked issue relationships.
- Root `WORKFLOW.md` is absent on `main`; the only workflow file in this repo checkout is `examples/WORKFLOW.md`.
- PR #628 already implemented #621's `agent.max_continuation_turns` D34 clean-turn budget. This task should not reimplement that code path.
- `DEVIATIONS.md` already has D34, but it cites PR #625 as the evidence PR and does not mention issue #627 or PR #628 as the implementation/lesson closeout.
- `AGENTS.md` principle 6 still says upstream absence is a strong over-design signal without the #621/#627 under-hardened-upstream exception.

## Requirements

- Amend `AGENTS.md` principle 6 so the rule remains strict by default but explicitly covers the #621/#627 exception: upstream absence can mean upstream is under-hardened only after live evidence proves SPEC/upstream behavior is operationally defective.
- Preserve principle 6's delete-don't-relocate default for wrong-side-of-§1 worker/orchestrator gates.
- Update `DEVIATIONS.md` ledger wording so D30/D34 accurately distinguish #625 upstream evidence from PR #628 implementation, and link #627 as the governance/process lesson.
- Keep the change documentation-only unless code inspection reveals a missing #628 implementation invariant.

## Acceptance Criteria

- [x] `AGENTS.md` names the under-hardened-upstream exception and its required evidence bar.
- [x] `DEVIATIONS.md` D34 cites #621, #627, PR #625 evidence, and PR #628 implementation accurately.
- [x] No duplicate D34 implementation or new worker/orchestrator phase/gate is introduced.
- [x] Local gates required for the final PR are run or explicitly reported if unavailable.
- [ ] Commit, pre-push Codex + Claude dual review, push, PR creation, post-push `@codex review`, GraphQL reviewThreads, CI, warning audit, and PR body ledger follow the repo protocol.

## Out of Scope

- Reworking the #628 `agent.max_continuation_turns` implementation.
- Closing or merging the PR without explicit user permission.
- Opening a batch parent Trellis task; #627 has no hard dependency relationship.

## Technical Approach

1. Record the live issue/upstream research in `research/spec-upstream-continuation-budget.md`.
2. Run `grill-with-docs` against `CONTEXT.md`, `AGENTS.md`, `DEVIATIONS.md`, and the dogfood/PR runbooks.
3. Update only the governance/ledger docs needed to close #627.
4. Verify with documentation-appropriate local gates plus the repository's required handle-issue gates unless infeasible.

## Definition of Done

- #627 has one focused PR, not merged.
- PR body is a live ledger with acceptance criteria, verification, mutation/regression rationale, dual-review verdicts, post-push review state, CI, warning audit, and size-gate classification.
- Final handoff is merge-ready or a concrete blocker.

## Verification Notes

- Documentation-only change; no production code path changed, so mutation testing is not applicable.
- Regression check is ledger/source consistency: live #621/#627/#625/#628 context, upstream SPEC/Elixir continuation behavior, `AGENTS.md` principle 6 wording, and `DEVIATIONS.md` D30/D34 references all agree.
- Local gates run: `gofmt -l $(git ls-files '*.go')`, `go mod tidy`, `git diff --exit-code -- go.mod go.sum`, `go vet ./...`, `go test -race -covermode=atomic ./...`, `go build ./cmd/worker ./cmd/tui`, `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run --config=.golangci.yml --issues-exit-code=0`, `git diff --check`.
- First full race run failed because local ignored dashboard dist assets were stale; rebuilding ignored `cmd/worker/dashboard/dist` with `npm run build` made the focused dashboard test and full race suite pass. The rebuilt assets are ignored and not part of the PR.
