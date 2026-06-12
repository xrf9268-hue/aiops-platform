# PRD: bound workspace git operations with per-op timeouts (#759)

## Problem

Every git subprocess under `internal/workspace` (`runGit` / `runGitRedacted` /
`runGitQuiet`, manager.go:485,500,528) executes via
`exec.CommandContext(ctx, "git", ...)` where `ctx` is the dispatch run context
created at `internal/orchestrator/actor_dispatch.go:268` with
`context.WithCancelCause` — no deadline anywhere on the chain. A remote that
black-holes leaves `git clone`/`git fetch` blocked indefinitely; the dispatch
never reaches the runner, the capacity slot is never released, and D9
reconcile-cancel never fires while the tracker issue stays active.

Violates AGENTS.md §Conventions ("All external I/O is timeout-bounded") and
cross-cutting checklist item 2.

## Solution (verdict from issue #759)

Wrap each git operation in a per-operation `context.WithTimeout` inside the
workspace package, at the `runGit*` seam so every call site is covered:

- `runGitRedacted` (network: `clone --bare`, `fetch`): generous fixed default
  (10 minutes).
- `runGit` / `runGitQuiet` (local: `config`, `worktree add/remove`, checkout):
  shorter default (5 minutes — worktree materialization of a large repo on a
  slow disk can exceed 1 minute).
- Timeouts are package-level `var`s so tests can shrink them; no new
  `WORKFLOW.md` key (avoids the SPEC-schema gate for a knob nobody needs yet).
- `context.WithTimeout` keeps the earlier of (parent deadline, per-op timeout),
  so reconcile-cancel / shutdown still pre-empts immediately.
- On deadline, surface a wrapped, classified error (`errors.Is(err,
  context.DeadlineExceeded)` must hold); no string matching.
- `runRedacted` pipes stdout/stderr through an io.Writer, so exec uses OS pipes
  + copy goroutines; an escaped git child holding the pipe would block
  `cmd.Wait` past the kill — set `cmd.WaitDelay` (mirror the workspace-hook
  runner's escaped-descendant handling) so the deadline actually terminates.

## Acceptance criteria (from #759)

- [ ] Every `runGit` / `runGitRedacted` / `runGitQuiet` call site executes
      under a bounded context even when the caller's ctx has no deadline.
- [ ] Test: a hung fake `git` (PATH stub that sleeps) is killed at the
      deadline and surfaces a wrapped, classified error.
- [ ] Test: parent-context cancellation still pre-empts the per-op timeout.
- [ ] Mutation-verify per clean-code rule 6/11: deleting the timeout wrap at
      the seam fails the test (verify against committed artifact).
- [ ] Local CI gates green: gofmt, go vet, go mod tidy, golangci-lint,
      size-budget test, `go test -race ./...`, builds.

## Out of scope

- New config keys, retry semantics, process-group kill changes beyond what the
  timeout fix requires.
- Tracker-client work (#761/#762) and CI pinning (#763).

## Refs

- Issue: https://github.com/xrf9268-hue/aiops-platform/issues/759
- AGENTS.md §Conventions, §Cross-cutting checklist item 2
- Branch: claude/project-review-report-nrxvf1
