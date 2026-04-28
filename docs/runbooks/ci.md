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

Checks:

- checkout with read-only repository permissions
- setup Go from `go.mod`
- Go module download
- `gofmt` check
- `go mod tidy` check
- `go test -race -covermode=atomic ./...`
- build `trigger-api`, `worker`, and `linear-poller`
- upload short-lived CI binaries
- Docker image build validation

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

The release job uses `contents: write` only in the release job because publishing a GitHub release needs write access. CI remains read-only.

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
- The release workflow uses `contents: write` only for the release job.
- Workflows do not use `pull_request_target`.
- No workflow stores or prints project secrets.
- `persist-credentials: false` is used for checkout because the workflows do not need to push commits.
