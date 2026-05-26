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
- `gofmt` check
- `go mod tidy` check
- `go test -race -covermode=atomic ./...`
- build `worker`, `linear-poller`, and `gitea-poller`
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

### Release

File:

```text
.github/workflows/release.yml
```

Triggers:

- push tag matching `v*.*.*`
- manual `workflow_dispatch` with an existing tag

Outputs:

- Linux amd64
- Linux arm64
- macOS amd64
- macOS arm64
- CycloneDX SBOM attached to the release
- GitHub artifact attestations for release artifacts

Release publishing first resolves the exact tag ref and passes that ref to the
CI workflow through `workflow_call`, so tag publishing inherits the same
race-test, security, e2e, Docker, Trivy, and SBOM quality gates on the commit
being released. The release job keeps `contents: write` scoped to publishing,
and adds `id-token: write` plus `attestations: write` only for GitHub artifact
provenance.

## Local checks before pushing

Run:

```bash
go mod tidy
gofmt -w cmd internal
go test ./...
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

It runs weekly and groups Go dependency updates to reduce pull request noise.

## Security posture

- CI uses top-level `permissions: contents: read`.
- The release workflow uses read-only permissions for the reusable CI gate.
- The release job uses `contents: write`, `id-token: write`, and
  `attestations: write` only for release publication and provenance.
- Workflows do not use `pull_request_target`.
- No workflow stores or prints project secrets.
- `persist-credentials: false` is used for checkout because the workflows do not need to push commits.
