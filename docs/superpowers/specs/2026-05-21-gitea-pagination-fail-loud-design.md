# Gitea pagination overflow: fail-loud — design

**Date:** 2026-05-21
**Issue:** [#225](https://github.com/xrf9268-hue/aiops-platform/issues/225) — [P1][bug] Gitea pagination overflow returns truncated issue list silently (workers miss dispatchable issues)

## Problem

`(*TrackerClient).listIssuesByStateLabel` (`internal/gitea/tracker_client.go:154-209`) silently returns a truncated result set when the page count exceeds `listIssuesMaxPages` (20 pages × 50 / page = 1000 issues). The cap hit fires a metric (`paginationCapHits.Add(1)`) and a log line, but the function returns `nil` error.

Callers — `ListActiveIssues`, `ListIssuesByStates`, and through them the orchestrator's poll-tick reconcile path — treat the returned slice as the authoritative active candidate set. Issues beyond page 20 are invisible to dispatch and to terminal-cleanup reconciliation.

The GitHub adapter (`internal/tracker/github.go:listIssuesForState`) handles the same condition by returning an error with the `paginationCapHits` metric still firing. This is the SPEC §11.4-aligned posture: "pagination integrity" belongs in the error-category surface, not in silent semantics drift.

## Decision: Option A (fail-loud, mirror GitHub adapter)

```go
if page > listIssuesMaxPages {
    if !hasNext && len(batch) == 0 {
        return out, nil
    }
    c.recordPaginationCapHit(labelName)
    return nil, fmt.Errorf("gitea issue pagination exceeded %d pages for label %q", listIssuesMaxPages, labelName)
}
```

This is exactly the GitHub adapter shape (with the `label` parameter name swapped). Rationale:

1. **SPEC §11.1 + §11.4** treat `fetch_candidate_issues` as returning *the* active candidate set; silent truncation violates the contract.
2. **GitHub adapter parity** — operators get the same operational semantics across trackers, which the cross-tracker portability promise requires.
3. **Multi-tracker resilience preserved** — `cmd/worker/main.go:1005-1031` (`multiTrackerRuntimeClient`) joins per-tracker errors via `errors.Join` and continues with the other trackers' results. A Gitea overflow surfaces as a per-tick error but does not stop a Linear/GitHub tracker on the same loop.
4. **`recordPaginationCapHit` metric preserved** — observability is unchanged; only the return contract tightens.

## What changes

| File | Change |
|---|---|
| `internal/gitea/tracker_client.go:161-167` | Replace the silent `return out, nil` overflow branch with metric + `return nil, fmt.Errorf("gitea issue pagination exceeded %d pages for label %q", ...)`. Use `!hasNext && len(batch) == 0` as the clean-exit condition (matches GitHub adapter). |
| `internal/gitea/tracker_client_test.go:324-363` | Invert `TestTrackerClientListIssuesByStatesReturnsCappedResultsInsteadOfFailingWhenPageLimitExceeded` → rename to `TestTrackerClientListIssuesByStatesErrorsWhenIssuePaginationOverflows`. Assert non-nil err containing "gitea issue pagination exceeded", retain metric + log assertions. |
| `internal/gitea/tracker_client_test.go:291-322` | Leave `TestTrackerClientListIssuesByStatesAllowsExactlyFullMaxPages` untouched — that exercises the empty-probe clean-exit path which still returns success. |
| `docs/runbooks/runtime-status.md` | Add a short subsection describing the Gitea-pagination-overflow error event: when it fires, how to triage, and that the GitHub adapter behaves identically. |

The defensive `return nil, fmt.Errorf("gitea issue pagination exceeded %d pages", ...)` at the end of the function (line 208) is left as a guard against future loop-bound changes; this also mirrors GitHub's pattern.

## Non-goals

- Do **not** raise `listIssuesMaxPages` — the cap exists for spend-bounding. Option B from the issue body is rejected.
- Do **not** change `listIssuesByLabel` callers or the multi-tracker aggregator — they already handle joined errors correctly.
- Do **not** alter the `paginationCapHits` metric or its log line.
- Do **not** add a separate `aiops/pagination_overflow` error category for now — the wrapped error message is searchable and the issue body does not require an explicit category.

## Verification

```bash
go test -race -covermode=atomic ./internal/gitea/...
```

Expected:
- `TestTrackerClientListIssuesByStatesAllowsExactlyFullMaxPages` — pass (unchanged).
- `TestTrackerClientListIssuesByStatesErrorsWhenIssuePaginationOverflows` — pass with the new assertions.
- All other Gitea tests — pass.

Full module gate before push:

```bash
gofmt -l $(git ls-files '*.go')           # must be empty
go test -race -covermode=atomic ./...
go build ./cmd/worker ./cmd/linear-poller ./cmd/gitea-poller
```

## Acceptance criteria checklist

- [ ] `listIssuesByStateLabel` returns error on overflow.
- [ ] Orchestrator surfaces the error per-tick (verified by multi-tracker error-join behavior — no code change needed there).
- [ ] `recordPaginationCapHit` metric continues to fire.
- [ ] `internal/gitea/tracker_client_test.go` test asserts the error category and metric.
- [ ] `docs/runbooks/runtime-status.md` documents the cap and triage.

## Refs

- Code: `internal/gitea/tracker_client.go:154-216`
- GitHub adapter reference: `internal/tracker/github.go:listIssuesForState`
- GitHub adapter test: `internal/tracker/github_test.go:197-222`
- SPEC §8.2, §11.1, §11.4, §14.2
- Multi-tracker aggregator: `cmd/worker/main.go:1005-1031`
