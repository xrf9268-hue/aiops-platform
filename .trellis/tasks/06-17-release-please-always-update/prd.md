# Fix release-please stale release PR updates

## Goal

Ensure the release-please Release PR is automatically refreshed when `main`
advances, even if the newly merged commits do not change release notes. This
keeps release PRs compatible with the repository's strict "branch must be up to
date" required-status-check policy.

## Requirements

* Configure release-please to update existing release PRs with the latest base
  changes instead of only when the generated release notes change.
* Document the behavior in the CI runbook so future operators understand why
  non-releasable commits can still update the Release PR.
* Keep the change scoped to release automation configuration and documentation.

## Acceptance Criteria

* [ ] `release-please-config.json` enables the official `always-update`
  setting.
* [ ] `docs/runbooks/ci.md` describes the `always-update` behavior and its CI
  trade-off.
* [ ] The release-please config remains valid JSON.
* [ ] A focused diff review confirms no unrelated release or governance changes.

## Definition of Done

* Config and docs are updated.
* Focused validation commands pass.
* No unrelated files are modified, aside from Trellis task bookkeeping for this
  work.

## Technical Approach

Add the top-level release-please manifest config field
`"always-update": true`. The official schema defines this as a root option that
forces release PR updates with latest changes; release-please otherwise skips
updating an existing PR when its generated body is unchanged.

## Decision (ADR-lite)

**Context**: GitHub ruleset strict required checks block stale release PRs even
when all PR checks passed. release-please's default behavior leaves the Release
PR branch stale after non-releasable commits because the release notes body is
unchanged.

**Decision**: Enable release-please `always-update` at the root config level.

**Consequences**: Release PRs may run CI more often after docs/refactor/ci-only
main merges, but they should no longer require a manual "Update branch" click
solely to satisfy strict status checks.

## Out of Scope

* Changing required status-check strictness or GitHub ruleset policy.
* Manually updating PR #917 from this task.
* Changing release-please versioning, changelog sections, labels, or release
  branch layout.

## Technical Notes

* Impacted files: `release-please-config.json`, `docs/runbooks/ci.md`.
* Relevant governance: `.github/governance/main-ruleset.json` has
  `strict_required_status_checks_policy: true`.
* Research reference:
  `research/release-please-always-update.md`.
