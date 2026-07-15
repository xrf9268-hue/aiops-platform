# Personal daily workflow

This runbook describes how to use `aiops-platform` day to day so it stays a useful tool instead of another system to babysit.

The defaults below match `examples/WORKFLOW.md` and the active state list in `internal/workflow/config.go`. `cmd/worker` is the day-to-day scheduler entrypoint: it reads the configured tracker directly, performs startup reconciliation, and dispatches through in-memory orchestrator runtime state.

## Daily entrypoint

Use the tracker issue as the planning ledger before moving work into the
worker's active set:

1. Write the goal, scope, acceptance criteria, dependency class, and verification
   in the tracker issue. For a batch, keep a short parent issue or local note
   that maps each child issue to its readiness and PR state.
2. Review the plan with `grill-with-docs` against `CONTEXT.md`, ADRs, and the
   relevant runbook.
3. Move only ready work through the tracker ready gate.

Local notes do not dispatch work and do not replace tracker state. The worker
only sees the configured tracker states or labels in `WORKFLOW.md`.

## Linear states

The worker poll tick reads issues whose state name appears in `tracker.active_states` of your `WORKFLOW.md`. Use a simple lifecycle:

```text
Backlog -> Todo -> In Progress -> Human Review -> Rework -> Done
                                                  \-> Canceled
```

Per-state rules:

- `Backlog`: not picked up. Use this for anything you have not refined yet.
- `Todo`: refined and small enough that an agent can attempt it. The worker poll tick picks these up. Name this state exactly `Todo`: the SPEC §8.2 blocker rule only gates issues whose state is literally `Todo`, so a renamed ready state silently disables dependency blocking (#739).
- `In Progress`: the agent has claimed an issue or you are iterating on it. Stays in `active_states` so re-runs after a push are allowed.
- `Human Review`: agent finished and opened a draft PR. Not in `active_states`. Read the diff yourself. Per SPEC §1, tracker updates belong on the agent/tool side, not in the worker scheduler.
- `Rework`: review found issues and you want another attempt. Keep `Rework` in `active_states` so the worker can pick the issue up again after the tracker state changes. The in-memory orchestrator state, not Postgres queue rows, is the scheduling authority for the running worker process.
- `Done` / `Canceled`: terminal. Listed under `tracker.terminal_states`.

Recommended `active_states` for this lifecycle (the code default is `[Todo, In Progress]`; `Rework` is an opt-in override):

```yaml
tracker:
  active_states:
    - Todo
    - In Progress
    - Rework
```

Rule of thumb: if you do not feel comfortable letting an agent open a draft PR for an issue right now, do not move it to `Todo`.

For GitHub dogfood, use the dedicated `aiops:ready` label as the ready gate
instead of a `Todo` state. Do not treat `open` or `priority:pN` labels as
readiness.

## Writing good issues

The issue title and description are passed to the runner via the `PROMPT.md` template (see `examples/WORKFLOW.md`). Vague issues produce vague diffs.

Checklist before moving an issue to `Todo`:

- One clear outcome. Not "improve logging" but "log tracker issue id in worker poll dispatch output".
- Concrete file or package hints. Mention paths like `internal/runner/shell.go` so the agent does not wander.
- Acceptance criteria as a bullet list. The verification section in `WORKFLOW.md` runs `go test ./...`, but tests do not catch design intent.
- Out-of-scope notes. Call out things you do not want touched (e.g. `infra/**`, `deploy/**`, `db/migrations/**`, `secrets/**`) directly in the issue / prompt so the agent self-limits; use `sandbox:` write restrictions for hard prevention.
- Size budget: keep the change small (aim for ≤12 files / ≤300 LOC as a review guideline). If a task realistically needs more, split it.
- Link to the relevant ADR or research doc when the task involves architecture decisions.
- Dependency class: mark the issue as a `hard dependency`, `soft overlap`, or
  `independent issue`. Do not move hard-dependent or soft-overlap work through
  the ready gate for unattended runs until the blocker is terminal.

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

The SPEC §10 runner (`internal/runner/codex_app_server.go`). It launches `codex app-server` once and drives a long-running JSON-RPC 2.0 session over stdio, running multiple agent turns inside one worker session bounded by `agent.max_turns` (SPEC §5.3.5) and, for fresh/continuation dispatches, the remaining D34 clean-turn budget. PROMPT.md seeds the first turn; combined stdio is captured to `.aiops/CODEX_APP_SERVER_OUTPUT.txt`.

Use when:

- you want the SPEC-aligned Codex runner — this is the default real runner.
- the task may need several agent turns within a single session.
- you have a Codex CLI session already authenticated locally.

```yaml
agent:
  default: codex-app-server
codex:
  command: codex app-server --config shell_environment_policy.inherit=all
```

The agent's sandbox/approval posture is set by `codex.thread_sandbox` (per-session) and `codex.turn_sandbox_policy` (per-turn; derived from `thread_sandbox` when unset — see DEVIATIONS.md D32). Use `workspace-write` on shared hosts and `danger-full-access` only on already-isolated workers (container, dedicated VM).

> The earlier non-SPEC `codex` (one-shot `codex exec`) runner was removed under [#541](https://github.com/xrf9268-hue/aiops-platform/issues/541); it drove the same agent as `codex app-server` in a strictly worse mode (no in-session `max_turns`).

### Reading codex output after a run

Each run writes `.aiops/CODEX_APP_SERVER_OUTPUT.txt` (combined stdout+stderr, capped at 1 MiB with a truncation footer when the cap fires). The `runner_end` task event payload also carries `output_head` (first 4 KiB), `output_tail` (last 4 KiB if non-overlapping), `output_bytes`, and `output_dropped` for at-a-glance triage from worker logs and runtime events without cloning the work branch.

### `claude`

Shell runner (`internal/runner/shell.go`) that invokes `claude.command` (default `claude`) via `sh -c` with PROMPT.md on stdin.

Use when:

- you prefer Claude as the coding agent, or want a second opinion when Codex produced a thin or wrong patch.
- you want Claude's tool use for a run.
- Note this runner is **one-shot**: it runs a single `sh -c` invocation per worker session, so multi-turn iteration relies on the orchestrator's continuation retries and D34 `agent.max_continuation_turns` budget, not an in-session turn loop like `codex-app-server`. To switch runner, set `agent.default` in `WORKFLOW.md`. Note: `agent.fallback` was removed in issue #40 — workflows that still carry the key now fail validation at load time.

```yaml
agent:
  default: claude
claude:
  command: claude
```

### Manual review (no runner)

Skip automation entirely when:

- the task touches sensitive areas (infra, deploy, migrations, secrets).
- it is a security-sensitive change or a data migration.
- requirements are still ambiguous. Use a planning issue instead and only move to `Todo` once the design is settled.
- the change is large enough that a draft PR would blow well past the ~12-file / ~300-LOC review guideline.

Decision shortcut:

```text
unsure or risky                       -> manual review, keep issue out of Todo
plumbing or smoke test                -> mock
real code change (SPEC default)       -> codex-app-server
prefer Claude / want a second opinion -> claude
```

`codex-app-server` is the SPEC §10 default real runner and handles the full
range of code changes — it drives a long-running Codex session of up to
`agent.max_turns` (default 20) back-to-back turns on one thread (SPEC §7.1),
also bounded by the remaining D34 clean-turn budget for fresh/continuation
dispatches. Pick `claude` when you want a different agent, not because the
change is larger.

## Handling failed tasks

The SPEC-aligned worker does not persist scheduler attempts in Postgres. It polls the tracker, records in-flight/completed/retry state in the in-memory orchestrator runtime, and starts with fresh scheduler state after restart. Before the first poll, startup reconciliation removes only deterministic workspaces whose issue identifiers the tracker confirms are terminal; unmatched workspaces remain untouched.

### Triage with the runtime status API

Use the worker-owned runtime status surface:

```bash
curl 'http://127.0.0.1:4000/api/v1/state'
```

For failed tasks, inspect process logs and task events emitted by
`worker.RunTask` such as `workflow_resolved`, `runner_start`, `runner_end`
(its `error` payload carries the failure reason), `run_phase_transition`, and
reconciliation events. The structured event log is the single source of truth
for why a run failed — the worker no longer writes a `.aiops/FAILURE.md`
post-mortem or `.aiops/CHANGED_FILES.txt` snapshot (#575).

### Common causes and fixes

- `repo.clone_url missing in WORKFLOW.md`: worker log line. Fix `WORKFLOW.md`, restart the worker.
- Verification command failed (`go test ./...` non-zero): read the `runner_end` event's `error` payload (and process logs) for the failure reason. Reproduce locally on the same branch.
- Change landed out of scope or oversized (touched an off-limits path, or the diff is too big for review): re-scope the task into a smaller issue, sharpen the prompt's scope guidance, or do it manually.
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
- the work has crossed into a sensitive (infra/deploy/migrations/secrets) area or grown well past the size guideline.

The platform is meant to save time on small, well-scoped tasks. When it stops doing that for a given issue, do not fight it.
