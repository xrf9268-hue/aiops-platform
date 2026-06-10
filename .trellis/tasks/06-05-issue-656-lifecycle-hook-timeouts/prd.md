# Issue #656: timeout Trellis lifecycle hooks

## Goal

Close GitHub issue #656 by adding a bounded timeout to Trellis lifecycle hook
commands so `task.py` cannot hang forever after task files have already been
written or moved.

## Live Issue Facts

- Issue: #656, `Follow up unresolved review thread from PR #650`
- Labels: `priority:p2`, `type:chore`
- State: closed by PR #664
- Originating PR: #650, merged at `2026-06-05T04:04:13Z`
- Review thread: `https://github.com/xrf9268-hue/aiops-platform/pull/650#discussion_r3360287673`
- Location: `.trellis/scripts/common/task_utils.py:242`
- Thread state from GraphQL: unresolved and non-outdated

## Plan

1. Add a Python regression test for `run_task_hooks` with a hook command that
   blocks longer than the configured timeout.
2. Add a small per-hook timeout constant to `.trellis/scripts/common/task_utils.py`
   and use a bounded subprocess wrapper.
3. Catch `subprocess.TimeoutExpired` separately and report it as a non-blocking
   warning, consistent with existing hook failure/error handling.
4. Keep the scope limited to lifecycle hook execution. Do not touch #655's task
   create duplicate behavior or add config schema for timeout tuning in this PR.

## Grill-With-Docs Challenge

- SPEC boundary: this is local Trellis developer workflow tooling, not a new
  Symphony worker hook implementation or workflow config key. No
  `DEVIATIONS.md` row is needed.
- Repo rule: AGENTS.md requires external I/O to be timeout-bounded. Hook
  commands can call tracker/network sync code, so `subprocess.run` needs a
  deadline even though hook failures remain non-blocking.
- Failure semantics: timeout should behave like other hook failures: warn on
  stderr and let the task CLI continue, because the task file operation has
  already happened.
- Test surface: reuse the `.trellis/scripts/tests` unittest directory created
  by #655 and keep tests stdlib-only.

## Acceptance Criteria

- [x] Hook subprocess calls include a timeout.
- [x] A timed-out hook prints a warning containing the event and command.
- [x] Timed-out hooks do not raise out of `run_task_hooks`.
- [x] Existing non-zero hook failure behavior remains non-blocking.
- [x] Regression test fails if the timeout/process-group/pipe-close behavior is removed.
- [x] #655 files remain untouched except for test-directory reuse.

## Outcome

- Branch/worktree: `fix/656-lifecycle-hook-timeouts` / `/Users/yvan/.codex/worktrees/656/aiops-platform`
- PR: #664, `https://github.com/xrf9268-hue/aiops-platform/pull/664`
- Head: `c4c47597e4d564017adc1c0efec1eff3851b3297`
- State: merged. PR #664 merged at `2026-06-05T14:07:29Z`; merge commit `2f5e81ed9572b377cecaa41e0596c80567d1a7b9`.
- Clean gates before merge: full local gate, mutation checks, subagent reviewer `Sagan`, Codex-family local JSON reviewer, Claude-family local JSON reviewer, CI run `27019226108`, metadata run `27019270068`, GraphQL reviewThreads `[]`, warning audit with only harmless checkout/tar/Vite warning text, and size gate `within budget`. GitHub Codex review failed twice with connector `An unknown error occurred` before the manual merge.

## Verification Plan

- Targeted Trellis tests: `python3 -m unittest discover -s .trellis/scripts/tests -p 'test_*.py'`
- Compile touched Python: `python3 -m py_compile .trellis/scripts/task.py .trellis/scripts/common/task_utils.py .trellis/scripts/tests/test_task_hooks_timeout.py`
- Full local Go gate before push:
  - `gofmt -l $(git ls-files '*.go')`
  - `go mod tidy && git diff --exit-code -- go.mod go.sum`
  - `go vet ./...`
  - `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run --config=.golangci.yml --issues-exit-code=0`
  - `go test -run '^TestProductionGoFilesStayWithinSizeBudget$' -count=1 ./scripts`
  - `go test -race -covermode=atomic ./...`
  - `go build ./cmd/worker ./cmd/tui`

## Out of Scope

- Adding a user-configurable hook timeout field.
- Resolving #655 duplicate task creation in this PR.
- Implementing Symphony workspace hooks in the worker/orchestrator.
