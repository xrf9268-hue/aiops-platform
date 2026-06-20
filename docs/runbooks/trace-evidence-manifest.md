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

## What is captured (metadata first)

The output uses schema `trace-evidence-manifest/v1`.

- Per input: the source log path, byte count, and `sha256` digest.
- Per run (keyed by `task_id`, falling back to the issue id): the affected
  issue, issue identifier, PR, run, and session ids.
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
  and, if the affected-id arrays alone would still exceed the cap, those arrays
  are truncated too with the dropped counts recorded under `affected.omitted`.
  Larger artifacts are referenced by `path:line` plus the input digest and byte
  count, never inlined.
- Manifests are operator-owned artifacts. If committed, keep them under
  `docs/validation/` (or another explicit evidence directory) with a short
  retention rationale; otherwise treat them as local, rotation-bounded scratch
  output. The manifest is evidence for harness improvement, not scheduler state,
  and must never become restart/recovery state.

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
