# Issue #953: trace evaluator result ratchet

## Goal

Close issue #953 by making the trace harness report emit, persist, and consume
advisory evaluator results so recurring failure classes are re-surfaced as
reviewable forge/agent follow-through proposals instead of being re-diagnosed
from scratch.

## Requirements

- Emit `trace-harness-advisory-evaluator-result/v1` records from report-only
  advisory evaluator candidates, using the shape already declared by M4:
  `schema`, `evaluator_id`, `source_cluster_id`, `mode`, `signal`,
  `evidence_refs`, and `false_positive_notes`.
- Persist emitted results as bounded, redacted, reviewable JSON artifacts that
  reuse the Trace L4 M5 evidence-manifest redaction and byte-bound assumptions.
- Consume prior result artifacts on a later manual report run and surface stable
  recurrence for the same cluster as an idempotent escalation proposal.
- Keep all escalation writes outside the worker: the report may generate
  forge/agent action text and a dedupe marker, but `gh` or a coding-agent
  workflow performs any issue reopen/comment action explicitly.
- Preserve redaction and retention limits: no raw prompts, agent streams,
  GraphQL payloads, forge comments, full logs, tokens, or unmasked clone URLs.
- Keep advisory evaluators non-blocking; do not introduce CI, runtime, merge, or
  worker-side gates.
- Update docs so operators understand the emit -> persist -> consume cycle,
  false-positive handling, recurrence dedupe, and gate-promotion evidence bar.

## Guardrail

- Required behavior: produce evaluator-result artifacts, read prior artifacts,
  and generate reviewable recurrence escalation proposals for stable recurring
  clusters.
- Negative constraints: do not store unbounded text, parse arbitrary natural
  language or forge comments as a machine contract, mutate tracker state from the
  worker/report command, rewrite harness files automatically, or promote an
  evaluator to a gate.
- Opaque boundaries: worker payload text, prompt/model output, forge comments,
  GraphQL/CI logs, and protocol text remain opaque unless an already-supported
  structured report/manifest field exposes bounded metadata.
- Design challenge verdict: recurrence can be proven with the structured result
  schema plus cluster ids and bounded evidence refs. No grammar/parser for
  arbitrary text is needed.

## Acceptance Criteria

- [ ] Advisory evaluator results are emitted in the declared
  `trace-harness-advisory-evaluator-result/v1` shape and covered by fixtures.
- [ ] Results are persisted as bounded, redacted artifacts; secret and byte
  bounds are tested and mutation-checked.
- [ ] A subsequent report run consumes prior results and escalates a
  stably-recurring cluster idempotently, without requiring the M7 driver.
- [ ] Evaluators remain non-blocking; no merge, runtime, CI, or worker gate is
  introduced.
- [ ] Docs explain the cycle, false-positive handling, and evidence bar for a
  later gate-promotion decision.

## Notes

- Issue #953 labels: `area:observability`, `area:testing`, `area:workflow`,
  `type:feature`, `priority:p2`.
- Owner probe on 2026-06-20: no open PR and no remote `fix/953-*` branch; free
  to start from `origin/main`.
- SPEC evidence: SPEC section 1 keeps Symphony as scheduler/runner and tracker
  reader; ticket writes are agent/runtime-tool actions. SPEC section 13 requires
  operator-visible observability but does not require a durable trace DB.
- Upstream Elixir has scheduler/tracker/read-side orchestration and no worker
  evaluator gate or post-turn tracker write phase.
