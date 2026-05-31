# Personal daily workflow

This runbook describes how to use `aiops-platform` day to day so it stays a useful tool instead of another system to babysit.

The defaults below match `examples/WORKFLOW.md` and the active state list in `internal/workflow/config.go`. `cmd/worker` is the day-to-day scheduler entrypoint: it reads the configured tracker directly, performs startup reconciliation, and dispatches through in-memory orchestrator runtime state.

## Linear states

The worker poll tick reads issues whose state name appears in `tracker.active_states` of your `WORKFLOW.md`. Use a simple lifecycle:

```text
Backlog -> AI Ready -> In Progress -> Human Review -> Rework -> Done
                                                  \-> Canceled
```

Per-state rules:

- `Backlog`: not picked up. Use this for anything you have not refined yet.
- `AI Ready`: refined and small enough that an agent can attempt it. The worker poll tick picks these up.
- `In Progress`: the agent has claimed an issue or you are iterating on it. Stays in `active_states` so re-runs after a push are allowed.
- `Human Review`: agent finished and opened a draft PR. Not in `active_states`. Read the diff yourself. Per SPEC §1, tracker updates belong on the agent/tool side, not in the worker scheduler.
- `Rework`: review found issues and you want another attempt. Keep `Rework` in `active_states` so the worker can pick the issue up again after the tracker state changes. The in-memory orchestrator state, not Postgres queue rows, is the scheduling authority for the running worker process.
- `Done` / `Canceled`: terminal. Listed under `tracker.terminal_states`.

Default `active_states` in code:

```yaml
tracker:
  active_states:
    - AI Ready
    - In Progress
    - Rework
```

Rule of thumb: if you do not feel comfortable letting an agent open a draft PR for an issue right now, do not move it to `AI Ready`.

## Writing good issues

The issue title and description are passed to the runner via the `PROMPT.md` template (see `examples/WORKFLOW.md`). Vague issues produce vague diffs.

Checklist before moving an issue to `AI Ready`:

- One clear outcome. Not "improve logging" but "log tracker issue id in worker poll dispatch output".
- Concrete file or package hints. Mention paths like `internal/runner/shell.go` so the agent does not wander.
- Acceptance criteria as a bullet list. The verification section in `WORKFLOW.md` runs `go test ./...`, but tests do not catch design intent.
- Out-of-scope notes. Call out things you do not want touched, especially anything under `policy.deny_paths` (`infra/**`, `deploy/**`, `db/migrations/**`, `secrets/**`).
- Size budget that fits `policy.max_changed_files: 12` and `policy.max_changed_loc: 300`. If a task realistically needs more, split it.
- Link to the relevant ADR or research doc when the task involves architecture decisions.

Issue template that works well:

```markdown
## Goal
<one sentence>

## Acceptance criteria
- ...
- ...

## Scope hints
- touch: <paths>
- do not touch: <paths>

## Notes
<anything an agent could not infer from the codebase>
```

## Choosing a runner

Runners are selected by the `agent.default` field in `WORKFLOW.md` and resolved in `internal/runner/runner.go`. Valid values: `mock`, `codex-app-server`, `claude`.

### `mock`

What it does: writes a stub file under `.aiops/<task-id>.md` and returns success. No model call. See `internal/runner/mock.go`.

Use when:

- you just changed `WORKFLOW.md`, tracker polling, startup reconciliation, or PR handoff and want to confirm the loop end-to-end.
- you are onboarding a new repository and want a safe first PR before pointing a real model at it.
- the model API is rate-limited or down and you still want to verify tracker polling and worker behavior.

```yaml
agent:
  default: mock
```

### `codex-app-server`

The SPEC §10 runner (`internal/runner/codex_app_server.go`). It launches `codex app-server` once and drives a long-running JSON-RPC 2.0 session over stdio, running multiple agent turns inside one worker session bounded by `agent.max_turns` (SPEC §5.3.5). PROMPT.md seeds the first turn; combined stdio is captured to `.aiops/CODEX_APP_SERVER_OUTPUT.txt`.

Use when:

- you want the SPEC-aligned Codex runner — this is the default real runner.
- the task may need several agent turns within a single session.
- you have a Codex CLI session already authenticated locally.

```yaml
agent:
  default: codex-app-server
codex:
  command: codex app-server
```

The agent's sandbox/approval posture is set by `codex.thread_sandbox` (per-session) and `codex.turn_sandbox_policy` (per-turn; derived from `thread_sandbox` when unset — see DEVIATIONS.md D32). Use `workspace-write` on shared hosts and `danger-full-access` only on already-isolated workers (container, dedicated VM).

> The earlier non-SPEC `codex` (one-shot `codex exec`) runner was removed under [#541](https://github.com/xrf9268-hue/aiops-platform/issues/541); it drove the same agent as `codex app-server` in a strictly worse mode (no in-session `max_turns`).

### Reading codex output after a run

Each run writes `.aiops/CODEX_APP_SERVER_OUTPUT.txt` (combined stdout+stderr, capped at 1 MiB with a truncation footer when the cap fires). The `runner_end` task event payload also carries `output_head` (first 4 KiB), `output_tail` (last 4 KiB if non-overlapping), `output_bytes`, and `output_dropped` for at-a-glance triage from worker logs and runtime events without cloning the work branch.

### `claude`

Shell runner (`internal/runner/shell.go`) that invokes `claude.command` (default `claude`) via `sh -c` with PROMPT.md on stdin.

Use when:

- the task touches several files or needs more reasoning across a package.
- you want richer tool use during the run.
- Codex produced a thin or wrong patch and you want a second opinion. To switch runner, set `agent.default` in `WORKFLOW.md`. Note: `agent.fallback` was removed in issue #40 — workflows that still carry the key now fail validation at load time.

```yaml
agent:
  default: claude
claude:
  command: claude
```

### Manual review (no runner)

Skip automation entirely when:

- the task touches `policy.deny_paths` (infra, deploy, migrations, secrets).
- it is a security-sensitive change or a data migration.
- requirements are still ambiguous. Use a planning issue instead and only move to `AI Ready` once the design is settled.
- the change is large enough that a draft PR would exceed `max_changed_files` or `max_changed_loc`.

Decision shortcut:

```text
unsure or risky                  -> manual review, keep issue out of AI Ready
plumbing or smoke test           -> mock
small, well-scoped code change   -> codex-app-server
multi-file or reasoning-heavy    -> claude
```

## Handling failed tasks

The SPEC-aligned worker does not persist scheduler attempts in Postgres. It polls the tracker, records in-flight/completed/retry state in the in-memory orchestrator runtime, and starts with fresh scheduler state after restart. Startup reconciliation compares tracker state with deterministic workspaces before the first poll so terminal/unknown workspace directories are cleaned without reading queue rows.

### Triage with the runtime status API

Use the worker-owned runtime status surface:

```bash
curl 'http://127.0.0.1:4000/api/v1/state'
```

For failed tasks, inspect process logs and task events emitted by
`worker.RunTask` such as `workflow_resolved`, `runner_start`, `verify_*`, and
reconciliation events. The deterministic workspace also retains
`.aiops/RUN_SUMMARY.md`, `.aiops/VERIFICATION.txt`, and
`.aiops/CHANGED_FILES.txt` when the runner reaches those phases.

### Common causes and fixes

- `repo.clone_url missing in WORKFLOW.md`: worker log line. Fix `WORKFLOW.md`, restart the worker.
- Verification command failed (`go test ./...` non-zero): read `.aiops/RUN_SUMMARY.md` in the work branch if the runner produced one. Reproduce locally on the same branch.
- Policy violation (deny path or size cap): re-scope the task into a smaller issue, or do it manually.
- Runner command not found: confirm `codex.command` (the `codex app-server` launch command) or `claude.command` resolves in the worker's scoped `PATH`. The `claude` shell runner uses plain `sh -c`, so `/etc/profile.d/*` and `~/.profile` are not re-sourced per command; pass any required non-secret env explicitly with `codex.env_passthrough` or `claude.env_passthrough`.
- Empty diff: agent decided nothing to do. Tighten the issue body, then move it to `Rework` (or the equivalent active Gitea `aiops/*` state label).

### Re-running a failed task

Three options, in order of preference:

1. Move the tracker issue to an active retry state such as `Rework`, or keep it in another configured `active_states` value after tightening the issue body. The worker poll tick sees active tracker state and asks the in-memory orchestrator to dispatch it.
2. Restart the worker if you need to clear in-memory retry state. Startup reconciliation runs first, then the next poll can dispatch still-active tracker issues without reading queue rows.
3. If a tracker issue still does not dispatch after its state is active and the worker has restarted cleanly, inspect the worker logs and open a focused bug. Do not reintroduce manual queue enqueue as the normal retry path.

### When to give up on automation

Mark the issue back to `Backlog` or `Human Review` and finish it by hand if any of these are true:

- two runner attempts produced wrong or empty diffs.
- the failure mode is unclear after reading worker logs and workspace artifacts.
- the work has crossed into a `deny_paths` area or grown past the size caps.

The platform is meant to save time on small, well-scoped tasks. When it stops doing that for a given issue, do not fight it.
