# PRD: issue #771 — drain the remaining non-2xx response bodies outside the #762 sweep

GitHub issue: https://github.com/xrf9268-hue/aiops-platform/issues/771
Labels: type:chore, priority:p3, area:tech-debt

## Verdict (researched per AGENTS.md principle 7)

Three undrained non-2xx sites remain after the #762 tracker-client sweep. Research
settles the two paths the issue left open:

1. `internal/runner/linear_graphql_current_issue_guard.go:342` (workflowStates
   lookup) — **live production code** → convert to `tracker.DrainAndClose`
   (single implementation, clean-code rule 3). `internal/runner` already imports
   `internal/tracker` (tools.go); no import cycle (`internal/tracker` imports
   only `internal/workflow`).
2. `internal/gitea/client.go` `FindOpenPullRequest` + `CreatePullRequest` —
   **dead #76-era transitional code**: zero production consumers (verified by
   whole-repo grep; worker uses `NewTrackerClient`, runner uses label/config
   helpers only; only their own unit tests reference them). The runbook
   `docs/runbooks/gitea-bot-and-branch-protection.md:59` independently records
   "defined and unit-tested but has no production call path". Per AGENTS.md
   harness principle 6 (delete, don't relocate) and clean-code rule 1 →
   **delete** `client.go` + `client_test.go` + `client_timeout_test.go`,
   migrating the shared `defaultGiteaRequestTimeout` const to
   `tracker_client.go` (its only remaining consumer).

## Acceptance criteria (from the issue)

- [ ] The three sites drain via `tracker.DrainAndClose` or are deleted with the
      #76 transitional code (one converted, two deleted).
- [ ] EOF-witness test coverage per converted site (recording-body pattern from
      `internal/tracker/drain_test.go` / `internal/gitea/tracker_client_drain_test.go`).
- [ ] No behavior change to error classification or messages.

## Atomic-sweep obligations (clean-code rule 10)

- `docs/runbooks/gitea-bot-and-branch-protection.md:59` ("defined and
  unit-tested but has no production call path") and `:145` ("no merge code path
  in … internal/gitea/client.go") — update both to reflect the deletion.
- Historical plan/audit docs under `docs/superpowers/` and `docs/audits/` are
  point-in-time records; do not rewrite.
- No golangci / file-size baseline references `internal/gitea/client.go`.

## Out of scope

- `internal/runner/tools.go:570`, `gitea_tools.go:399` — already read the body
  on all paths (stated in the issue).
