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

---

## Testing Requirements

<!-- What level of testing is expected -->

When adding CI governance scripts, add local tests that execute the script
against fixtures instead of relying on YAML review alone.

---

## Code Review Checklist

<!-- What reviewers should check -->

(To be filled by the team)
