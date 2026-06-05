Closes #<issue>

## Summary
- describe the user-visible change
- call out the root cause or architecture delta when relevant

## SPEC alignment (AGENTS.md principle 6/7)
Check exactly one. When this PR changes a SPEC-sensitive path
(`internal/workflow/config.go`, a newly-added `internal/orchestrator/` or
`internal/worker/` file), the **PR Metadata** gate rejects the first option —
an extension upstream lacks must be cited or tracked, not waved through.

- [ ] No new top-level `WORKFLOW.md`/`Config` key and no new worker/orchestrator phase/gate/artifact.
- [ ] Adds one, justified by an upstream **Elixir reference** — cite the matching file and line from the openai/symphony Elixir tree in the body below (a bare module name does not count; the gate looks for a concrete source path).
- [ ] Adds one, tracked as a **DEVIATIONS.md row** + an `area:spec-alignment` issue, updated in this PR.

## PR diff size budget (AGENTS.md)
Classify the production diff into exactly one state (review discipline; this
PR size-gate is human-signed, not CI-blocked):

- [ ] `within budget` — ≤12 production files / ≤300 production LOC (test files and generated code excluded).
- [ ] `size-gated: justified overage` — production diff over budget for correctness / regression / race-safety coverage that cannot be split without losing atomicity. **Needs human size-gate sign-off.**
- [ ] `size-gated: split recommended` — over budget for scope creep / separable concerns. Split instead.

## Testing
- [ ] `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.trellis/scripts python3 -m unittest discover -s .trellis/scripts/tests -p 'test_*.py'`
- [ ] `go test -run '^TestProductionGoFilesStayWithinSizeBudget$' -count=1 ./scripts`
- [ ] `go vet ./...`
- [ ] `go test -race -covermode=atomic ./...`
- [ ] `gofmt -l` clean + blocking golangci gate (`staticcheck,unparam,unused,…`)
- [ ] additional validation noted below when needed

## Notes
- Keep all three `SPEC alignment` options present and exactly one checked, or the PR Metadata gate fails.
