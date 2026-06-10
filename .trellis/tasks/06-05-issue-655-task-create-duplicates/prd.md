# Issue #655: reject duplicate task creation

## Goal

Close GitHub issue #655 by preventing `task.py create` from overwriting an
existing same-day task directory with the same title/slug.

## Live Issue Facts

- Issue: #655, `Follow up unresolved review thread from PR #650`
- Labels: `priority:p2`, `type:chore`
- State: closed by PR #663
- Existing open PR: none found at batch startup
- Originating PR: #650, merged at `2026-06-05T04:04:13Z`
- Review thread: `https://github.com/xrf9268-hue/aiops-platform/pull/650#discussion_r3360287672`
- Location: `.trellis/scripts/common/task_store.py:230`
- Thread state from GraphQL: unresolved and non-outdated

## Plan

1. Add a regression test for duplicate same-day `task.py create` with the same
   slug/title. The test must prove the second create exits non-zero and the
   existing `task.json` metadata remains unchanged.
2. Change `cmd_create` in `.trellis/scripts/common/task_store.py` so an existing
   active task directory is a hard error, matching the existing archived-task
   duplicate behavior.
3. Keep the scope limited to duplicate active task creation. Do not add a
   force/overwrite option in this issue; the review thread explicitly requires
   failure unless such an explicit operation exists, and none exists today.

## Grill-With-Docs Challenge

- SPEC boundary: this touches local Trellis task-management tooling, not the
  Symphony worker/orchestrator/tracker boundary. No worker phase, config key, or
  DEVIATIONS row is involved.
- Data-loss invariant: an existing task directory means existing metadata may
  include status, assignee, parent/children, branch, PR, commit, and notes. The
  fix must preserve that file byte-for-byte on duplicate create.
- UX: archived duplicates already fail. Active duplicates should fail with the
  same fail-fast posture and suggest choosing a new slug.
- Test surface: the repository does not have a dedicated pytest suite for
  Trellis scripts, so add a focused Python stdlib `unittest` under
  `.trellis/scripts/tests/` and run it directly in addition to `py_compile`.

## Acceptance Criteria

- [x] Duplicate active task create exits non-zero before writing task metadata.
- [x] Existing `task.json` content remains unchanged after a duplicate create.
- [x] Error output names the duplicate task directory and suggests a new slug.
- [x] Regression test fails when the production guard is removed or softened to
  a warning.
- [x] #656 remains untouched and serialized because it targets
  `.trellis/scripts/common/task_utils.py`.

## Outcome

- Branch/worktree: `fix/655-task-create-duplicates` / `/Users/yvan/.codex/worktrees/655/aiops-platform`
- PR: #663, `https://github.com/xrf9268-hue/aiops-platform/pull/663`
- Head: `dd8b066047ed70ef71ce3240ac802f64600dc3f8`
- State: merged. PR #663 merged at `2026-06-05T13:08:13Z`; merge commit `e7df3213e3013276e9062de28615323b915b5993`.
- Clean gates before merge: full local gate, mutation check, subagent reviewer `Russell`, Codex-family local JSON reviewer, Claude-family local JSON reviewer, CI run `27015678121`, metadata run `27016167602`, GraphQL reviewThreads `[]`, warning audit with no `warning:` lines, and size gate `within budget`.

## Verification Plan

- Targeted Trellis test: `python3 -m unittest .trellis.scripts.tests.test_task_create_duplicates`
- Compile touched Python: `python3 -m py_compile .trellis/scripts/task.py .trellis/scripts/common/task_store.py`
- Full local Go gate before push:
  - `gofmt -l $(git ls-files '*.go')`
  - `go mod tidy && git diff --exit-code -- go.mod go.sum`
  - `go vet ./...`
  - `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run --config=.golangci.yml --issues-exit-code=0`
  - `go test -run '^TestProductionGoFilesStayWithinSizeBudget$' -count=1 ./scripts`
  - `go test -race -covermode=atomic ./...`
  - `go build ./cmd/worker ./cmd/tui`

## Out of Scope

- Adding a `--force` or overwrite mode.
- Changing lifecycle hook timeout behavior from #656.
- Resolving any review thread from PR #650 that is not the #655 duplicate task
  directory thread.
