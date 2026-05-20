# Kanban Throughput Migration Plan

Updated: 2026-05-20T09:27:00+08:00

## Problem snapshot
- The aiops-platform Kanban board was operating in a low-throughput legacy mode: tasks used `workspace_kind=dir` with shared checkout `/home/pi/work/aiops-platform`, and `kanban.max_spawn` was effectively a single-worker lane.
- The shared checkout is currently dirty on branch `fix/issue-149-workspace-root-precedence` with issue #149 changes. This means generic dispatch of later issues can mutate the wrong branch.
- The embedded gateway dispatcher auto-spawned issue #150 into the dirty #149 checkout. That worker was reclaimed immediately as a misdirected spawn before it made a useful handoff.
- Ready tasks were temporarily blocked as migration holds so no further worker is spawned against the shared checkout while worktree migration is in progress.

## Target operating model
1. Issue implementation runs in dedicated git worktrees under `/home/pi/work/aiops-platform-worktrees/issue-<number>`.
2. `kanban.max_spawn` is a live concurrency cap. Use `2` initially on this Raspberry Pi; consider `3` only after CPU/memory/API behavior is stable.
3. The shared checkout `/home/pi/work/aiops-platform` is reserved for operator recovery, main fast-forward/sync, and serial PR merge/follow-through operations.
4. PR follow-through and merge remain serialized behind `/tmp/aiops-platform-agent.lock`; implementation workers should not wait on CI/Codex for long periods while holding the implementation lane.
5. Heartbeat/precheck must detect throughput/configuration defects, not merely the existence of a process. In particular, `max_spawn > 1` plus ready tasks on shared `dir` workspace is an actionable defect.
6. Do not dispatch later issues while the shared checkout is dirty unless those issues have independent worktrees and the dirty checkout has an explicit owner/handoff.

## Current safe-state actions already taken
- Paused heartbeat cron job `75e947fdff1c`.
- Set `kanban.dispatch_in_gateway=false` and restarted the gateway so embedded dispatch cannot race the migration.
- Reclaimed the misdirected issue #150 worker from shared checkout.
- Blocked ready tasks #150/#151/#159 with migration-hold reasons.

## Implementation steps
1. Finish/preserve issue #149 dirty checkout:
   - Identify whether the diff is complete enough to test and PR, or needs a dedicated issue #149 worktree.
   - Preserve before any destructive action: no reset/stash without diff backup.
   - Run gofmt/focused tests/full `/home/pi/.local/go1.25.10/bin/go test ./...` and independent diff review before push.
2. Add worktree bootstrap tooling:
   - Create an operator script that maps Kanban task ids to issue numbers, creates/fixes `issue-<n>` branches from current `origin/main`, and updates `tasks.workspace_kind='worktree'`, `tasks.workspace_path=<absolute path>`.
   - The script must refuse to overwrite dirty existing worktrees.
3. Harden precheck:
   - Include `kanban_config` (`dispatch_in_gateway`, `max_spawn`) in diagnostics.
   - Detect ready/running tasks with `workspace_kind='dir'` and shared path `/home/pi/work/aiops-platform`.
   - If `max_spawn > 1` and shared ready tasks exist, set `wakeAgent=true`, lane `shared_workspace_migration_required`.
   - Distinguish dirty shared checkout from dirty per-issue worktree; dirty shared checkout blocks generic shared dispatch, but not independent worktree dispatch.
4. Harden heartbeat cron prompt:
   - Replace “dispatch exactly one worker” with “dispatch up to the configured live concurrency cap after dry-run, but only worktree-backed tasks are eligible for parallel implementation.”
   - Keep PR follow-through/merge serial and lock-protected.
   - Prohibit unblocking `dir:/home/pi/work/aiops-platform` implementation tasks when `max_spawn > 1`.
5. Migrate board tasks:
   - #149: resolve shared dirty work first, then move any remaining task state to `/home/pi/work/aiops-platform-worktrees/issue-149` or complete after PR merge.
   - #150/#151/#159: create/update dedicated worktrees and unblock up to the concurrency target.
   - #146/#147/#148: migrate before any future unblock; note #148 already has a worktree directory and should be reconciled before reuse.
6. Enable controlled dispatch:
   - Set `kanban.max_spawn=2`.
   - Keep `dispatch_in_gateway=false` during validation; use manual `hermes kanban dispatch --dry-run --max 2 --json`, then dispatch.
   - After verification, either resume heartbeat as the orchestrator or explicitly re-enable gateway dispatcher only if precheck/prompt are proven safe.
7. Verify and resume:
   - Dry-run shows at most two worktree-backed tasks.
   - Live dispatch creates workers whose cwd/workspace is their per-issue worktree.
   - No worker uses `/home/pi/work/aiops-platform` for implementation.
   - Precheck reports coherent state: valid worktree workers active, clean PR follow-through, or explicit recovery lane.

## Retrospective notes
- Root cause: shared checkout + auto-dispatch made task ownership ambiguous; a later task could start on a previous issue branch.
- Durable guardrail: never treat a live process as sufficient progress if queue/workspace configuration prevents throughput or risks wrong-branch mutation.
- No credentials or secrets are recorded here.
