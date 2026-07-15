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
$AIOPS_WORKSPACE_ROOT/                    # per-task worktrees
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
- Each task gets its own directory under `AIOPS_WORKSPACE_ROOT`, so two
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
  Go build caches under `.cache/`) and build outputs carry over so the
  next run benefits from the warm cache.
  - The `git reset --quiet HEAD -- .` step is load-bearing: if a hook or
    other step left intent-to-add entries in the index (e.g. `git add -N`),
    those paths point at the empty blob. Without the reset,
    the subsequent `git checkout --force -B` would treat them as
    "files in the index but not in the target ref" and silently delete
    them from the working tree. The reset clears the index back to HEAD
    so the checkout only updates tracked files.
- **Tracked-file modifications are reset.** The `--force` flag
  discards any uncommitted edits to tracked files, so the runner starts
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
   Catches the "crashed `worktree add` / `rm -rf .git` race" cases.
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
| `AIOPS_WORKSPACE_ROOT` | _unset_ — falls back to WORKFLOW.md `workspace.root` (SPEC §6.4 default `<system-temp>/symphony_workspaces`) | Where per-task worktrees live.                           |
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
- The mirror lock only serializes mirror operations. During an active agent
  run, the worker also holds a per-worktree ownership `flock` in that worktree's
  git-admin directory so stale foreign-root reclaim fails safely when another
  process is still using the old worktree. That guard protects against deletion;
  it does not make shared `AIOPS_MIRROR_ROOT` maker/reviewer topologies
  supported.
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
disk and remain unmatched by current workspace keys. Startup
reconciliation deliberately keeps those dirs because their absence from a
tracker result is not proof of terminal state. Operators who no longer need
the orphans should audit and remove them explicitly:

```sh
# Audit (no removals): list workspace components that contain old-style
# dash separators or lowercase identifiers under the per-issue subtree.
find "$AIOPS_WORKSPACE_ROOT" -mindepth 4 -maxdepth 4 -type d

# Migrate by wiping the workspace root entirely. The bare mirror cache
# under $AIOPS_MIRROR_ROOT is untouched, so the next task pays only a
# fresh worktree-add, not a fresh clone.
rm -rf "$AIOPS_WORKSPACE_ROOT"/*
```

Active *rework* workspaces survive the cutover even without a manual
sweep: `reworkWorkspaceKeyPrefixes` emits two prefix forms for each
extracted base key so it matches every aiops-platform sanitizer
vintage that may have written to disk
(`internal/worker/reconcile.go`):

1. `<base>_rework_…` — current SPEC §4.2 sanitizer.
2. `<base>-rework-…` — interim/pre-#229 case-preserved layout with dash
   separators. Because every shipped tracker builds the Rework key from
   the all-lowercase `issue.ID` (a UUID or numeric value), this form
   also matches the pre-#229 lowercased directories, e.g.
   `linear_issue/issue-3-rework-2026-05-16t10-00-00z` produced by an
   older worker for an active Linear Rework issue, where the base of the
   dir name was the issue ID (`issue-3`) rather than the human-facing
   identifier (`LIN-123`).

(#679 removed a speculative third `<lowercased-pre-spec-base>-rework-…`
form: since form 2 is case-preserving and the shipped trackers' keys are
already lowercase, it never matched a directory form 2 did not. Re-add it
only when a tracker actually emits an `issue.ID` containing uppercase or
`[^a-zA-Z0-9._-]` characters.)

Plain (non-rework) per-issue dirs created under the old sanitizer are
not back-compat-matched. They remain on disk unless the tracker explicitly
confirms the corresponding issue is terminal or an operator removes them.

## Cleanup policy

The worker does **not** automatically delete old worktrees. The
recommended strategy is age-based pruning, gated on operator preference:

- **Per-task worktrees** (`$AIOPS_WORKSPACE_ROOT/...`): safe to delete once
  the corresponding task is `succeeded` or `failed`. Use a 24h-72h
  window for personal use; pick something larger if you frequently
  inspect old runs.
- **Bare mirrors** (`$AIOPS_MIRROR_ROOT/...`): treat as a long-lived
  cache. They survive worktree cleanup intentionally, because deleting
  them costs you a fresh `git clone` on the next task. Only purge a
  mirror when you change its upstream URL or the cache disk fills up.
- **Agent Go caches** (`$TMPDIR/aiops-go-cache/...`): the Codex
  app-server runner injects a shared `mod/` cache plus a per-workspace
  `build/<key>/` cache so sandboxed Go commands can write their cache
  files. `build/<key>/` entries are removed when the worker deletes the
  matching workspace and are also lazily reaped on app-server setup when
  the top-level `build/<key>/` directory mtime is older than 7 days. That
  TTL is not a last-cache-hit signal, so a warm but idle cache can be
  rebuilt. `mod/` is intentionally shared and is not managed by workspace
  cleanup.

The worker exposes no in-process cleanup API and no scheduler; pruning is
an out-of-band operator task.

### How to trigger cleanup

Delete stale per-task worktrees directly on disk between runs. The next
task re-creates its worktree from the bare mirror (fast), so this is safe
to run during any pause.

First confirm your **effective** workspace root: it is `workspace.root`
from `WORKFLOW.md` when set, otherwise `$AIOPS_WORKSPACE_ROOT`, otherwise
the SPEC temp default. `go run ./cmd/worker
--print-config <repo-clone>` prints the resolved value. Then prune that
exact path — never an unset variable, or the glob below expands to `/*`
and recurses into host directories:

```sh
# The :? guard makes the shell refuse to run rm when the root is unset or
# empty, so the glob can never become "/*". Replace the env var with your
# resolved workspace.root path if you do not deploy via AIOPS_WORKSPACE_ROOT.
rm -rf "${AIOPS_WORKSPACE_ROOT:?set the workspace root before pruning}"/*
```

For age-based pruning, delete only the individual task directories under
that root whose corresponding task is already `succeeded` or `failed` and
that fall outside your chosen window (24h–72h for personal use; see the
policy above). Never delete the bare mirrors under `$AIOPS_MIRROR_ROOT`:
they are the long-lived cache that keeps worktree re-creation cheap.

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
`$AIOPS_WORKSPACE_ROOT` should call `workspace.SafeRemove` rather than
`os.RemoveAll` directly.

## Troubleshooting

- `fatal: '<dir>' is not a working tree` printed during task setup is
  expected on the first prepare for a brand-new task ID and is
  swallowed. If it shows up repeatedly for the same task ID, check
  whether `$AIOPS_WORKSPACE_ROOT` is on a shared filesystem with a different
  worker writing to the same path.
- If you see `error: cannot lock ref 'refs/heads/<work>'` during
  `worktree add`, run `git -C $AIOPS_MIRROR_ROOT/<host>/<repo>.git
  worktree prune` and retry. The worker does this automatically before
  every prepare; manual intervention is only needed if you ran
  out-of-band git commands against the mirror.
- To inspect what mirrors exist:
  `find $AIOPS_MIRROR_ROOT -maxdepth 4 -name HEAD -printf '%h\n'`.
