# Issue #659: align go vet checklist with CI workflow

## Goal

Close GitHub issue #659 by making the `go vet ./...` source of truth explicit
across AGENTS.md, CI runbook text, and script-level workflow regression tests.

## Live Issue Facts

- Issue: #659, `docs: align go vet checklist with CI workflow`
- Labels: `area:docs`, `area:testing`, `type:docs`, `priority:p3`
- State: open
- Existing comments: none
- Dependency markers: none found
- Current `main`: `2f5e81ed9572b377cecaa41e0596c80567d1a7b9`

## Plan

1. Keep `go vet ./...` as a real CI step. Current `main` already has
   `.github/workflows/ci.yml` job `Security and supply-chain` step `go vet`
   running `go vet ./...`; the intended source of truth is CI parity, not a
   local-only extra.
2. Update AGENTS.md and docs/runbooks/ci.md so they distinguish standalone
   `go vet ./...` from the separate golangci-lint `govet` analyzer.
3. Add a Node regression test under `.github/scripts/` that parses
   `.github/workflows/ci.yml` structurally and fails if the standalone
   `go vet ./...` step disappears or drifts to a different command.
4. Keep the PR small. Do not change CI execution order or add unrelated
   checklist items.

## Grill-With-Docs Challenge

- SPEC boundary: this is repository CI/documentation tooling, not a Symphony
  worker/orchestrator/tracker behavior. No SPEC citation or DEVIATIONS row is
  needed.
- False-parity risk: #659 exists because humans could not tell whether
  `go vet ./...` was CI-backed or local-only. A prose-only fix can drift again,
  so add a regression-style script test for the workflow shape.
- Duplicate-check risk: `golangci-lint` includes a `govet` analyzer, but that
  is not equivalent to the standalone `go vet ./...` subprocess in CI. The docs
  should name both distinctly instead of implying one replaces the other.
- Scope guard: do not bundle #660 file-size policy audit or #661 baseline debt.

## Acceptance Criteria

- [x] The intended source of truth is recorded: `go vet ./...` is a CI step.
- [x] AGENTS.md and docs/runbooks/ci.md no longer leave ambiguity between
  standalone `go vet ./...` and the golangci-lint `govet` analyzer.
- [x] A script test fails if the CI workflow no longer contains a standalone
  `go vet ./...` step.
- [x] Local docs/workflow tests and the standard Go gate pass before push.

## Outcome

- Branch/worktree: `fix/659-go-vet-ci-checklist` / `/Users/yvan/.codex/worktrees/659/aiops-platform`
- PR: #674, `https://github.com/xrf9268-hue/aiops-platform/pull/674`
- Head: `4dc3a33313cb23dbe372ecdea4a03e427a3f64c5`
- State: deferred, not merge-ready. GitHub Codex review did not return clean:
  trigger `4632572769` failed with connector `An unknown error occurred`;
  trigger `4632620697` did not produce a clean review within the follow-through
  window. The batch hard stop prevents auto-merge.
- Clean gates before deferral: full local gate, mutation check, subagent
  reviewer `Boole`, Codex-family local JSON reviewer, Claude-family local JSON
  reviewer, CI run `27020435529`, metadata run `27020435530`, GraphQL
  reviewThreads `[]`, warning audit with only harmless checkout/tar/Vite warning
  text, and size gate `within budget`.

## Verification Plan

- `node --test .github/scripts/*.test.mjs`
- `python3 - <<'PY' ... yaml.safe_load('.github/workflows/ci.yml') ... PY`
- Full local gate before push:
  - `gofmt -l $(git ls-files '*.go')`
  - `go mod tidy && git diff --exit-code -- go.mod go.sum`
  - `go vet ./...`
  - `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run --config=.golangci.yml --issues-exit-code=0`
  - `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.trellis/scripts python3 -m unittest discover -s .trellis/scripts/tests -p 'test_*.py'`
  - `go test -run '^TestProductionGoFilesStayWithinSizeBudget$' -count=1 ./scripts`
  - `go test -race -covermode=atomic ./...`
  - `go build ./cmd/worker ./cmd/tui`

## Out of Scope

- Changing CI behavior that is already present on current `main`.
- Auditing Go official file-size guidance (#660).
- Refactoring oversized file baselines (#661).
