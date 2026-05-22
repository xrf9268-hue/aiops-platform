# WorkflowReloadFailed fingerprint dedupe — design

**Date:** 2026-05-22
**Issue:** [#239](https://github.com/xrf9268-hue/aiops-platform/issues/239)

## Problem

`WorkflowRuntime.ReloadOnce` emits `EventWorkflowReloadFailed` on every tick whenever the workflow file is present but invalid. Default tick = 1 s; an hour of bad config = 3600 identical emissions. The 200-entry runtime event ring (`internal/orchestrator/status.go`) evicts everything useful, structured-log sinks alert thousands of times, and the reload-loop wastes CPU re-running `Load + validate` for content that hasn't changed.

The fingerprint check at `workflow_runtime.go:108` only suppresses re-emission of `EventWorkflowReloaded` — it short-circuits when the **last successful** fingerprint matches the current file. When the file is invalid, no successful snapshot is stored, so the comparison never matches and the load+validate runs every tick.

## Decision

Track the most recent **failed** fingerprint separately. Suppress `EventWorkflowReloadFailed` and the load/validate work when the fingerprint hasn't changed since the last emission. Clear the failed-fingerprint state on the next successful load.

Treat fingerprint-extraction failures (file missing, EISDIR, permission denied) with a sentinel string so they dedupe the same way as load/validate failures.

### Why fingerprint-keyed and not time-rate-limited

Rate limiting (e.g. once per minute) would still emit many duplicates per day and gives no signal about whether the underlying file actually changed. Fingerprint-keying means **operators see exactly one event per distinct broken configuration** — the cardinality reflects real operator activity, not wall-clock time.

### Concurrency

`ReloadOnce` has exactly one production caller (`RunWorkflowReloadLoop` in `cmd/worker/main.go:206`), sequential. The new field is still guarded by a `sync.Mutex` for safety in tests (which call `ReloadOnce` directly from the test goroutine and may run with `-race`).

### Sentinel for read-error case

`workflowFileFingerprint` returns an error before any hex digest is computed when the file is missing/unreadable. We use the literal `"<workflow-read-error>"` as the fingerprint marker for this case. SHA-256 hex is `[0-9a-f]{64}`, so the angle-bracket sentinel cannot collide with a real fingerprint.

## What changes

| File | Change |
| --- | --- |
| `internal/orchestrator/workflow_runtime.go` | Add `mu sync.Mutex` + `lastFailedFingerprint string` fields on `WorkflowRuntime`. |
| `internal/orchestrator/workflow_runtime.go` (`ReloadOnce`) | After fingerprint read: if read fails, compare against sentinel and skip re-emit if matched; otherwise compare current file fingerprint against `lastFailedFingerprint` and skip both `workflow.Load` and re-emit when matched. On any successful path (snap match or new snap stored), clear `lastFailedFingerprint`. |
| `internal/orchestrator/workflow_reloader_test.go` | Add `TestWorkflowRuntimeReloadOnceDedupesIdenticalFailures`: write invalid workflow, call `ReloadOnce` six times, assert `count(EventWorkflowReloadFailed) == 1`; then restore validity and assert one `EventWorkflowReloaded`. Add a second test for missing-file dedupe. |

## Implementation sketch

```go
const workflowReloadReadErrorSentinel = "<workflow-read-error>"

type WorkflowRuntime struct {
    // ... existing fields ...
    mu                    sync.Mutex
    lastFailedFingerprint string
}

func (r *WorkflowRuntime) ReloadOnce(ctx context.Context) error {
    // (existing nil / SourceDefault checks unchanged)
    fingerprint, err := workflowFileFingerprint(r.path)
    if err != nil {
        r.mu.Lock()
        suppress := r.lastFailedFingerprint == workflowReloadReadErrorSentinel
        r.lastFailedFingerprint = workflowReloadReadErrorSentinel
        r.mu.Unlock()
        if !suppress {
            r.emit(ctx, task.EventWorkflowReloadFailed, ...)
        }
        return err
    }
    if snap := r.Current(); snap.Fingerprint != "" && snap.Fingerprint == fingerprint {
        r.clearLastFailedFingerprint()
        return nil
    }
    r.mu.Lock()
    if r.lastFailedFingerprint == fingerprint {
        r.mu.Unlock()
        return errWorkflowReloadDeduped
    }
    r.mu.Unlock()
    wf, err := workflow.Load(r.path)
    if err == nil && r.validate != nil {
        err = r.validate(wf.Path, wf.Source, wf.Config)
    }
    if err != nil {
        r.mu.Lock()
        r.lastFailedFingerprint = fingerprint
        r.mu.Unlock()
        r.emit(ctx, task.EventWorkflowReloadFailed, ...)
        return err
    }
    r.clearLastFailedFingerprint()
    r.current.Store(r.snapshotFromWorkflow(wf, fingerprint))
    r.emit(ctx, task.EventWorkflowReloaded, ...)
    return nil
}
```

`errWorkflowReloadDeduped` is a sentinel `errors.New("workflow reload deduped: identical to last-failed fingerprint")`. The single existing caller (`RunWorkflowReloadLoop`) does not switch on the error type, so the sentinel is purely diagnostic for tests.

## Non-goals

- Don't introduce a time-based rate-limiter — fingerprint dedupe gives exactly-one-per-distinct-failure cardinality, which is what operators want.
- Don't change `RunWorkflowReloadLoop` backoff behavior — the loop's interval is unchanged; the cost saving is per-tick (no `Load`/`validate` re-run on identical fingerprints).
- Don't expose `lastFailedFingerprint` on `WorkflowSnapshot` — it's internal state, not a snapshot property.

## Acceptance criteria

- [ ] Persistent invalid workflow emits exactly one `EventWorkflowReloadFailed` per distinct broken fingerprint.
- [ ] When the file is fixed, exactly one `EventWorkflowReloaded` fires and dedupe state clears.
- [ ] Missing-file failure also dedupes.
- [ ] Test simulates ≥6 ticks against an invalid file and asserts the count remains 1.

## Refs

- Issue: https://github.com/xrf9268-hue/aiops-platform/issues/239
- Code: `internal/orchestrator/workflow_runtime.go:96-122, 195-205`
- Existing test patterns: `internal/orchestrator/workflow_reloader_test.go:655-682` (single-failure emission), `:93-122` (success dedupe by fingerprint)
- SPEC §6.2 ("Invalid reloads MUST NOT crash the service; … emit an operator-visible error")
