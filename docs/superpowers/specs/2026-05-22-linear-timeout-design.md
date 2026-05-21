# Linear client 30s network timeout — design

**Date:** 2026-05-22
**Issue:** [#212](https://github.com/xrf9268-hue/aiops-platform/issues/212)

## Problem

`internal/tracker/linear.go:101` constructs the Linear client with `HTTP: http.DefaultClient`. `http.DefaultClient.Timeout == 0`. The GraphQL request site at `linear.go:530` uses the caller's context as-is. The orchestrator's poll-tick path passes a long-lived context with no deadline, so a wedged Linear endpoint (CDN hiccup, network partition) hangs the poll loop indefinitely instead of failing fast per SPEC §14.2 "skip this tick, try again on next tick".

SPEC §11.2 specifies a 30 000 ms network timeout for Linear queries.

## Decision

**Option A** from the issue body: wrap each request's context in `context.WithTimeout` at the call site. Expose the timeout as a `RequestTimeout time.Duration` field on `LinearClient`, default 30 s in `NewLinearClient`, overridable for tests.

### Why Option A (per-call) over Option B (client.Timeout)

- Option B sets `http.Client.Timeout`. That works but applies uniformly to anything done through that client; future code that introduces streaming or long-poll endpoints would inherit a 30 s ceiling silently.
- Option A makes the contract explicit at every GraphQL call site and survives swapping `c.HTTP` for any other `*http.Client`.
- Tests inject `client.HTTP = srv.Client()` — a per-call timeout still fires there.

### Why a struct field (not a package-level var or hardcoded constant)

- Hardcoded `30 * time.Second` forces the test to actually wait 30 s on a wedged endpoint. Unacceptable for CI.
- Package-level mutable var races between parallel tests.
- A `RequestTimeout` field defaults to 30 s in the constructor and is set per-client in tests. No global state.

## What changes

| File | Change |
| --- | --- |
| `internal/tracker/linear.go` (struct) | Add `RequestTimeout time.Duration`. |
| `internal/tracker/linear.go` (`NewLinearClient`) | Default-initialize `RequestTimeout = 30 * time.Second`. |
| `internal/tracker/linear.go` (request method around line 530) | Resolve effective timeout (fall back to 30 s if zero), wrap `ctx` with `context.WithTimeout`, build the request with the wrapped context, defer the cancel. |
| `internal/tracker/linear_test.go` | Add `TestLinearClient_EnforcesRequestTimeout`: blocking handler, `RequestTimeout = 50 * time.Millisecond`, asserts the error unwraps to `context.DeadlineExceeded`. |

## Concrete change

```go
type LinearClient struct {
    APIKey         string
    BaseURL        string
    Config         workflow.TrackerConfig
    HTTP           *http.Client
    RequestTimeout time.Duration // SPEC §11.2 — defaults to 30s when zero
}

func NewLinearClient(cfg workflow.TrackerConfig) *LinearClient {
    base := "https://api.linear.app/graphql"
    return &LinearClient{
        APIKey:         cfg.APIKey,
        BaseURL:        base,
        Config:         cfg,
        HTTP:           http.DefaultClient,
        RequestTimeout: 30 * time.Second,
    }
}

// In the request method (around line 530):
timeout := c.RequestTimeout
if timeout <= 0 {
    timeout = 30 * time.Second
}
reqCtx, cancel := context.WithTimeout(ctx, timeout)
defer cancel()
req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.BaseURL, bytes.NewReader(b))
```

Existing `*tracker.Error.Unwrap()` (linear.go:69) preserves `context.DeadlineExceeded`, so `errors.Is(err, context.DeadlineExceeded)` works through the wrapper.

## Non-goals

- Don't introduce a separate exposed constant for the 30 s value — the struct default is the canonical source.
- Don't change `c.HTTP` from `http.DefaultClient` to a `&http.Client{Timeout: 30 * time.Second}` — Option B explicitly rejected above.
- Don't add per-method timeout overrides (e.g. shorter for `ListActiveIssues`, longer for mutations) — outside SPEC scope.

## Acceptance criteria

- [ ] Linear API calls enforce a 30 s timeout (default), overridable per-client.
- [ ] `TestLinearClient_EnforcesRequestTimeout` proves the ceiling via a blocking server + small `RequestTimeout`.
- [ ] No regression in existing Linear tests.

## Refs

- Issue: https://github.com/xrf9268-hue/aiops-platform/issues/212
- Code: `internal/tracker/linear.go:92-101, 525-540`
- Error wrapping: `internal/tracker/linear.go:69-74` (Unwrap), 76-82 (Is)
- SPEC §11.2 (30 000 ms), §14.2 (per-tick failure behavior)
