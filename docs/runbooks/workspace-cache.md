# Workspace cache and cleanup

The worker (`cmd/worker`) prepares a Git workspace for every claimed task.
Until M2 each task ran a fresh `git clone`, which paid the full network
cost on every retry and on every concurrent task. This runbook describes
the post-#11 layout: a per-repo bare cache plus per-task worktrees, and
how to prune the cache on a long-running host.

## Layout

```text
$AIOPS_MIRROR_ROOT/                 # bare mirror cache (one per repo)
  <host>/<owner>/<repo>.git/        # populated by `git clone --bare`
$WORKSPACE_ROOT/                    # per-task worktrees
  <owner>_<repo>/<task-id>/         # populated by `git worktree add`
```

- The first task targeting a given clone URL pays the network cost of
  `git clone --bare`. The fetch refspec is normalised to
  `+refs/heads/*:refs/remotes/origin/*` so future fetches keep upstream
  branches under `refs/remotes/origin/*` and never disturb the
  per-task work branches we create under `refs/heads/<work>`.
- Subsequent tasks (same repo, different task IDs) skip the clone and
  run `git fetch --prune --tags origin` against the cached mirror, then
  `git worktree add -B <work-branch> <task-dir> origin/<base-branch>`.
  The worktree shares objects with the mirror via `gitdir` linkage, so
  disk usage is roughly `O(repo)` rather than `O(repo) * O(tasks)`.
- Each task gets its own directory under `WORKSPACE_ROOT`, so two
  concurrent workers never share a working tree.

## Workspace reuse across runs (SPEC §9.1 + §9.4)

Per-issue workspaces are **reused across runs for the same issue**
(SPEC §9.1). `PrepareGitWorkspace` only adds a fresh worktree on the
first touch (or when recovering from a corrupted leftover); subsequent
prepares for the same `(repo, issue)` pair reset the work branch to
`origin/<base-branch>` via:

```text
git reset --quiet HEAD -- .
git checkout --force --no-track -B <work-branch> origin/<base-branch>
```

What this preserves and what it resets:

- **Untracked files survive.** Cached deps (`node_modules/`, `venv/`,
  Go build caches under `.cache/`), build outputs, and any
  `.aiops/POLICY_VIOLATION_FEEDBACK.md` left by a prior policy
  violation carry over so the next run benefits from the warm cache
  and sees the same operator/agent feedback.
  - The `git reset --quiet HEAD -- .` step is load-bearing: the prior
    run's `EnforcePolicy` calls `git add --intent-to-add --all` to make
    untracked files visible to `git diff --numstat`, which leaves those
    paths in the index pointing at the empty blob. Without the reset,
    the subsequent `git checkout --force -B` would treat them as
    "files in the index but not in the target ref" and silently delete
    them from the working tree. The reset clears the index back to HEAD
    so the checkout only updates tracked files.
- **Tracked-file modifications are reset.** The `--force` flag
  discards any uncommitted edits to tracked files; `RUN_SUMMARY.md` is
  also wiped explicitly by `workspace.ResetRunSummary` before the
  runner starts, and `enforceAnalysisOnlyChanges` resets
  `.aiops/PLAN.md` for analysis-only mode. The runner therefore starts
  every run from a known-clean tracked state.
- **The work branch is rebased to `origin/<base-branch>`.** Commits the
  previous run pushed on the work branch live in the PR / remote; the
  next run starts from the refreshed base, not on top of those
  commits. `--no-track` matches the create path's
  `worktree add --no-track` so the work branch never tracks the base
  branch (otherwise a stray `git pull` inside the worktree would merge
  the base into the work branch).

The `after_create` workspace hook runs only on the first touch
(SPEC §9.4 / §17.2 conformance test): `RunTask` checks the
`createdNow` boolean from `PrepareGitWorkspace` and skips the hook on
reuse. Expensive one-time bootstrap commands (`npm ci`,
`pip install -r requirements.txt`) belong in `after_create`; per-run
setup belongs in `before_run`.

If you depend on a brand-new workspace for a particular run, delete
the workspace directory yourself before the next task tick — the
worker treats a missing path as "first touch" and fires
`after_create` again.

### Recovery from a corrupted or hostile workspace

Before taking the reuse path, `PrepareGitWorkspace` runs three gates
against `<workdir>`. Failing any one falls through to the
nuke-and-recreate path, reports `createdNow=true`, and fires
`after_create` again:

1. **No symlinks.** `os.Lstat` rejects a symlinked `<workdir>` so the
   reuse-path `git reset` / `git checkout -B` can never be redirected
   into a repository outside the workspace root by a planted
   symlink. The recreate path's `os.RemoveAll` removes the symlink
   itself, not its target.
2. **Valid git worktree.** `git rev-parse --git-dir` must succeed.
   Catches the "crashed `worktree add` / `rm -rf .git` race / chmod
   broke linkage" cases.
3. **Linked to OUR mirror.** `git rev-parse --git-common-dir` must
   resolve (via `filepath.EvalSymlinks`) to the same path as the
   cached bare mirror for `t.CloneURL`. Catches an independent
   `git init` planted at the workspace path and a worktree linked to
   a different mirror (e.g. clone URL changed between prepares).

Operators can always force a clean reset by removing `<workdir>` (or
just `<workdir>/.git`) — the worker recovers on the next prepare.

## Configuration

| Env var               | Default                                                                 | Purpose                                                  |
| --------------------- | ----------------------------------------------------------------------- | -------------------------------------------------------- |
| `WORKSPACE_ROOT`      | `/tmp/aiops-workspaces`                                                 | Where per-task worktrees live.                           |
| `AIOPS_MIRROR_ROOT`   | `os.UserCacheDir()/aiops-platform/mirrors` (fallback `$TMPDIR/...`)     | Where bare mirror clones are cached.                     |

On Linux containers `os.UserCacheDir()` resolves to `$XDG_CACHE_HOME` or
`$HOME/.cache`. On macOS dev boxes it resolves to
`~/Library/Caches/aiops-platform/mirrors`. Override `AIOPS_MIRROR_ROOT`
when you want the cache on a dedicated volume — for example the
`workspace-cache` named volume in `deploy/docker-compose.yml`.

## Cross-process mirror locking

Two workers sharing the same `AIOPS_MIRROR_ROOT` on the same host serialize
their `git fetch` / `git clone` / `git worktree add` operations through an
advisory `flock(2)` on a `<mirror>.lock` sidecar file (#228). This closes
the gap where the per-process `sync.Mutex` alone could not stop one
worker's `git fetch --prune` from racing another worker's
`git worktree add` against the same bare repo, which historically
surfaced as sporadic `fatal: <branch> already exists` errors that "just
worked" on retry.

Operational notes:

- The lock is **host-scoped**. Two workers must run on the same OS
  instance for the lock to mediate them; cross-host concurrency is not
  supported, because `flock(2)` is a per-kernel primitive.
- **NFS mirror roots are unsupported.** `flock(2)` on NFS is silently
  best-effort on Linux (depends on `lockd` being healthy) and not
  honoured on macOS. Use a local filesystem.
- The lock sidecar file lives next to the mirror as `<mirror>.lock`.
  Operators removing a mirror by hand should also remove the sidecar,
  but must not delete it while a worker holds the lock — `lsof
  <mirror>.lock` attributes the holder to a specific pid.
- Windows hosts have no `flock(2)`; the cross-process layer is a no-op
  there and only the in-process mutex serializes. The supported
  deployment platforms (Linux containers, macOS LaunchAgents) both use
  the full file-lock path.

## Cleanup policy

The worker does **not** automatically delete old worktrees. The
recommended strategy is age-based pruning, gated on operator preference:

- **Per-task worktrees** (`$WORKSPACE_ROOT/...`): safe to delete once
  the corresponding task is `succeeded` or `failed`. Use a 24h-72h
  window for personal use; pick something larger if you frequently
  inspect old runs.
- **Bare mirrors** (`$AIOPS_MIRROR_ROOT/...`): treat as a long-lived
  cache. They survive worktree cleanup intentionally, because deleting
  them costs you a fresh `git clone` on the next task. Only purge a
  mirror when you change its upstream URL or the cache disk fills up.

The Go API for cleanup is `(*workspace.Manager).Cleanup(ctx, maxAge)`.
It walks `$WORKSPACE_ROOT`, deletes each task directory whose mtime
predates `now - maxAge`, and returns a `CleanupReport` with counts of
removed/skipped/failed entries. It never touches the mirror cache.

### How to trigger cleanup

There is no built-in scheduler. Pick one of the following based on how
the worker is deployed:

1. **Cron / launchd**: schedule a tiny wrapper binary that calls
   `workspace.New(root).Cleanup(ctx, 48*time.Hour)`. Suitable for the
   personal daily workflow described in
   [personal-daily-workflow.md](personal-daily-workflow.md).
2. **Manual**: `rm -rf $WORKSPACE_ROOT/*` between long pauses works in
   a pinch. The next task simply re-creates its worktree from the bare
   mirror, which is fast.
3. **Future hook**: a follow-up issue may wire `Cleanup` into the
   worker's main loop (e.g. once per N completed tasks). Until that
   lands, treat cleanup as an out-of-band concern.

## Cleanup containment invariant

All cleanup paths that delete a per-task worktree go through
`workspace.SafeRemove(root, path)`. The helper refuses to delete a path
that is not strictly contained under the configured workspace root:
empty/whitespace input, the root itself, a sibling of the root, a
`..` traversal, and symlinks whose resolved target points outside the
root all return `ErrSafeRemoveEscapesRoot` and the on-disk path is left
untouched. This is a defense-in-depth guard against a future refactor or
a malformed hook output that could otherwise pass an empty string or
`/` into `os.RemoveAll` — see SPEC §9.5 Invariants 2 & 3, §15.2.

The two production call sites today are
`internal/worker/runtask.go::removeWorkdirAfterHookFailure` (rollback
after `after_create` hook failure) and
`internal/worker/reconcile.go::removeWorkspace` (reconcile-driven
cleanup for closed/cancelled issues). Any new cleanup code that touches
`$WORKSPACE_ROOT` should call `workspace.SafeRemove` rather than
`os.RemoveAll` directly.

## Troubleshooting

- `fatal: '<dir>' is not a working tree` printed during task setup is
  expected on the first prepare for a brand-new task ID and is
  swallowed. If it shows up repeatedly for the same task ID, check
  whether `$WORKSPACE_ROOT` is on a shared filesystem with a different
  worker writing to the same path.
- If you see `error: cannot lock ref 'refs/heads/<work>'` during
  `worktree add`, run `git -C $AIOPS_MIRROR_ROOT/<host>/<repo>.git
  worktree prune` and retry. The worker does this automatically before
  every prepare; manual intervention is only needed if you ran
  out-of-band git commands against the mirror.
- To inspect what mirrors exist:
  `find $AIOPS_MIRROR_ROOT -maxdepth 4 -name HEAD -printf '%h\n'`.
