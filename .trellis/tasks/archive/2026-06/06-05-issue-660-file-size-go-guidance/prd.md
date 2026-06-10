# Issue #660: audit file-size gate against official Go guidance

## Goal

Close GitHub issue #660 by auditing the production Go file-size gate against
official Go guidance and documenting the intended interpretation clearly.

## Live Issue Facts

- Issue: #660, `docs: audit file-size gate against official Go guidance`
- Labels: `area:docs`, `area:testing`, `type:docs`, `priority:p3`
- State: open
- Existing comment: one owner comment with suggested additions
- Dependency markers: none found beyond the already-merged #657 context
- Current `main`: `2f5e81ed9572b377cecaa41e0596c80567d1a7b9`

## Official Go Research

- Effective Go, formatting section: Go delegates formatting to `gofmt` and says
  Go has no **line length** limit. This does not define a file-length policy.
  Source: https://go.dev/doc/effective_go
- Go Code Review Comments: relevant implementation checklist includes gofmt,
  useful failure messages, context/deadline handling, naming, error style, and
  generated-code conventions. Source: https://go.dev/wiki/CodeReviewComments
- Organizing a Go module: Go projects can split code across files and packages,
  with `internal` packages recommended for server/internal implementation
  boundaries. Source: https://go.dev/doc/modules/layout

## Plan

1. Keep #657's custom baseline-aware file-size gate. It is a repo-specific
   maintainability policy, not an official Go file-length rule.
2. Update AGENTS.md and docs/runbooks/ci.md to explicitly say:
   - the 800-line threshold is aiops-platform policy, not official Go guidance;
   - the metric is raw physical lines counted by `scripts/file_size_budget_test.go`;
   - test/generated files are excluded;
   - the baseline is a ratchet for gradual burn-down;
   - future debt reduction should decompose by responsibility into clearer files
     and packages, using `internal` only when a cohesive boundary exists.
3. Add a regression-style Go test under `scripts/` that fails if the docs stop
   carrying those key interpretations.
4. Do not change the enforcement strategy unless the audit finds a concrete
   implementation defect.

## Grill-With-Docs Challenge

- Official Go scope: Effective Go's line-length guidance is not file-length
  guidance. Do not overclaim that Go recommends 800-line files or forbids them.
- Custom test justification: a generic linter would not naturally encode this
  repo's exact oversized-file baseline and ratchet behavior. The custom test is
  justified if the docs name that reason.
- Implementation audit: `scripts/file_size_budget_test.go` already uses
  context-bounded git calls, helpful actual/want failure messages,
  non-test/non-generated filtering, and `-count=1` CI/local invocations to avoid
  stale package-cache results. No implementation change is needed unless review
  finds a missed invariant.
- #661 dependency: this issue must end with a clear policy verdict so #661 can
  proceed without changing strategy unexpectedly.

## Acceptance Criteria

- [x] Docs clearly state the 800-line file-size gate is repo policy, not an
  official Go file-length limit.
- [x] Docs specify raw physical line counting and test/generated exclusions.
- [x] Docs explain why the custom baseline/ratchet test remains justified over a
  generic linter rule.
- [x] Docs point future burn-down toward decomposition by responsibility rather
  than permanent baseline maintenance.
- [x] A script/doc test guards the key statements.
- [x] Policy verdict for #661 is recorded.

## Outcome

- Branch/worktree: `docs/660-file-size-go-guidance` /
  `/Users/yvan/.codex/worktrees/660/aiops-platform`
- PR: #681, `docs: clarify Go file size budget policy`
- Head: `4898445de23ce27bac1e6ce90093aa2951be555c`
- State: deferred
- CI: green (`CI` run `27042765243`, `PR Metadata` run `27042765272`)
- GraphQL reviewThreads: clean (`[]`)
- Warning audit: clean; CI and metadata logs had no `warning:` matches.
- Size gate: within budget; no production Go diff.
- GitHub Codex review: not clean. Trigger `4635973663` and trigger
  `4635997269` both failed with connector `An unknown error occurred`, so the
  PR remains draft/open and was not auto-merged.

## Policy Verdict for #661

#660 does not change the file-size policy or enforcement strategy. The intended
policy remains the existing 800 raw-physical-line, non-test/non-generated
production Go file baseline/ratchet with burn-down by cohesive responsibility
and no public API created solely to satisfy the budget. Because PR #681 is
deferred/open, #661 should wait until the policy PR is accepted/merged or the
operator manually waives that dependency gate.

## Verification Plan

- `go test -run 'TestFileSizeBudget' -count=1 ./scripts`
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

- Burning down the oversized-file baseline itself (#661).
- Changing the 800-line threshold without a new policy issue.
- Reworking function-size/complexity budgets (#521).
