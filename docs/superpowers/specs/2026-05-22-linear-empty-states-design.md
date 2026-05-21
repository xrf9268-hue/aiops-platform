# Linear `ListIssuesByStates([])` early return — design

**Date:** 2026-05-22
**Issue:** [#233](https://github.com/xrf9268-hue/aiops-platform/issues/233)

## Problem

SPEC §17.3 Core Conformance says "Empty `fetch_issues_by_states([])` returns empty without API call". `internal/tracker/linear.go:119` does not honor this — with `states == nil` or `[]` (or all-whitespace `[" "]`), it still validates API key + project slug, builds a GraphQL request, and dispatches a network call to Linear. Linear returns empty; the worker burns a quota slot for nothing.

`internal/tracker/github.go:97` already short-circuits via `nonEmptyGitHubStates`. The Linear adapter is missing the equivalent.

## Decision

Add an empty/whitespace-aware filter at the top of `LinearClient.ListIssuesByStates`. If the cleaned list is empty, return `(nil, nil)` without API key / project-slug validation, without building the request. Pass the cleaned list as the GraphQL `$states` variable so whitespace-only entries don't reach Linear as bogus state names either.

### Why filter precedes API-key validation

Conformance says "without API call" — by the time the function knows the inputs cannot produce results, returning is correct regardless of credential state. A caller probing emptiness shouldn't be forced to set API keys first. (And the existing GitHub adapter places its `len(stateFilters) == 0` check at line 97-99, *after* token validation. The strictest reading of SPEC §17.3 says no API call; the strictest reading of "without API call" lets either order satisfy. The Linear placement here matches GitHub's by placing the empty-check after credential validation. **Updated:** match GitHub adapter ordering exactly to keep parity — credential checks first, then empty-state guard.)

Final placement: after API key / project slug validation, before GraphQL construction, mirroring GitHub.

### Why not extract a shared helper

`nonEmptyStates` exists in `internal/worker` (different package). `nonEmptyGitHubStates` is in `internal/tracker` but does GitHub-specific lowercasing and dedup. Linear state names are arbitrary workflow strings — no lowercasing/dedup needed. A 4-line inline filter is simpler than a third helper.

## What changes

| File | Change |
| --- | --- |
| `internal/tracker/linear.go:119` (`ListIssuesByStates`) | After existing API-key + project-slug validation, filter `states` to `nonEmpty` via `strings.TrimSpace`. If `len(nonEmpty) == 0`, return `nil, nil`. Use `nonEmpty` as the GraphQL `$states` variable on line 195. |
| `internal/tracker/linear_test.go` | Add `TestLinearClient_ListIssuesByStatesEmptyShortCircuits` that records HTTP traffic and asserts 0 requests when `states` is `nil`, `[]`, `[""]`, or `["  "]`. |

## Concrete change

```go
func (c *LinearClient) ListIssuesByStates(ctx context.Context, states []string) ([]Issue, error) {
    if c.APIKey == "" {
        return nil, NewError(CategoryMissingTrackerAPIKey, "Linear API key is required", nil)
    }
    projectSlug := strings.TrimSpace(c.Config.ProjectSlug)
    if projectSlug == "" {
        return nil, NewError(CategoryMissingTrackerProjectSlug, "Linear project slug is required", nil)
    }
    nonEmpty := make([]string, 0, len(states))
    for _, s := range states {
        if t := strings.TrimSpace(s); t != "" {
            nonEmpty = append(nonEmpty, t)
        }
    }
    if len(nonEmpty) == 0 {
        return nil, nil
    }
    // ...rest unchanged, but pass `nonEmpty` in the variables map:
    if err := c.graphql(ctx, query, map[string]any{
        "projectSlug": projectSlug,
        "states":      nonEmpty,
        "first":       linearIssuePageSize,
        "after":       after,
    }, &out); err != nil { ... }
}
```

## Non-goals

- Don't extract a cross-package `nonEmptyStrings` helper — three call sites already each have their own variant; YAGNI.
- Don't change the credential-validation order — match GitHub adapter's "validate creds first, then guard empty".
- Don't audit / change the Gitea adapter — Gitea already short-circuits in its own state-mapping path; this PR is Linear-only.
- Don't change `ListActiveIssues` — it calls `ListIssuesByStates(ctx, c.Config.ActiveStates)`, so the guard covers it transitively.

## Acceptance criteria

- [ ] `Linear.ListIssuesByStates(ctx, nil)` returns `(nil, nil)` without HTTP.
- [ ] Same for `[]string{}`, `[]string{""}`, `[]string{" "}`.
- [ ] Regression test records HTTP traffic and asserts zero requests.
- [ ] Existing Linear tests continue to pass.

## Refs

- Issue: https://github.com/xrf9268-hue/aiops-platform/issues/233
- Code: `internal/tracker/linear.go:119`, `internal/tracker/github.go:97`
- Helper precedent: `internal/worker/reconcile.go:293` (`nonEmptyStates`), `internal/tracker/github.go:360` (`nonEmptyGitHubStates`)
- SPEC §17.3 Core Conformance bullet
