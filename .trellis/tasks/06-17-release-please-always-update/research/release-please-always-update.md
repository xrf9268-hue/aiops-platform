# release-please always-update research

## Question

Should this repository enable release-please `always-update` so Release PRs do
not remain stale when `main` receives non-releasable commits?

## Evidence

* PR #917 was open with `autorelease: pending`, head
  `release-please--branches--main`, and merge state `BEHIND`.
* The release-please workflow run after #918 succeeded and logged:
  `PR https://github.com/xrf9268-hue/aiops-platform/pull/917 remained the same`.
* #918 was a `refactor:` commit. release-please's README describes releasable
  units as `feat`, `fix`, and `deps`; `chore` and `build` are explicitly not
  releasable units, and `refactor` is not listed unless it carries a breaking
  change.
* release-please v17.6.0 implements the default update path as "only update an
  existing pull request if it has release note changes"; if the existing PR body
  equals the newly generated body, it logs `remained the same` and returns
  without pushing updates.
* The release-please config schema defines top-level `always-update` as:
  "Always update the pull request with the latest changes. Defaults to
  `false`."
* This repository's committed and live main ruleset has
  `strict_required_status_checks_policy: true`, so an otherwise green Release
  PR is still blocked when its branch is behind `main`.

## Conclusion

Enable top-level `"always-update": true` in `release-please-config.json`. This
is the official release-please setting that matches the repository's strict
required-check policy. The trade-off is extra Release PR CI after non-user-facing
commits, which is preferable to manual branch updates for every stale release
PR.
