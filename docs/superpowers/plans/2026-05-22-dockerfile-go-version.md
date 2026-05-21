# Dockerfile Go version alignment Implementation Plan

**Goal:** Stop the Dockerfile (Go 1.26) / go.mod (Go 1.25) toolchain drift. Pin Dockerfile to 1.25 via `ARG GO_VERSION=1.25`; add a CI step that asserts they stay aligned.

**Spec:** [`docs/superpowers/specs/2026-05-22-dockerfile-go-version-design.md`](../specs/2026-05-22-dockerfile-go-version-design.md)

**Issue:** [#220](https://github.com/xrf9268-hue/aiops-platform/issues/220)

---

## Task 1: Update `Dockerfile`

- [ ] Replace `FROM golang:1.26-bookworm AS build` with:

```dockerfile
ARG GO_VERSION=1.25
FROM golang:${GO_VERSION}-bookworm AS build
```

- [ ] `docker build --check` (or `docker build .`) if available. Validate the YAML/Dockerfile lints.

## Task 2: Add CI alignment check

- [ ] In `.github/workflows/ci.yml`, find the `go` job. Insert immediately after the `Check gofmt` step:

```yaml
      - name: Verify Dockerfile Go version matches go.mod
        shell: bash
        run: |
          set -euo pipefail
          go_mod_version=$(awk '/^go [0-9]/ {split($2, p, "."); print p[1]"."p[2]; exit}' go.mod)
          docker_version=$(awk -F= '/^ARG GO_VERSION=/ {print $2; exit}' Dockerfile)
          if [ -z "$go_mod_version" ] || [ -z "$docker_version" ]; then
            echo "could not extract version: go.mod=$go_mod_version Dockerfile=$docker_version"
            exit 1
          fi
          if [ "$go_mod_version" != "$docker_version" ]; then
            echo "Drift detected: go.mod Go major.minor = '$go_mod_version', Dockerfile GO_VERSION = '$docker_version'"
            echo "Bump them together."
            exit 1
          fi
          echo "Go version aligned: $go_mod_version"
```

- [ ] Sanity test the awk extractor locally:

```bash
go_mod_version=$(awk '/^go [0-9]/ {split($2, p, "."); print p[1]"."p[2]; exit}' go.mod)
docker_version=$(awk -F= '/^ARG GO_VERSION=/ {print $2; exit}' Dockerfile)
echo "go.mod=$go_mod_version Dockerfile=$docker_version"
test "$go_mod_version" = "$docker_version" && echo PASS || echo FAIL
```

Expected: `go.mod=1.25 Dockerfile=1.25` and `PASS`.

## Task 3: Validate full gate

- [ ] `go test -race -covermode=atomic ./...` passes (no Go code change).
- [ ] `docker build` (if docker is available locally) succeeds with the pinned 1.25 base.

## Task 4: Commit + dual PR + Codex + merge

Standard fork-routing flow.
