# Implementation Plan

## Checklist

- [x] Add evaluator-result constants and rendering helpers in
  `scripts/trace-harness-report.py`.
- [x] Add CLI support for explicit result persistence, likely
  `--evaluator-results-out <path>`.
- [x] Add CLI support for consuming prior result artifacts, likely
  `--prior-evaluator-results <path>` repeatable.
- [x] Add recurrence escalation proposal generation for current-positive +
  prior-positive matching cluster ids, including a stable dedupe marker.
- [x] Add fixture tests for:
  - declared record shape
  - persisted artifact metadata and bounded/redacted contents
  - prior result consumption and idempotent recurrence proposal
  - non-blocking evaluator execution fields
  - secret and byte-bound behavior
- [x] Update trace harness runbooks to document emit -> persist -> consume,
  false-positive notes, dedupe marker, and gate-promotion evidence.
- [x] Run focused tests.
- [x] Commit, mutation-check the committed artifact, then run required gates.

## Validation

Focused during implementation:

```bash
go test -run 'TestTraceHarnessReport|TestTraceEvidenceManifest' -count=1 ./scripts
```

Full gate before PR:

```bash
gofmt -l $(git ls-files '*.go')
go mod tidy && git diff --exit-code -- go.mod go.sum
go vet ./...
go test -race -covermode=atomic ./...
go build ./cmd/worker ./cmd/tui
```

Mutation checks after commit:

- Break the evaluator-result schema string or omit `evidence_refs`; focused
  tests must fail.
- Break redaction/bounds by allowing an oversized or secret-bearing result
  field; focused tests must fail.
- Break recurrence matching or marker generation; focused tests must fail.

## Review Notes

- First reviewer brief must include the guardrail from `prd.md`.
- Classify findings as algorithm/SPEC boundary, cross-module consistency, Go
  runtime hardening, placebo test, or wrong-side-of-boundary.
- PR body must include `Closes #953`, issue acceptance mapping, verification,
  mutation checks, and size-gate state.
