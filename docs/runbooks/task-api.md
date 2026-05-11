# Task Debugging API

The trigger API exposes local task inspection endpoints for development and smoke testing.

These endpoints are currently unauthenticated and are intended for local development or trusted internal networks only. Do not expose them directly to the public internet.

## List Tasks

```bash
curl 'http://localhost:8080/v1/tasks?status=queued'
```

The `status` query parameter is optional. Common values are `queued`, `running`, `succeeded`, and `failed`.

## Get Task

```bash
curl 'http://localhost:8080/v1/tasks/tsk_example'
```

Returns the task record as JSON, including repository fields, branch fields, model, priority, attempts, and timestamps.

## Get Task Events

```bash
curl 'http://localhost:8080/v1/tasks/tsk_example/events'
```

Returns task events in creation order. Use this after a manual enqueue or webhook run to inspect enqueue, claim, retry, and completion progress.

The worker emits the following per-stage events with structured `payload`
context (durations in `duration_ms`, command summaries, error excerpts) so a
failed run can be reconstructed from the event timeline alone:

- `enqueued`, `claimed` — queue lifecycle
- `runner_start`, `runner_end` — agent runner timing and summary
- `verify_start`, `verify_end` — workflow verify command results
- `push` — git push outcome and changed file count. Retries push the work
  branch with `--force-with-lease` so an earlier attempt's tip is overwritten
  cleanly instead of failing as non-fast-forward.
- `pr_created` — pull request creation (number, html_url) or failure
- `pr_reused` — emitted instead of `pr_created` when the work branch already
  has an open PR from a previous attempt; the worker reuses it (number,
  html_url, title) and skips the create call so retries do not duplicate PRs
- `tracker_transition` — Linear issue successfully moved to the configured
  target state (in-progress on claim, human-review on PR open, rework on
  failure); payload carries `issue_id`, `target_state`, and `reason`
- `tracker_transition_error` — issue move or fallback comment failed at the
  Linear API; payload includes the error excerpt so the task can still
  complete without aborting on a tracker hiccup
- `tracker_comment` — failure comment posted as a fallback after a rework
  transition failed, so the human still has visibility on the issue
- `succeeded`, `failed_attempt` — terminal outcomes

The worker also writes the following artifacts under `.aiops/` in the
workspace so failed tasks can be inspected on disk:

- `PROMPT.md` — rendered prompt sent to the runner
- `TASK.md` — task description
- `RUN_SUMMARY.md` — high-level summary of the run, including changed files and verify status
- `CHANGED_FILES.txt` — newline-separated list of files the worker is committing
- `VERIFICATION.txt` — combined stdout+stderr of every verify command, with exit codes and durations
