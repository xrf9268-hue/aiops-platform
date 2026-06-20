# Trace evidence manifest

`scripts/trace-evidence-manifest.py` turns worker logs the operator already
retained into a durable, redacted, metadata-first evidence manifest that the
trace harness report and later trace-driven milestones can consume repeatably.
It is the Milestone 5 (#952) durable input named in
[`docs/design/trace-driven-harness-improvement.md`](../design/trace-driven-harness-improvement.md)
(§"Redaction, retention, and bounds"): the design's pre-specified "bounded
per-run evidence manifest", explicitly **not** a trace database, queue, metrics
store, or scheduler/recovery state.

## Why it exists

Worker events go to stderr, `/api/v1/state` is in-memory and restart-local, and
workspace `.aiops/*` artifacts live only while the workspace exists, so today's
evidence is durable only if the operator keeps logs. Learning from real runs
needs a dependable, bounded input instead of relying on whatever stderr was
captured. This tool normalizes retained logs into that input as a standalone,
SPEC-safe step: it does not add any worker-written capture, so restart recovery
stays tracker/filesystem-driven (#73/#407), and it does not mutate tracker
state, open PRs, or persist scheduler state.

```bash
python3 scripts/trace-evidence-manifest.py \
  --worker-log /path/to/worker.log \
  --json-out /tmp/trace-evidence-manifest.json
```

Multiple `--worker-log` flags are allowed. The command writes compact JSON, so
for local inspection pipe it through `python3 -m json.tool` or `jq`.

Byte-identical inputs are collapsed by their `sha256` digest before folding, so
passing the same log (or, for the report, the same manifest) twice does not
double events or inflate the per-class affected summary (#961). Two *distinct*
inputs whose contents **overlap** (for example, log-rotation slices that share
lines) keep different digests and are not deduped: their kept affected-id lists
still merge correctly, but the per-class `omitted` counts — bounded counts whose
dropped id values are gone — are summed and may over-state recurrence. Prefer
**non-overlapping** log slices.

## What is captured (metadata first)

The output uses schema `trace-evidence-manifest/v2`.

- Per input: the source log path, byte count, and `sha256` digest.
- Per run (keyed by `task_id`, falling back to the issue id): an
  `affected_by_class` map from each resolved failure class to the affected
  issue, issue identifier, PR, run, and session ids seen for that class. The
  per-class summary is accumulated from every event **before** the per-run event
  cap drops evidence, so a dropped event's ids survive under its own class and
  the report consumer folds them into the matching cluster without ever
  mis-attributing a dropped event to a different class.
- Per event: `source`, the masked source reference (`path:line`), event `kind`,
  the resolved failure `class` (so consumers re-cluster without re-parsing the
  redacted excerpt), the redacted structured-prefix `excerpt`, and trusted
  top-level scalar `metadata` (timestamp, byte counts, exit code, method, model,
  and similar worker/tracker-generated scalars).

## What is omitted

Parsing, redaction, and bounds are reused verbatim from
[`trace-harness-report.py`](trace-harness-report.md), so the manifest omits
exactly what the report omits:

- The entire `payload=map[...]` region is replaced with
  `payload=[redacted-payload]`, so `output_head`, `output_tail`, `error`,
  `arguments`, `arguments_raw`, `raw`, and `params` are never reproduced. Only
  trusted scalar metadata before the first opaque payload key is kept.
- Clone-URL userinfo is masked with `workflow.MaskCloneURL` and token-like
  values are replaced with `[redacted-token]`.
- It does not parse GraphQL or other free-form text, and it is **not a new
  evidence source**: it durably captures the worker events the report already
  supports, not tracker, PR, CI, or review evidence.

## Retention and bounds

- Evidence is bounded at **64 KiB per run** and each run group at the
  cluster-equivalent **256 KiB** cap, matching the report's per-cluster bounds on
  both axes. Whole events are dropped past the cap (reported as `dropped_events`)
  and, if the per-class `affected_by_class` arrays alone would still exceed the
  cap, those arrays are truncated too — the longest first — with the dropped
  counts recorded per class under `affected_by_class.<class>.omitted`. Larger
  artifacts are referenced by `path:line` plus the input digest and byte count,
  never inlined.
- Manifests are operator-owned artifacts. If committed, keep them under
  `docs/validation/` (or another explicit evidence directory) with a short
  retention rationale; otherwise treat them as local, rotation-bounded scratch
  output. The manifest is evidence for harness improvement, not scheduler state,
  and must never become restart/recovery state.

### Capped-run affected ids: faithful per-class recovery

When a single run's events exceed the 64 KiB event cap, the dropped events'
issue/session/PR ids are still recovered on the report round trip, attributed to
their own failure class (#958). Because `affected_by_class` is partitioned by the
class resolved at parse time, the consumer folds each class's summary into the
matching cluster, so a dropped `turn_input_required` id lands in an
`input-required` cluster — never in a retained `runner-timeout` one. When *every*
event of a class was dropped, the report still creates an **evidence-less**
cluster for that class carrying only the recovered affected ids. It contributes
no evidence-derived occurrences but still carries the summary's byte-cap
`omitted` counts, so its recurrence signal — `candidate-only` or
`positive-recurring` — matches whatever the raw `--worker-log` path reports for
the same class; it is never a *false* escalation, because both paths count the
same distinct trimmed ids. Capped and non-capped runs therefore round-trip the
same per-class affected ids the raw `--worker-log` path reports.

## How it is consumed

- The **trace harness report** consumes it directly:

  ```bash
  python3 scripts/trace-harness-report.py \
    --evidence-manifest /tmp/trace-evidence-manifest.json \
    --json-out /tmp/trace-report.json
  ```

  The report re-clusters the manifest's per-run events by failure class with the
  same classifier it applies to raw worker logs, and records the manifest as an
  `evidence_manifest` input. A manifest and raw `--worker-log` files may be
  passed together.
- **Milestone 6** (#953, the ratchet closure) reviews the report clusters and
  proposals produced from these manifests to confirm whether each recurring
  failure class became a reviewed harness change, a regression check, or a
  documented known limitation.
- **Milestone 7** (#951, the later scheduled driver) can run this normalization
  on a cadence to keep a rolling, bounded manifest set as its durable input,
  instead of depending on ad hoc retained stderr.
