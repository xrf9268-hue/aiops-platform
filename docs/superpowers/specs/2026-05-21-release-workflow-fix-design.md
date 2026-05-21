# Release workflow fix — design

**Date:** 2026-05-21
**Issue:** [#219](https://github.com/xrf9268-hue/aiops-platform/issues/219) — [P0][bug] Release workflow builds non-existent `./cmd/trigger-api` and omits gitea-poller (every tag push fails)

## Problem

`.github/workflows/release.yml` builds three binaries per platform on every tagged release:

```yaml
GOOS=... go build -o "${out}/trigger-api"    ./cmd/trigger-api
GOOS=... go build -o "${out}/worker"         ./cmd/worker
GOOS=... go build -o "${out}/linear-poller"  ./cmd/linear-poller
```

`./cmd/trigger-api` does not exist in this repo. AGENTS.md confirms it was removed under #74 (Gitea webhook ingress, not in SPEC). The build step fails with `package ./cmd/trigger-api is not in std` before reaching the upload step, so every tag push and every `workflow_dispatch` invocation of the release workflow is broken end-to-end.

Secondarily, `./cmd/gitea-poller` is shipped by `ci.yml:60-67` and `Dockerfile:6-9` but missing from release artifacts — operators that install from release tarball cannot run the Gitea legacy poller (still in tree until #73 second half lands).

## Ground truth

`cmd/` directory at HEAD (2026-05-21):

```
cmd/
├── gitea-poller
├── linear-poller
└── worker
```

| File | Current binary list | Status |
|---|---|---|
| `cmd/` | `gitea-poller`, `linear-poller`, `worker` | source of truth |
| `.github/workflows/ci.yml:60-67` | `worker`, `linear-poller`, `gitea-poller` | correct |
| `.github/workflows/release.yml:78-80` | `trigger-api` (gone), `worker`, `linear-poller` | wrong |
| `Dockerfile:6-9` | `worker`, `linear-poller`, `gitea-poller` | correct |

Only `release.yml` is out of sync.

## Decision

**Option A** from the issue body: remove the `./cmd/trigger-api` line, add `./cmd/gitea-poller`. Factor the per-binary build into a single shell loop variable inside `release.yml`.

### Why Option A (not Option B = create a `cmd/trigger-api` package)

- AGENTS.md "Transitional notes" says `cmd/trigger-api` was removed under #74 because webhook ingress is not in SPEC. Re-introducing the package would re-introduce out-of-spec surface.
- No SPEC reference and no design doc references a separate trigger-api binary today; trigger needs are served by `cmd/worker`'s HTTP `POST /api/v1/refresh` (#186/#203).

### Why intra-`release.yml` loop (not cross-file shared source)

Acceptance criterion #3 in the issue reads "Binary list factored into a single loop variable (CI + release + Dockerfile all read it)". We adopt the **smaller form** — factor within `release.yml` only — for these reasons:

1. The issue body itself describes the factoring as `for binary in worker linear-poller gitea-poller; do ... done`, i.e. a single shell variable inside the release loop. That is what the recommendation actually shows.
2. Cross-file unification requires Dockerfile `ARG` plumbing (multiple `RUN go build` lines can't easily iterate a list without splitting builds) or an external shell script — real machinery for marginal value.
3. The binary list is **transitional**: `linear-poller` and `gitea-poller` are slated for removal once #73 closes the second half (per AGENTS.md). Investing in cross-file unification ahead of that contraction is anti-YAGNI.
4. `ci.yml` and `Dockerfile` already match `cmd/`; only `release.yml` drifted. Fix the drifter, don't refactor the correct files.

## Change

`.github/workflows/release.yml`, replace the per-binary build lines with a single loop over a shell array:

```bash
binaries=(worker linear-poller gitea-poller)
for target in "${targets[@]}"; do
  read -r goos goarch <<< "$target"
  out="dist/aiops-platform_${{ steps.release.outputs.tag }}_${goos}_${goarch}"
  for binary in "${binaries[@]}"; do
    GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
      go build -trimpath -ldflags="-s -w" -o "${out}/${binary}" "./cmd/${binary}"
  done
  tar -C dist -czf "${out}.tar.gz" "$(basename "$out")"
  rm -rf "$out"
done
```

No other file changes. No new files.

## Non-goals

- Do **not** create `cmd/trigger-api` — that's Option B and explicitly out of scope.
- Do **not** factor `ci.yml` or `Dockerfile` — they're already correct and the binary list is transitional.
- Do **not** add new linters or release-artifact validation tooling — verification is end-to-end via the fork dry-run.

## Verification plan

The issue's acceptance criterion #4 requires "a dry-run of the release workflow on a fixture tag succeeds end-to-end". Procedure on the **fork** (`xrf-9527/aiops-platform`, which has GitHub Actions minutes; upstream is exhausted):

1. After the branch is pushed to the fork, tag the branch HEAD as `v0.0.0-rc-test-219` and push the tag.
2. `release.yml` triggers on `push: tags v*.*.*` — runs automatically. The tag `v0.0.0-rc-test-219` matches the glob (`0.0.0-rc-test-219` is three dot-separated segments).
3. Wait for the release workflow to complete. Confirm:
   - All four platforms (`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`) build.
   - Each tarball contains `worker`, `linear-poller`, `gitea-poller` (3 binaries × 4 platforms = 12 binaries total inside 4 tarballs).
   - A draft/published GitHub release exists on the fork at `v0.0.0-rc-test-219`.
4. After the upstream PR merges, clean up: delete the fork release + delete the `v0.0.0-rc-test-219` tag from the fork.

Local checks (before push):

```bash
gofmt -l .github/workflows/release.yml || true  # YAML; gofmt N/A but doc completeness
git diff --check                                 # whitespace
# Smoke-build the three binaries locally:
for b in worker linear-poller gitea-poller; do
  go build -o /tmp/${b} ./cmd/${b}
done
```

## Acceptance criteria checklist

- [ ] `./cmd/trigger-api` line removed from `release.yml`.
- [ ] `./cmd/gitea-poller` added to release-built artifacts.
- [ ] Binary list factored into a single shell array inside the `Build release binaries` step.
- [ ] Fork dry-run on `v0.0.0-rc-test-219` succeeds end-to-end (all 4 platform tarballs uploaded, each contains all 3 binaries).
- [ ] Fork test tag + test release cleaned up post-merge.

## Open questions

None — design is fully determined by SPEC + AGENTS.md + the issue body.

## Risks

- **Tag-glob match**: `v0.0.0-rc-test-219` is unusual; if the glob `v*.*.*` doesn't match (e.g. it requires segments to be numeric-only), the dry-run won't trigger. Mitigation: use `v0.0.999` instead, which is unambiguous.
- **Fork release litter**: the dry-run creates a real GitHub release on the fork. Mitigation step in verification plan #4.
- **No effect on `cmd/worker` HTTP endpoint**: this change does not touch runtime trigger plumbing — `POST /api/v1/refresh` continues to be the trigger path.

## Refs

- Issue: https://github.com/xrf9268-hue/aiops-platform/issues/219
- AGENTS.md "Transitional notes" — `cmd/trigger-api` removed under #74, `linear-poller`/`gitea-poller` removal scheduled under #73 second half
- `.github/workflows/release.yml:75-82`
- `.github/workflows/ci.yml:60-67`
- `Dockerfile:6-9`
