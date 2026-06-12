# issue #765 — mirror robustness follow-ups from the #764 review

Requirements = issue #765 acceptance criteria (PR #764 round-2 LOW findings),
both in `internal/workspace/mirror.go`:

1. **Legacy HEAD-bearing partial mirror wedge.** `ensureMirrorLocked`'s
   existing-mirror branch must re-assert `remote.origin.fetch`
   (`+refs/heads/*:refs/remotes/origin/*`) before the refresh fetch, so a
   pre-#764 partial clone at the final path (HEAD + origin URL, no refspec,
   no refs) heals instead of wedging every §8.4 retry.
   - Regression test constructs the legacy state and proves
     `EnsureMirror`/`PrepareGitWorkspace` yields a worktree-able mirror.
   - Adjacent-path repair: `TestPrepareGitWorkspace_StartRefFallsBackToBareBaseBranchName`
     currently drives the startRef fallback by unsetting the refspec — the
     re-assert makes that construction a placebo. Rework it to drive the
     fallback by deleting the upstream branch (fetch --prune drops
     `origin/main`, mirror keeps bare `refs/heads/main`).

2. **Stale `.git.staging` leak on failed first clone.** `cloneMirrorLocked`
   error paths must remove the staging dir (success path renames it away).
   - Test pins it via a PATH git-shim whose `clone` leaves a partial staging
     dir and exits non-zero (the timeout/SIGKILL shape from #759).
   - Keep the head-of-function `RemoveAll(staging)`: a SIGKILLed process
     never runs the in-process cleanup, so the next-attempt sweep stays.

Mutation-verify both per AGENTS.md clean-code rule 6 (commit first, break the
production line, test must fail, restore).

Upstream check (principle 6): Elixir reference has no mirror/bare-clone
equivalent — the mirror cache is an established aiops workspace mechanism
(#228, #764); this is same-file hardening, not a new worker-side phase.
