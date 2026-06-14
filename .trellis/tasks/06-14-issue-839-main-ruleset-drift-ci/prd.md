# Issue 839: Main Ruleset Drift CI

## Goal

Add a read-only GitHub Actions drift check for the `main` branch ruleset so the
live GitHub ruleset cannot silently diverge from
`.github/governance/main-ruleset.json` after manual apply misses or UI edits.

## Requirements

* Fetch the live repository ruleset with GitHub's read-only API path and the
  default Actions token.
* Normalize the live response and committed source before comparison, dropping
  server-managed fields such as ids, timestamps, links, source metadata, and
  live-only empty defaults.
* Exclude `bypass_actors` from the read-only comparison because GitHub's API
  only returns that property to callers with write access to the ruleset.
* Diff the normalized JSON and fail with a GitHub Actions annotation when live
  settings diverge from the committed source.
* Keep the job read-only: no `PUT`, no auto-apply, no write-scoped token.
* Detect out-of-band drift, not only PR-time changes.

## Acceptance Criteria

* [ ] A CI workflow runs the drift check without write permissions.
* [ ] The checker compares read-visible live and committed ruleset settings
  after deterministic normalization.
* [ ] Drift produces a failing exit code and an annotation pointing at
  `.github/governance/main-ruleset.json`.
* [ ] Tests cover matching normalized rulesets, drift failure, and the
  no-auto-apply invariant.
* [ ] The PR body closes #839 and documents that auto-apply remains out of scope.

## Definition of Done

* Node script tests pass for the GitHub workflow/checker.
* The checker is run locally against the current live ruleset.
* Standard repository gates are run as far as practical before push.
* PR is opened with the issue acceptance criteria and verification evidence.

## Technical Approach

Implement a small `.github/scripts/check-ruleset-drift.sh` script and a
dedicated `.github/workflows/ruleset-drift.yml` workflow. The workflow should
run on `push` to `main`, `workflow_dispatch`, and a schedule so UI drift is
detected even when the committed file does not change. The script should use
`gh api` for live reads and `jq -S` for normalized projections. Tests should
exercise the script with fixture JSON so drift behavior is verified without
network access.

## Decision (ADR-lite)

Context: The live ruleset is server-side state and is applied manually. An
auto-apply workflow would require a write credential capable of rewriting branch
protection.

Decision: Add a read-only drift detector, not an auto-apply workflow.

Consequences: A legitimate ruleset source change may produce a failing main
drift check until an admin imports the updated JSON. That is the intended alert:
it preserves least privilege while making the manual step visible.

## Out of Scope

* Auto-applying or auto-reverting the ruleset.
* Managing repository-level merge settings as code.
* Making the drift check a required branch-protection status check in this PR.

## Technical Notes

* Issue: https://github.com/xrf9268-hue/aiops-platform/issues/839
* Owner probe: no linked/open PR and no remote `fix/839-*` branch at start.
* Live ruleset id observed during planning: `17166171` (`main merge governance`).
* Official docs checked: GitHub REST "Get a repository ruleset" requires only
  Metadata read, while "Update a repository ruleset" requires Administration
  write; the same page notes `bypass_actors` is only returned to callers with
  write access to the ruleset. GitHub Actions docs recommend limiting
  `GITHUB_TOKEN` to the minimum required permissions, and workflow command docs
  define the `::error file=...::...` annotation format.
* Relevant docs: `AGENTS.md`, `.claude/skills/handle-issue/SKILL.md`,
  `docs/runbooks/pr-review-merge-protocol.md`, `.github/governance/README.md`.
* Trellis context: `.trellis/spec/backend/agent-workflow-guidelines.md`,
  `.trellis/spec/guides/code-reuse-thinking-guide.md`.
