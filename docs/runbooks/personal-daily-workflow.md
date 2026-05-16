# Personal daily workflow

This runbook describes how to use `aiops-platform` day to day so it stays a useful tool instead of another system to babysit.

The defaults below match `examples/WORKFLOW.md` and the active state list hardcoded in `internal/workflow/config.go`.

## Linear states

The Linear poller in `cmd/linear-poller` only enqueues issues whose state name appears in `tracker.active_states` of your `WORKFLOW.md`. Use a simple lifecycle:

```text
Backlog -> AI Ready -> In Progress -> Human Review -> Rework -> Done
                                                  \-> Canceled
```

Per-state rules:

- `Backlog`: not picked up. Use this for anything you have not refined yet.
- `AI Ready`: refined and small enough that an agent can attempt it. The poller picks these up.
- `In Progress`: the agent has claimed an issue or you are iterating on it. Stays in `active_states` so re-runs after a push are allowed.
- `Human Review`: agent finished and opened a draft PR. Not in `active_states`. Read the diff yourself. Per SPEC §1, the agent moves the issue here through its tool surface; the worker does not write tracker state on completion.
- `Rework`: review found issues and you want another attempt. Moving the Linear issue into `Rework` re-enqueues a fresh task automatically. The poller composes `source_event_id` as `<issue.ID>|rework|<issue.updatedAt>` whenever the state is `Rework`, so each transition into Rework is a brand-new dedupe key and Postgres INSERTs a new task row. While the issue stays parked in Rework, repeated polls reuse the same `updatedAt` and dedupe (no enqueue loop). Non-Rework states still use plain `issue.ID`, so `AI Ready` -> `In Progress` iteration on a single task is unchanged. See `cmd/linear-poller/main.go` (`sourceEventID`) and `internal/queue/postgres.go` (`Enqueue`).
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

- One clear outcome. Not "improve logging" but "log request id in `cmd/trigger-api` access log".
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

Runners are selected by the `agent.default` field in `WORKFLOW.md` and resolved in `internal/runner/runner.go`. Valid values: `mock`, `codex`, `claude`. Per-task overrides can be sent via the `model` field on the manual task API.

### `mock`

What it does: writes a stub file under `.aiops/<task-id>.md` and returns success. No model call. See `internal/runner/mock.go`.

Use when:

- you just changed `WORKFLOW.md`, queue plumbing, or PR handoff and want to confirm the loop end-to-end.
- you are onboarding a new repository and want a safe first PR before pointing a real model at it.
- the model API is rate-limited or down and you still want to verify queue and worker behavior.

```yaml
agent:
  default: mock
```

### `codex`

Profile-driven runner (`internal/runner/codex.go`) that invokes the codex CLI with sandbox/approval flags chosen by `codex.profile`. PROMPT.md is piped on stdin; output is captured to `.aiops/CODEX_OUTPUT.txt`. The `custom` profile falls back to `sh -lc <codex.command>` (still stdin-fed).

Use when:

- the task is a focused change inside one or two files.
- you have a Codex CLI session already authenticated locally.
- you want to compare a Codex result against a Claude result for the same issue.

```yaml
agent:
  default: codex
codex:
  command: codex exec
```

**Profiles** select how the runner invokes codex:

- `safe` (default): builds `codex exec --full-auto --skip-git-repo-check --cd <workdir> -o <workdir>/.aiops/CODEX_LAST_MESSAGE.md` from argv (no shell). PROMPT.md is piped on stdin. `--full-auto` is codex's documented shorthand for `--sandbox workspace-write --ask-for-approval=never`.
- `bypass`: same shape but with `--dangerously-bypass-approvals-and-sandbox`. Use only when the worker host is already isolated (container, dedicated VM); the flag turns codex's own sandbox off.
- `custom`: runs the literal `codex.command` via `sh -lc` with PROMPT.md on stdin. Note the change from earlier versions: the runner no longer appends `< .aiops/PROMPT.md` to the command — your command must consume stdin (which `codex exec` does by default when no positional prompt is given).

### Reading codex output after a run

Each codex run writes `.aiops/CODEX_OUTPUT.txt` (combined stdout+stderr, capped at 1 MiB with a truncation footer when the cap fires) and reads `.aiops/CODEX_LAST_MESSAGE.md` (codex's own `-o` artifact) as the run summary. The `runner_end` task event payload also carries `output_head` (first 4 KiB), `output_tail` (last 4 KiB if non-overlapping), `output_bytes`, and `output_dropped` for at-a-glance triage from `/v1/tasks/<id>/events` without cloning the work branch.

### `claude`

Same shell runner, invokes `claude.command` (default `claude`).

Use when:

- the task touches several files or needs more reasoning across a package.
- you want richer tool use during the run.
- Codex produced a thin or wrong patch and you want a second opinion. To switch runner you set `agent.default` in `WORKFLOW.md`, or pass `model` per task via `POST /v1/tasks`. Note: `agent.fallback` was removed in issue #40 — workflows that still carry the key now fail validation at load time.

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
small, well-scoped code change   -> codex
multi-file or reasoning-heavy    -> claude
```

## Handling failed tasks

A task is `failed` only after `attempts` reaches `max_attempts`. Until then, a failing attempt sets the status back to `queued` with a 60s backoff (`internal/queue/postgres.go`). Status values are defined in `internal/task/task.go`: `queued`, `running`, `succeeded`, `failed`.

### Triage with the debugging API

See `docs/runbooks/task-api.md` for full details. Quick commands:

```bash
curl 'http://localhost:8080/v1/tasks?status=failed'
curl 'http://localhost:8080/v1/tasks/<task-id>'
curl 'http://localhost:8080/v1/tasks/<task-id>/events'
```

`/events` is the most useful endpoint. Look for `enqueued`, `claimed`, `failed_attempt`, `succeeded`, `failed` in order, and read the messages.

### Common causes and fixes

- `repo.clone_url missing in WORKFLOW.md`: poller log line. Fix `WORKFLOW.md`, restart the poller.
- Verification command failed (`go test ./...` non-zero): read `.aiops/RUN_SUMMARY.md` in the work branch if the runner produced one. Reproduce locally on the same branch.
- Policy violation (deny path or size cap): re-scope the task into a smaller issue, or do it manually.
- Runner command not found: confirm `codex.command` or `claude.command` resolves on the worker host. The shell runner uses `sh -lc`, so PATH must be set in the worker's login shell.
- Empty diff: agent decided nothing to do. Tighten the issue body, then move it to `Rework`. The state change alone re-enqueues a fresh task (the poller keys Rework by `issue.ID|rework|updatedAt`). `/ai-run` on Gitea and `POST /v1/tasks` are still available as fallbacks if the poller is paused.

### Re-running a failed task

Three options, in order of preference:

1. Move the Linear issue to `Rework`. The poller composes `source_event_id` as `<issue.ID>|rework|<updatedAt>` for the Rework state, so the transition INSERTs a new task row instead of deduping against the original. Subsequent polls while the issue stays in Rework reuse the same `updatedAt` and stay deduped, so you do not get an enqueue loop.
2. Re-trigger from Gitea by commenting `/ai-run` on the issue. The trigger API uses the Gitea delivery ID (or `comment-<id>`) as `source_event_id`, so each comment produces a new queued task. Useful when you want to retry without changing Linear state.
3. Manual enqueue against the trigger API (each call defaults `source_event_id` to `manual-<unix-nanos>`, so it is always fresh):

   ```bash
   curl -X POST http://localhost:8080/v1/tasks \
     -H 'Content-Type: application/json' \
     -d '{
       "repo_owner": "your-user",
       "repo_name": "your-repo",
       "clone_url": "git@gitea.local:your-user/your-repo.git",
       "base_branch": "main",
       "title": "retry: <original title>",
       "description": "...",
       "model": "claude",
       "priority": 50
     }'
   ```

   Set `model` to switch runner for this attempt only.

### When to give up on automation

Mark the issue back to `Backlog` or `Human Review` and finish it by hand if any of these are true:

- two runner attempts produced wrong or empty diffs.
- the failure mode is unclear after reading `/v1/tasks/<id>/events`.
- the work has crossed into a `deny_paths` area or grown past the size caps.

The platform is meant to save time on small, well-scoped tasks. When it stops doing that for a given issue, do not fight it.
