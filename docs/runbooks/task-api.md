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
