# Issue #647: Large File Size Signal

## Goal

Add a lightweight CI-backed signal for oversized production Go files so new
large files and growth in current large-file debt are visible before review and
PR merge.

## Live Issue Context

- Target: GitHub issue #647, `ci: large files and grandfathered large functions
  are not enforced`.
- Labels: `area:testing`, `type:chore`, `area:tech-debt`.
- Dependency class: independent issue.
- #627 gate: live issue #627 is closed and GraphQL reports no blocked-by,
  blocking, parent, sub-issue, tracked, or tracked-in relationships.
- #647 dependency check: GraphQL reports no blocked-by, blocking, parent,
  sub-issue, tracked, or tracked-in relationships.
- No batch parent is needed for this single independent issue.

## Workflow Context

- `main` was refreshed with `git checkout main && git pull --ff-only`.
- Required docs read: `CONTEXT.md`,
  `docs/runbooks/personal-daily-workflow.md`,
  `docs/runbooks/dogfood-development.md`,
  `docs/runbooks/pr-review-merge-protocol.md`,
  `.trellis/spec/backend/agent-workflow-guidelines.md`,
  `.claude/skills/handle-issue/SKILL.md`,
  `.claude/skills/handle-pr/SKILL.md`, and
  `/Users/yvan/.agents/skills/grill-with-docs/SKILL.md`.
- Root `WORKFLOW.md` is absent on `origin/main`; `examples/WORKFLOW.md` was
  read as the available workflow template, but there is no root workflow file
  to obey in this checkout.

## Acceptance Criteria

- [ ] CI fails when a new non-test, non-generated Go source file exceeds 800
  lines.
- [ ] CI fails when an existing grandfathered oversized production Go file grows
  beyond its recorded baseline.
- [ ] Existing oversized production Go files are explicitly documented as the
  current baseline so #521-style decomposition can reduce that list over time.
- [ ] Existing function-budget behavior remains unchanged: `funlen`/`gocognit`
  stay blocking, with inline `//nolint... baseline (#521)` debt.
- [ ] `AGENTS.md` and `docs/runbooks/ci.md` describe the new file-size signal.
- [ ] Regression and mutation verification prove the new check is not a
  placebo.
- [ ] Local handle-issue gates pass before push.

## Out of Scope

- Do not decompose the five existing oversized files in this issue.
- Do not change `go.mod`'s Go directive or add third-party dependencies.
- Do not add worker/orchestrator runtime gates, workflow config keys, post-turn
  artifacts, or PR/push/tracker-write behavior.
- Do not merge the PR.

## Implementation Plan

1. Add a `scripts` package Go test that scans tracked `*.go` files using
   `git ls-files`, skips `_test.go`, skips generated files, and enforces:
   - production files at or below 800 lines pass;
   - current oversized files must be listed in a baseline map and must not grow;
   - any unlisted production file above 800 lines fails with actual and expected
     values in the test message.
2. Baseline the five current oversized production files with their live line
   counts.
3. Update `AGENTS.md` clean-code rule 7 from "file budget review-only" to the
   baseline-aware CI rule.
4. Update `docs/runbooks/ci.md` to mention the file-size test in the CI and
   local-gate descriptions.
5. Run focused tests, mutate the baseline/check to prove failure, commit, then
   run full local gates and pre-push dual reviewers.

## Grill-With-Docs Review

- Question: Does "large file" belong in the domain glossary?
  - Answer: No. `CONTEXT.md` is a domain glossary for scheduler/tracker/runtime
    terms. This is CI hygiene, not domain language.
- Question: Is this a worker runtime behavior or a repository quality gate?
  - Answer: Repository quality gate. SPEC §3.2 homes team policy in workflow
    prompt and tooling, and upstream has no worker-side file-size phase. The
    implementation must stay in CI/tests/docs.
- Question: Is `revive file-length-limit` sufficient?
  - Answer: No. It can enforce a hard threshold, but excluding current large
    files would preserve silent growth. A baseline-aware test matches the
    repository's existing debt-burn-down pattern.
- Question: Does the plan create a durable trade-off needing an ADR?
  - Answer: No. The change is reversible CI hygiene and already follows
    AGENTS.md clean-code rule 7; updating AGENTS/runbook is enough.

## Verification Plan

- `go test ./scripts`
- Mutation check: temporarily lower or remove one baseline entry and confirm
  `go test ./scripts` fails with the oversized file and actual line count.
- `gofmt -l $(git ls-files '*.go')`
- `go mod tidy && git diff --exit-code -- go.mod go.sum`
- `go vet ./...`
- `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run --config=.golangci.yml --issues-exit-code=0`
- `go test -race -covermode=atomic ./...`
- `go build ./cmd/worker ./cmd/tui`
