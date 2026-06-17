# CI/CD runbook

This repository uses GitHub Actions for CI and tag-based release publishing.

## Workflows

### CI

File:

```text
.github/workflows/ci.yml
```

Triggers:

- push to `main`
- pull request targeting `main`
- manual `workflow_dispatch`
- reusable `workflow_call` from release publishing

Checks:

- checkout with read-only repository permissions
- setup Go from `go.mod`
- Go module download
- standalone `go vet ./...` in the `Security and supply-chain` job
- `gofmt` check
- blocking `golangci-lint` gate for all enabled linters in one pass:
  `contextcheck`, `errcheck`, `errorlint`, `funlen`, `gocognit`, `gocritic`,
  `govet`, `ineffassign`, `revive`, `staticcheck`, `unparam`, and `unused`
- Trellis script regression tests:
  `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.trellis/scripts python3 -m unittest discover -s .trellis/scripts/tests -p 'test_*.py'`
- `go mod tidy` check
- uncached production Go file-size baseline check:
  `go test -run '^TestProductionGoFilesStayWithinSizeBudget$' -count=1 ./scripts`
- `go test -race -covermode=atomic ./...`
- build `worker` and `tui`
- upload short-lived CI binaries
- govulncheck
- e2e Gitea mock loop
- Docker image build validation
- blocking Trivy image scan for fixed CRITICAL/HIGH vulnerabilities
- CycloneDX SBOM upload

The Trivy image scan uses `ignore-unfixed: true` and `exit-code: "1"`, so it
blocks only CRITICAL/HIGH findings that already have an upstream fix. If this
gate fails on a package inherited from the Debian base image, first rebuild with
`docker build --pull --no-cache --tag aiops-platform:local .` to force a fresh
`apt-get update && apt-get upgrade` layer. Update the Dockerfile or base image
only if the rebuilt image still contains a fixed finding. Do not add a
`.trivyignore` entry for a vulnerability that the package manager can already
fix.

The `golangci-lint` gate runs as a single blocking pass over every enabled
linter, including the `funlen`/`gocognit` complexity budgets (AGENTS.md rule 7).
A new oversized / over-complex non-test function — or in-place growth of an
un-annotated one — fails CI. The existing complexity baseline is grandfathered
in-line with per-function `//nolint:gocognit[,funlen] // baseline (#521)`
directives (removed as #521 decomposes each), not by a report-only step; test
files are exempt via `.golangci.yml`. Its `govet` analyzer is separate from the
standalone `go vet ./...` step in the `Security and supply-chain` job; keep both
listed when describing the CI gate. Configuration, action, or runtime failures
also fail the workflow.

The file-size budget is enforced by `scripts/file_size_budget_test.go` through
an explicit `-count=1` CI step before the normal Go test gate. The uncached
focused step matters because the test discovers tracked files with `git
ls-files`, so package test caching could otherwise miss a newly added oversized
production file. Non-test, non-generated Go files must stay at or below 800
lines unless they are in the exact oversized-file baseline. If an existing
oversized file shrinks, update or remove its baseline in the same PR so the
budget ratchets downward instead of allowing silent regrowth.

The 800-line file gate is an aiops-platform repo-specific maintainability
budget, not an official Go file-length limit. Effective Go delegates formatting
to `gofmt` and explicitly says Go has no **line length** limit
(`https://go.dev/doc/effective_go`); that line-length guidance should not be
misread as a file-length recommendation. The custom test counts raw physical
lines in each tracked Go file, excluding `_test.go` files and generated files
with the standard `Code generated` / `DO NOT EDIT` header. A generic linter rule
would not encode this repository's exact oversized-file baseline/ratchet:
new unbaselined oversized production files fail, existing oversized files are
grandfathered only at their recorded line count, and any shrink must ratchet the
baseline down in the same PR.

Decompose oversized files by responsibility during burn-down rather than keep
baselines permanently. Split within the same package when that is enough; move
code into `internal/...` helper packages only when there is a cohesive subsystem
boundary, matching the Go module layout guidance for server/internal packages
(`https://go.dev/doc/modules/layout`). Do not introduce public API surface just
to satisfy the line budget.

When comparing local lint output to CI, use an isolated cache if multiple
worktrees for this repository are open under the same parent directory:

```bash
GOLANGCI_LINT_CACHE=$(mktemp -d) go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run --config=.golangci.yml
```

This prevents stale package-analysis entries from sibling worktrees from
appearing as false findings.

### Release Please (version, CHANGELOG, tag automation)

File:

```text
.github/workflows/release-please.yml
release-please-config.json
.release-please-manifest.json
```

On every push to `main`, release-please maintains a Release PR that bumps the
version and updates `CHANGELOG.md` from Conventional Commits (`release-type:
go`, manifest mode; pre-1.0 bump flags so breaking changes bump minor and
features bump patch). Merging the Release PR creates the `vX.Y.Z` tag and the
GitHub Release with changelog notes; the tag push triggers the Release
workflow below, which attaches binaries and the SBOM.

Because `main` merges squash-only with `squash_merge_commit_title: PR_TITLE`,
the squash commit subject is the PR title, and release-please parses that
subject. A PR title that is not a Conventional Commit (any of the canonical
types in AGENTS.md → Conventions) is dropped silently — no
changelog entry, no version bump — so the `Validate PR title (Conventional
Commits)` required check (`.github/workflows/pr-title-lint.yml`, a SHA-pinned
`amannn/action-semantic-pull-request`) gates every PR title. `release-please-config.json`
deliberately carries **no** `changelog-sections` override: it inherits
release-please's default, which hides `chore`/`refactor`/`docs`/`style`/`test`/
`build`/`ci` so the CHANGELOG stays a user-facing "what changed for an operator"
document (breaking changes of any type still surface). If you add a new required
CI job, register its check name in `.github/governance/main-ruleset.json`.

`release-please-config.json` sets `"always-update": true` so an existing Release
PR is refreshed when `main` advances even if the newest commit is
non-releasable and does not change release notes. This keeps the Release PR
compatible with the repository's strict required status checks, at the cost of
rerunning PR CI after docs/refactor/ci-only merges that would otherwise leave
the Release PR branch stale.

The workflow authenticates with a short-lived GitHub App installation token,
not `GITHUB_TOKEN`: GitHub suppresses workflow runs for events created by
`GITHUB_TOKEN`, so a `GITHUB_TOKEN`-cut tag would never trigger
`on: push: tags`, and required checks on the Release PR would wait for a
manual "Approve workflows to run" click on every update.

One-time App setup (operator):

1. Create a GitHub App named `aiops-platform-release` (Settings → Developer
   settings → GitHub Apps → New GitHub App). Disable the webhook. Repository
   permissions: Contents read/write, Pull requests read/write, Issues
   read/write. If the name is taken, pick another and update the
   `<app-slug>[bot]` entry in `exemptAuthorLogins`
   (`.github/scripts/validate-pr-metadata.mjs`) plus its test to match.
2. Install the App on `xrf9268-hue/aiops-platform` only.
3. Store repo secrets: `RELEASE_PLEASE_APP_ID` (the App ID) and
   `RELEASE_PLEASE_APP_PRIVATE_KEY` (a generated `.pem` private key).

Key rotation: generate a new private key in the App settings, replace the
`RELEASE_PLEASE_APP_PRIVATE_KEY` secret, then revoke the old key.

Rolling back a bad release: delete the GitHub Release and the tag
(`gh release delete vX.Y.Z --yes` then `git push origin :refs/tags/vX.Y.Z`),
and revert the Release PR merge commit so `CHANGELOG.md` and
`.release-please-manifest.json` no longer claim the version. The next
release-please run recomputes from the remaining tags.

The Release PR is authored by `aiops-platform-release[bot]`, which is exempt
from the PR Metadata gate (no `Closes #N`, no SPEC checklist) — see
`exemptAuthorLogins` in `.github/scripts/validate-pr-metadata.mjs`.

### Release

File:

```text
.github/workflows/release.yml
```

Triggers:

- push tag matching `v*.*.*`
- manual `workflow_dispatch` with an existing tag

Outputs:

- Linux amd64 `worker` and `tui`
- Linux arm64 `worker` and `tui`
- macOS amd64 `worker` and `tui`
- macOS arm64 `worker` and `tui`
- CycloneDX SBOM attached to the release
- GitHub artifact attestations for release artifacts

Release publishing first resolves the exact tag ref and passes that ref to the
CI workflow through `workflow_call`, so tag publishing inherits the same
race-test, security, e2e, Docker, Trivy, and SBOM quality gates on the commit
being released. The release job keeps `contents: write` scoped to publishing,
and adds `id-token: write` plus `attestations: write` only for GitHub artifact
provenance.

The GitHub Release itself is created by release-please (with changelog notes);
this workflow attaches artifacts to it via `gh release upload --clobber`. On
the manual `workflow_dispatch` path, a tag without an existing Release gets one
created as a fallback before the upload.

## Local checks before pushing

Run:

```bash
go mod tidy
gofmt -w cmd internal
go vet ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run --config=.golangci.yml
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.trellis/scripts python3 -m unittest discover -s .trellis/scripts/tests -p 'test_*.py'
cd cmd/worker/dashboard && npm ci && npm test && npm run build && cd -
go test -run '^TestProductionGoFilesStayWithinSizeBudget$' -count=1 ./scripts
go test ./...
go build ./cmd/worker ./cmd/tui
docker build --pull --tag aiops-platform:local .
```

Then verify:

```bash
git diff --exit-code -- go.mod go.sum
```

## Expected first CI failure

If `go.sum` has not been committed yet, the first CI run may fail at `Verify go mod tidy`.

Fix locally:

```bash
go mod tidy
git add go.mod go.sum
git commit -m "chore: add go.sum"
git push
```

## Dependabot

File:

```text
.github/dependabot.yml
```

Dependabot is configured for:

- Go modules
- GitHub Actions
- dashboard npm dependencies in `cmd/worker/dashboard`

It runs weekly and groups Go and dashboard npm dependency updates to reduce
pull request noise.

## Security posture

- CI uses top-level `permissions: contents: read`.
- The release workflow uses read-only permissions for the reusable CI gate.
- The release job uses `contents: write`, `id-token: write`, and
  `attestations: write` only for release publication and provenance.
- Workflows do not use `pull_request_target`.
- No workflow stores or prints project secrets.
- `persist-credentials: false` is used for checkout because the workflows do not need to push commits.
