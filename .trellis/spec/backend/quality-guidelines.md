# Quality Guidelines

> Code quality standards for backend development.

---

## Overview

<!--
Document your project's quality standards here.

Questions to answer:
- What patterns are forbidden?
- What linting rules do you enforce?
- What are your testing requirements?
- What code review standards apply?
-->

(To be filled by the team)

---

## Forbidden Patterns

<!-- Patterns that should never be used and why -->

(To be filled by the team)

---

## Required Patterns

<!-- Patterns that must always be used -->

### Scenario: GitHub ruleset drift checks

#### 1. Scope / Trigger
- Trigger: adding or changing CI that verifies GitHub repository rulesets
  against committed governance JSON.

#### 2. Signatures
- Read live rulesets with `gh api repos/<owner>/<repo>/rulesets` and
  `gh api repos/<owner>/<repo>/rulesets/<ruleset_id>`.
- Store the source ruleset in `.github/governance/main-ruleset.json`.

#### 3. Contracts
- Workflow token permissions must stay read-only, normally
  `permissions: contents: read`.
- The workflow may pass `GH_TOKEN: ${{ github.token }}` to `gh api`.
- The comparison is a read-visible projection: exclude fields the default
  token cannot see, including `bypass_actors`, and exclude server-managed
  fields such as ids, timestamps, links, and source metadata.

#### 4. Validation & Error Matrix
- Missing `gh` or `jq` -> fail the workflow with an error annotation.
- Zero or multiple matching live rulesets -> fail and print the live candidates.
- Normalized live/source diff -> fail with an annotation on
  `.github/governance/main-ruleset.json` and print a unified diff.

#### 5. Good/Base/Bad Cases
- Good: scheduled/manual/push workflow that only calls read endpoints and fails
  on drift.
- Base: local fixture tests proving server-managed fields do not create false
  drift.
- Bad: a workflow that auto-applies ruleset JSON or carries a write-capable
  branch-protection credential.

#### 6. Tests Required
- Matching source/live fixtures pass after normalization.
- Drift fixture exits non-zero and includes a GitHub Actions annotation.
- Script/workflow contents are tested to prevent write-method or auto-apply
  regressions.

#### 7. Wrong vs Correct

Wrong:
```yaml
permissions:
  administration: write
```

Correct:
```yaml
permissions:
  contents: read
```

### Scenario: Trace Harness Evaluator-Result Artifacts

#### 1. Scope / Trigger
- Trigger: adding or changing trace harness report CLI flags or artifact
  schemas that emit or consume evaluator-result feedback.

#### 2. Signatures
- Report command emits evaluator results with:
  `python3 scripts/trace-harness-report.py --worker-log <log> --json-out <report> --evaluator-results-out <results>`.
- Report command consumes prior results with:
  `python3 scripts/trace-harness-report.py --worker-log <log> --prior-evaluator-results <results> --json-out <report>`.

#### 3. Contracts
- Result artifact envelope schema:
  `trace-harness-advisory-evaluator-results/v1`.
- Each `results[]` entry uses schema
  `trace-harness-advisory-evaluator-result/v1` with exactly
  `schema`, `evaluator_id`, `source_cluster_id`, `mode`, `signal`,
  `evidence_refs`, and `false_positive_notes`.
- Recurrence proposals must carry a stable marker:
  `trace-harness-recurrence:<source_cluster_id>:<evaluator_id>`.
- `recurrence_escalations[]` is the canonical recurrence output; the cluster
  proposal mirror may be omitted only when needed to preserve the cluster byte
  cap.
- The report command remains read-only: it does not open issues, post comments,
  mutate tracker state, rewrite harness files, or create CI/runtime/merge gates.

#### 4. Validation & Error Matrix
- Missing all report inputs -> fail with the existing missing-input error.
- `--prior-evaluator-results` path larger than the artifact byte cap -> fail
  before JSON parsing.
- Prior result artifact with wrong envelope schema -> fail and name the expected
  schema.
- Prior result artifact missing `results` -> fail rather than treating it as an
  empty prior artifact.
- Prior result entry missing required fields or list-typed refs/notes -> fail.
- Artifact cannot fit within the declared byte cap after trimming -> fail rather
  than writing an oversized artifact.
- Artifact cannot fit all evaluator-result records after trimming refs/notes ->
  fail rather than dropping records silently.
- Current and prior positive results for the same cluster -> emit a top-level
  recurrence escalation even when the cluster proposal mirror cannot fit.

#### 5. Good/Base/Bad Cases
- Good: two manual report runs where the first writes evaluator results and the
  second consumes them to render one recurrence escalation proposal.
- Base: no prior positive result for the current cluster, so no recurrence
  proposal is emitted.
- Bad: parsing issue comments or forge text to infer recurrence, or accepting a
  giant prior artifact before checking size.
- Bad: popping later `results[]` entries to satisfy the byte cap, which loses
  recurrence evidence for those clusters.
- Bad: dropping the only recurrence proposal for a high-volume cluster because
  `clusters[].proposals.recurrence_escalation` cannot fit under the cluster byte
  cap.

#### 6. Tests Required
- CLI fixture that writes evaluator results and asserts the exact record shape.
- CLI fixture that feeds prior results back into a later report and asserts the
  dedupe marker and forge comment.
- High-volume fixture that keeps the top-level recurrence proposal when the
  cluster mirror is omitted for byte-cap reasons.
- Oversized prior artifact fixture that proves the pre-parse size check and path
  masking.
- Redaction assertion that opaque payload text, tokens, and clone-URL userinfo
  do not enter result artifacts.

#### 7. Wrong vs Correct

Wrong:
```bash
python3 scripts/trace-harness-report.py \
  --worker-log worker.log \
  --prior-evaluator-results huge-untrusted.json
# then read huge-untrusted.json fully before checking size
```

Correct:
```bash
python3 scripts/trace-harness-report.py \
  --worker-log worker.log \
  --prior-evaluator-results bounded-results.json \
  --json-out report.json
# stat bounded-results.json first; reject files above max_artifact_bytes
```

---

## Testing Requirements

<!-- What level of testing is expected -->

When adding CI governance scripts, add local tests that execute the script
against fixtures instead of relying on YAML review alone.

---

## Code Review Checklist

<!-- What reviewers should check -->

(To be filled by the team)
