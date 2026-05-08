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
- `push` — git push outcome and changed file count
- `pr_created` — pull request creation (number, html_url) or failure
- `succeeded`, `failed_attempt` — terminal outcomes

The worker also writes the following artifacts under `.aiops/` in the
workspace so failed tasks can be inspected on disk:

- `PROMPT.md` — rendered prompt sent to the runner
- `TASK.md` — task description
- `RUN_SUMMARY.md` — high-level summary of the run, including changed files and verify status
- `CHANGED_FILES.txt` — newline-separated list of files the worker is committing
- `VERIFICATION.txt` — combined stdout+stderr of every verify command, with exit codes and durations
