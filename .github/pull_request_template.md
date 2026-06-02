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
- [ ] Adds one, justified by an upstream **Elixir reference** cited here (e.g. `elixir/lib/symphony_elixir/orchestrator.ex:1142`).
- [ ] Adds one, tracked as a **DEVIATIONS.md row** + an `area:spec-alignment` issue, updated in this PR.

## Size budget (AGENTS.md)
Classify into exactly one (review discipline; the size-gate is human-signed, not CI-blocked):

- [ ] `within budget` — ≤12 files / ≤300 changed LOC.
- [ ] `size-gated: justified overage` — over budget for correctness / regression / race-safety coverage that cannot be split without losing atomicity. **Needs human size-gate sign-off.**
- [ ] `size-gated: split recommended` — over budget for scope creep / separable concerns. Split instead.

## Testing
- [ ] `go test -race -covermode=atomic ./...`
- [ ] `gofmt -l` clean + blocking golangci gate (`staticcheck,unparam,unused,…`)
- [ ] additional validation noted below when needed

## Notes
- Keep all three `SPEC alignment` options present and exactly one checked, or the PR Metadata gate fails.
