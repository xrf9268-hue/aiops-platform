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
- `run_phase_transition` — SPEC §7.2 run-attempt phase transitions. Payloads
  carry `event`, `from`, and `to`; phase names are `PreparingWorkspace`,
  `BuildingPrompt`, `LaunchingAgentProcess`, `InitializingSession`,
  `StreamingTurn`, `Finishing`, `Succeeded`, `Failed`, `TimedOut`, `Stalled`,
  and `CanceledByReconciliation`.
- `session_started`, `startup_failed`, `turn_completed`, `turn_failed`,
  `turn_cancelled`, `turn_ended_with_error`, `turn_input_required`,
  `approval_auto_approved`, `unsupported_tool_call`, `tool_call_mutation`,
  `notification`, `other_message`, `malformed` — SPEC §10.4 app-server runtime
  vocabulary. The `codex-app-server` runner captures the observed protocol
  branches for this vocabulary, including auto-approved approvals, malformed
  protocol-like lines, and known JSON payloads that do not match a handled
  method. The worker forwards captured runtime events into the task event
  stream with their structured payloads. `tool_call_mutation` is emitted once
  per successful Linear GraphQL mutation dispatched through the agent-visible
  `linear_graphql` tool (SPEC §15.5 harness narrowing, #298); its payload
  carries the top-level mutation field name (`operation_field`) but never the
  query body or variables.
- `runner_start`, `runner_end`, `runner_timeout` — transitional worker runner
  timing and summary events retained for compatibility while the SPEC phase
  stream is adopted.
- `stalled` — emitted when the streaming runner exceeds its inactivity budget.
- `policy_violation` — worker policy rejected the run. Payload includes the
  violations, whether one retry remains, the repeated violation count, and the
  feedback file path when available.
- `policy_feedback_loaded`, `policy_feedback_read_error` — prior policy
  violation feedback was loaded into the next prompt, or the worker could not
  read that feedback file.
- `verify_start`, `verify_end` — transitional workflow verify command results.
- `push`, `pr_created`, `pr_reused` — legacy worker-side PR handoff events
  retained as constants for compatibility with older event streams; current
  SPEC-aligned worker success paths must not emit them because push and PR
  handoff are agent-side responsibilities per SPEC §1.
- `tracker_transition`, `tracker_transition_error`, `tracker_comment` —
  implementation extensions for operator visibility around tracker-side
  transitions/comments; tracker writes remain tool-driven rather than worker
  responsibilities.
- `succeeded`, `failed_attempt` — terminal outcomes

The worker also writes the following artifacts under `.aiops/` in the
workspace so failed tasks can be inspected on disk:

- `PROMPT.md` — rendered prompt sent to the runner
- `TASK.md` — task description
- `RUN_SUMMARY.md` — high-level summary of the run, including changed files and verify status
- `CHANGED_FILES.txt` — newline-separated list of files the worker is committing
- `VERIFICATION.txt` — combined stdout+stderr of every verify command, with exit codes and durations
