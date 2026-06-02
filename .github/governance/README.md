# Branch governance

`main-ruleset.json` is the source-of-truth for the `main` branch ruleset. A
committed JSON file is **not** auto-applied — GitHub stores rulesets server-side,
so it must be imported once (and re-imported after edits) by a repo admin.

## What it enforces

Required-check `context` values are the **emitted check-run names** (the
Actions job `name:`), not the `Workflow / Job` form shown in the PR UI — a
mismatched context blocks every PR forever waiting on a check that never reports.

- **`Validate PR metadata`** is a required status check — the SPEC-deviation
  merge gate (AGENTS.md principle 6/7, #588). A PR that changes a SPEC-sensitive
  path (`internal/workflow/config.go`, a newly-added or renamed
  `internal/orchestrator/`/`internal/worker/` file) cannot merge while claiming
  it adds no new key/phase; it must cite an upstream Elixir reference or track a
  `DEVIATIONS.md` row.
- **`Go build and test`** and **`Security and supply-chain`** required.
- Review-thread resolution required (the Codex-review protocol's unresolved
  threads block merge); stale reviews dismissed on push; squash-only; no branch
  deletion or force-push on `main`.

`required_approving_review_count` is `0` because the repo is single-maintainer /
agent-driven; raise it (and re-import) once a second reviewer account exists.
`E2E Gitea mock loop` and `Docker image build` are intentionally left out of the
required set (heavier / Docker-pull sensitive); add their (job-name) contexts to
`required_status_checks` if you want them blocking too.

## Apply / update the ruleset

```bash
# Create (first time):
gh api --method POST repos/xrf9268-hue/aiops-platform/rulesets \
  --input .github/governance/main-ruleset.json

# List to find the id, then update after edits:
gh api repos/xrf9268-hue/aiops-platform/rulesets --jq '.[] | "\(.id)\t\(.name)"'
gh api --method PUT repos/xrf9268-hue/aiops-platform/rulesets/<id> \
  --input .github/governance/main-ruleset.json
```

## Sequencing

Apply the ruleset **after** this PR and any other open PRs merge. Once
`PR Metadata / Validate PR metadata` is required, every open PR must carry the
SPEC-alignment checklist from `.github/pull_request_template.md` or it cannot
merge — so land the template + gate first, then import the ruleset.
