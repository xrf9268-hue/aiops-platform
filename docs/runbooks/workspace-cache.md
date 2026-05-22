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

## SPEC §4.2 path-component sanitization

Workspace path components (`<owner>`, `<repo>`, `<source_type>`,
`<source_event_id>`, task-ID fallback) go through
`workspace.SanitizeComponent`. The rule, lifted from SPEC §4.2:

> Derive from `issue.identifier` by replacing any character not in
> `[A-Za-z0-9._-]` with `_`.

That is exactly what the sanitizer does today — case is preserved
verbatim, and any other character (including the multi-byte runes of a
CJK identifier) is substituted with a single `_`. On top of the SPEC
rule the harness adds three filesystem-safety guards:

1. **Rune-length cap**: long inputs are truncated to 120 runes so the
   resulting directory name fits common filesystem limits.
2. **Path-traversal block**: a component that sanitizes to exactly `.`
   or `..` (the only two values that `filepath.Join` would interpret as
   a traversal segment) is replaced with the literal string `unknown`.
   A `..` *substring* inside a longer component (e.g. `a..b`) is fine
   and is left untouched, because `filepath.Join` treats it as a name.
3. **Empty fallback**: an empty input maps to `unknown` so `PathFor`
   never produces an empty path segment.

### Migration from the pre-SPEC layout

Before #229 the sanitizer lowercased the input, accepted any
`unicode.IsLetter` rune (so CJK identifiers passed through unchanged),
and substituted `-` for invalid characters. The resulting workspace
paths therefore looked like `acme/demo/linear_issue/lin-1-needs-fix`
rather than the SPEC-conformant `acme/demo/linear_issue/LIN_1_Needs_Fix`.

Because the project is pre-release ([`AGENTS.md`
§SPEC-alignment-is-a-hard-requirement](../../AGENTS.md#spec-alignment-is-a-hard-requirement),
[`DEVIATIONS.md`](../../DEVIATIONS.md)) the cutover is hard, not
gradual: dirs created under the pre-#229 sanitizer are orphaned on
disk and will be removed by the next startup reconciliation pass as
"unknown" workspaces. Operators should clean up the orphans before
rolling out the change to avoid a one-time burst of reconcile-remove
events on the next worker boot:

```sh
# Audit (no removals): list workspace components that contain old-style
# dash separators or lowercase identifiers under the per-issue subtree.
find "$WORKSPACE_ROOT" -mindepth 4 -maxdepth 4 -type d

# Migrate by wiping the workspace root entirely. The bare mirror cache
# under $AIOPS_MIRROR_ROOT is untouched, so the next task pays only a
# fresh worktree-add, not a fresh clone.
rm -rf "$WORKSPACE_ROOT"/*
```

Active *rework* workspaces survive the cutover even without a manual
sweep: `reworkWorkspaceKeyPrefixes` emits both the canonical SPEC
`_rework_` prefix and the pre-#229 `-rework-` prefix when looking for
on-disk dirs that belong to a still-active Rework issue
(`internal/worker/reconcile.go`). Plain (non-rework) per-issue dirs
created under the old sanitizer are not back-compat-matched and will
be reconciled away.

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
