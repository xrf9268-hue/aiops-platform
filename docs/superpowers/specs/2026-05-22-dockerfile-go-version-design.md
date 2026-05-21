# Dockerfile / go.mod Go version alignment — design

**Date:** 2026-05-22
**Issue:** [#220](https://github.com/xrf9268-hue/aiops-platform/issues/220) — [P2][bug] Dockerfile pins golang:1.26-bookworm but go.mod declares go 1.25.0

## Problem

`Dockerfile:1` pins `FROM golang:1.26-bookworm`. `go.mod:3` declares `go 1.25.0`. CI's `setup-go@v6` resolves to the version from `go-version-file: go.mod` (1.25.x). The Docker image therefore builds with Go 1.26.x while CI tests with Go 1.25.x — a silent toolchain split.

## Decision

**Option B from the issue body**: pin Dockerfile to `1.25` (matching go.mod), parameterised through `ARG GO_VERSION=1.25` so a future bump is one line. Add a CI step that asserts the Dockerfile `GO_VERSION` and go.mod `go` directive share the same `MAJOR.MINOR`.

### Why Option B and not Option A (bump go.mod to 1.26)

- `go.mod` is the language-version contract for the module. Bumping `go 1.25.0 → 1.26.0` raises the minimum Go version for any downstream consumer / contributor and changes std-library `min` for `slices`, `maps`, etc. That's a deliberate choice the maintainer should make, not a side effect of fixing a Dockerfile drift.
- The CI suite is already passing on 1.25. Pinning the container to 1.25 brings reproducibility without changing the language version semantics.

If the maintainer wants 1.26 later, the new `ARG` makes it `--build-arg GO_VERSION=1.26` plus a `go.mod` bump in lockstep — and the drift check fires if only one side moves.

### Why an ARG (not a hard literal)

`ARG GO_VERSION=1.25` permits ad-hoc testing with `docker build --build-arg GO_VERSION=1.26 .` without editing the Dockerfile. Cost: one extra line. The default keeps the canonical version close to where humans look.

### Why a CI bash check (not Renovate / Dependabot config)

The repo does not have Renovate / Dependabot configured. Adding a one-step bash check inside the existing `go` job in `ci.yml` is the smallest defence-in-depth: it fires on every PR and PR-base push, with no new dependency. Renovate / Dependabot can be added later as a separate concern.

## What changes

| File | Change |
| --- | --- |
| `Dockerfile` | `FROM golang:1.26-bookworm AS build` → `ARG GO_VERSION=1.25\nFROM golang:${GO_VERSION}-bookworm AS build` |
| `.github/workflows/ci.yml` | Add a step in the `go` job (between gofmt and "Verify go mod tidy") titled `Verify Dockerfile Go version matches go.mod` that extracts `MAJOR.MINOR` from both files and fails the build on mismatch. |

## CI check (concrete)

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
      echo "Bump them together. See docs/superpowers/specs/2026-05-22-dockerfile-go-version-design.md."
      exit 1
    fi
    echo "Go version aligned: $go_mod_version"
```

## Non-goals

- Do **not** bump go.mod to 1.26 — that's a separate, maintainer-driven decision.
- Do **not** introduce Renovate / Dependabot — out of scope.
- Do **not** introduce a `.go-version` file — the ARG plus CI check already pins both sides; another file would be a third place to maintain.

## Acceptance criteria checklist

- [ ] Dockerfile and `go.mod` agree on `MAJOR.MINOR` (1.25).
- [ ] CI continues to pass.
- [ ] CI step asserts the alignment and fails on drift.

## Refs

- `Dockerfile:1`
- `go.mod:3`
- `.github/workflows/ci.yml` (setup-go step)
