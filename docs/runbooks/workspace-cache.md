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
