# File Size Signal Research

## Sources Checked

- GitHub issue #647 live body and labels via `gh issue view`.
- GitHub issue dependency fields for #627 and #647 via GraphQL `blockedBy`,
  `blocking`, `parent`, `subIssues`, `trackedIssues`, and `trackedInIssues`.
- Current `.golangci.yml`, `.github/workflows/ci.yml`, `scripts/ci_workflow_test.go`,
  `AGENTS.md`, and `docs/runbooks/ci.md`.
- Upstream Symphony `SPEC.md` and Elixir config/schema/workflow paths under
  `/tmp/symphony-upstream`.
- Official golangci-lint revive settings documentation for `file-length-limit`.

## Findings

- #627 is closed and has no comments or GitHub dependency relationships.
- #647 is open and has no GitHub dependency relationships.
- Root `WORKFLOW.md` is absent on `origin/main`; `examples/WORKFLOW.md` is the
  available workflow template and was read for workflow-owned scope/handoff
  rules.
- Existing function budgets are already blocking through `funlen` and
  `gocognit`; legacy debt is grandfathered with inline `//nolint... baseline
  (#521)` comments.
- Existing production Go files above the 800-line review budget:
  - `internal/doctor/doctor.go`: 1294
  - `internal/orchestrator/state.go`: 1020
  - `cmd/tui/main.go`: 904
  - `internal/workflow/loader.go`: 852
  - `internal/runner/codex_app_server.go`: 840
- `revive` supports a `file-length-limit` rule, but configured excludes would
  keep current oversized files quiet even if they keep growing. That does not
  close #647's "silently accreting" failure mode.
- A repo-local Go test can enforce a baseline-aware rule: files under or at 800
  lines pass, existing oversized files must not exceed their recorded baseline,
  and new oversized production files fail unless explicitly added to the
  baseline.
- Subagent review found the test must run through an explicit `-count=1` CI and
  local follow-through step. If it only rides along in `go test ./scripts`, Go's
  package test cache can miss newly staged oversized files discovered through
  `git ls-files`, because no `scripts` package source file changed.

## Verdict

Implement a small baseline-aware Go test under `scripts/`, run it explicitly in
CI/local follow-through with `-count=1`, then update `AGENTS.md` and
`docs/runbooks/ci.md` so the machine-enforced file-size signal becomes the
documented source of truth. Do not add a worker/orchestrator phase, workflow
key, or post-turn artifact; this is repository CI hygiene, not Symphony runtime
behavior.
