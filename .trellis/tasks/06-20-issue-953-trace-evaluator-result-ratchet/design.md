# Design

## Boundary

The implementation stays in the trace-report tooling and docs:

- `scripts/trace-harness-report.py`
- `scripts/trace_harness_report_docs_test.go`
- `docs/runbooks/trace-harness-report.md`
- `docs/runbooks/trace-evidence-manifest.md` if the M6 relationship needs a
  small clarification

It must not touch `internal/worker`, `internal/orchestrator`, tracker clients,
CI required checks, or merge policy.

## Data Flow

1. The operator runs `trace-evidence-manifest.py` or provides retained worker
   logs.
2. `trace-harness-report.py` builds `trace-harness-report/v3` clusters exactly
   as today.
3. The report renderer derives one evaluator-result record per advisory
   evaluator candidate:
   - `schema`: `trace-harness-advisory-evaluator-result/v1`
   - `evaluator_id`
   - `source_cluster_id`
   - `mode`: `report-only`
   - `signal`: candidate-only or positive-recurring-cluster
   - `evidence_refs`: bounded, redacted evidence refs
   - `false_positive_notes`: bounded false-positive expectations
4. A new explicit output path persists those records as a JSON artifact with
   artifact metadata, inputs, bounds, and records. The individual records keep
   the declared shape; the envelope is the reviewable durable artifact.
5. A later report run accepts prior result artifacts. When the same cluster has
   a prior positive signal and the current run is positive again, the report
   attaches a recurrence escalation proposal with a stable provenance marker.
6. The proposal includes ready-to-use forge/agent action text for reopening or
   bumping the tracking issue, but the command remains read-only.

## Idempotency Contract

Use a stable marker derived only from the structured cluster/evaluator ids:

`trace-harness-recurrence:<source_cluster_id>:<evaluator_id>`

Generated escalation text includes this marker so a human, `gh` workflow, or
future M7 driver can search comments/issues and skip duplicates. The report does
not parse existing forge comments in M6.

## Redaction and Bounds

Evaluator results are derived only from already-redacted cluster evidence and
candidate false-positive text. Evidence refs are capped with the existing
proposal evidence limit. Field values are truncated with existing bounded-field
helpers where text can grow. The persisted artifact must stay below the existing
cluster-equivalent cap for current supported failure classes.

## Tradeoffs

- The report produces an escalation proposal, not an automatic issue mutation.
  This preserves SPEC section 1 and keeps the M7 scheduled driver as the later
  automation layer.
- The result artifact has a metadata envelope even though each record has the
  exact M4-declared shape. The envelope is needed for input digests, bounds, and
  reviewability without overloading the per-record schema.
- Consuming only prior result artifacts avoids parsing natural-language issues
  or comments. That means M6 can prove recurrence and idempotent proposal
  generation, while M7 later owns actual dedup search against forge state.

## Compatibility

Existing `trace-harness-report.py --worker-log/--evidence-manifest --json-out`
behavior remains unchanged unless the new flags are provided. No schema bump is
needed for the main report unless a new optional top-level recurrence field is
added; if added, docs and tests must pin it.
