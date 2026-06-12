# PRD: replace fixed time.Sleep synchronization in orchestrator tests (#760)

## Problem

Orchestrator tests synchronize with async behavior via fixed `time.Sleep`
waits: 9 in `internal/orchestrator/poller_test.go`, 7 in
`internal/orchestrator/actor_test.go` (plus a few in `test/e2e`). Fixed sleeps
are the classic flaky pattern under `-race` on loaded CI runners.

## Solution (from issue #760)

Per sleep site, prefer in order:
1. Deterministic signal: have the fake (dispatcher/tracker) close a channel /
   send on completion; block with `select` + generous timeout. Reuse existing
   fake types where possible.
2. Poll-until-deadline: retry the assertion every few ms for up to ~5s, fail
   with input/actual/expected (rule 9).

Sleeps that ARE the behavior under test (real backoff timing assertions) may
stay, each with a comment saying why.

Do not change production code unless a genuinely missing observation point is
found; then add the smallest seam with written justification.

## Acceptance criteria

- [ ] No fixed-sleep synchronization remains in internal/orchestrator tests
      (justified exceptions commented).
- [ ] `go test -race -count=10 ./internal/orchestrator/` stable.
- [ ] No net loss of assertion strength.
- [ ] test/e2e sleeps reviewed in the same pass; convert where the same idiom
      applies.
- [ ] Full local CI gates green.

## Refs

- Issue: https://github.com/xrf9268-hue/aiops-platform/issues/760
- Branch: fix/760-deflake-orchestrator-test-sync
- AGENTS.md clean-code rules 9, 10
