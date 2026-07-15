# Issue State Refresh Outcomes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every narrow tracker refresh classify each requested issue reference as current, confirmed absent, or unknown so reconciliation releases only authoritative disappearances.

**Architecture:** Add a zero-safe outcome to the existing narrow `tracker.IssueState` fact, normalize every request to an explicit map row, and make Linear, GitHub, and Gitea assign authoritative outcomes at their adapter boundaries. The poller will pass only `Current` rows into existing state reconciliation and send only `ConfirmedAbsent` refs to a dedicated actor operation; `Unknown` rows and failed requests preserve claims. Retry timers retain ownership of failure and capacity retries, while cleanup rechecks distinguish absence from terminal state.

**Tech Stack:** Go 1.25, standard library HTTP/GraphQL clients, actor-style orchestrator, `testing`, Docker-backed Gitea E2E.

## Global Constraints

- Base all work and final review on `054bab42ca9b477e6b93d6c0b94aa7fd2d2cf34d` (`origin/main` when this plan was written).
- Keep the adapter-contract and orchestrator-consumer change atomic on branch `codex/issue-1113-refresh-outcomes`; do not add a compatibility path that interprets a missing map key as absence.
- `IssueStateOutcomeUnknown` must be the zero value. Every non-empty requested ID must appear in the returned map, including configuration, transport, partial-response, malformed-reference, and repository-mismatch failures.
- Only a successful authoritative lookup may emit `IssueStateOutcomeAbsent`. A malformed/partial issue payload is `Unknown`; a successfully fetched Gitea issue that definitively has no recognized aiops workflow-state label is absent from this adapter's workflow domain and is `Absent`, preserving the existing #740 continuation-unwedge behavior.
- Preserve results from earlier clean batches and independent successful REST references when another batch/reference fails, and return the joined error for diagnostics. A GraphQL `data + errors` chunk is partial as a whole and must remain `Unknown` unless error paths prove individual fields complete.
- Treat a decodable response with missing required structural fields as a typed incomplete-response error and `Unknown`; distinguish an omitted field from a valid empty collection.
- Stop issuing later per-reference/chunk requests after a typed rate-limit or canceled/deadline context, leaving all unattempted rows `Unknown` while preserving earlier authoritative outcomes.
- Confirmed absence is not terminal: never delete its workspace and never set terminal-cleanup or operator-stop latches.
- Do not release failure, quota, or capacity retry entries during poll-tick reconciliation. SPEC section 8.4 retry timers remain their owner.
- Bound all added tracker I/O with existing request contexts and route asynchronous test/runtime work through the repository's panic-safe helpers.
- Use `apply_patch` for edits, run red tests before implementation, create the single atomic implementation commit after Tasks 1-3, and review the final three-dot diff against the fixed base.

---

### Task 1: Define and enforce the per-reference adapter contract

**Files:**

- Modify: `internal/tracker/tracker.go`
- Modify: `internal/tracker/linear.go`
- Create: `internal/tracker/linear_refresh.go`
- Modify: `internal/tracker/linear_test.go`
- Modify: `internal/tracker/github_refresh.go`
- Modify: `internal/tracker/github_blockers.go`
- Modify: `internal/tracker/github_test.go`
- Modify: `internal/gitea/tracker_client_refresh.go`
- Modify: `internal/gitea/tracker_client_test.go`

- [ ] **Step 1: Add failing contract tests for the zero-safe outcome model**

  Add table tests around `IssueState` that require these exact outcomes:

  ```go
  const (
      IssueStateOutcomeUnknown IssueStateOutcome = iota
      IssueStateOutcomeCurrent
      IssueStateOutcomeAbsent
  )
  ```

  Test that the zero value is `Unknown`, current rows retain state/labels/blockers, and absent rows carry no fabricated state. Run:

  ```bash
  go test ./internal/tracker -run 'TestIssueStateOutcome' -count=1
  ```

  Expected: FAIL because the outcome type and constants do not exist.

- [ ] **Step 2: Add failing Linear adapter outcome tests**

  Extend `internal/tracker/linear_test.go` with cases proving:

  - a clean exact-ID response marks returned nodes `Current` and omitted requested IDs `Absent`;
  - a GraphQL `data + errors` response marks the entire affected chunk `Unknown` and returns an error, even when partial nodes are present;
  - missing `data.issues.nodes`, node ID/state, or labels structure leaves the entire affected chunk `Unknown` with a typed incomplete-response error, while an explicit empty `nodes: []` is a clean authoritative absence;
  - a failed chunk leaves that chunk `Unknown` while preserving authoritative results from earlier chunks;
  - a rate-limited/canceled chunk prevents later chunk I/O and leaves unattempted IDs unknown, including Linear's documented HTTP 400 GraphQL shape with `errors[].extensions.code == "RATELIMITED"`;
  - API-key or transport failure returns an explicit `Unknown` row for each requested non-empty ID.

  Run:

  ```bash
  go test ./internal/tracker -run 'TestFetchIssueStatesByIDs.*(Outcome|Partial|Unknown|Absent)' -count=1
  ```

  Expected: FAIL because clean omissions are sparse and failures currently discard the result map.

- [ ] **Step 3: Add failing GitHub and Gitea adapter outcome tests**

  Add table cases proving for each REST adapter:

  - a resolved issue with a valid workflow state is `Current`;
  - a reference resolved through this client's current-repository ID-to-number cache and returning 404 (and GitHub 410) is `Absent`;
  - an uncached identifier fallback returning 404/410, unresolvable ref, malformed identifier, cache/identifier mismatch, or fetched-ID/repository mismatch is `Unknown`;
  - a successfully fetched Gitea issue with no recognized aiops state label is `Absent`, while a malformed/partial state payload is `Unknown`;
  - a cached issue number that conflicts with the ref's valid identifier remains `Unknown` even when the cached number's lookup returns 404; the adapter must validate reference consistency before treating not-found as authoritative;
  - a decoded REST issue whose returned number differs from the requested endpoint remains `Unknown` and cannot rewrite the ID-to-number cache;
  - a REST payload that omits required ID/number/state/labels structure remains `Unknown` with a typed incomplete-response error; explicit `labels: []` remains valid;
  - one HTTP/JSON failure produces an error and an `Unknown` row without erasing other authoritative rows;
  - a rate-limited/canceled reference prevents later reference I/O and leaves unattempted refs unknown;
  - a GitHub blocker-hydration failure leaves Todo-like rows `Unknown` but preserves authoritative non-Todo/terminal rows as `Current`;
  - Todo blocker subqueries fail closed on structurally partial data: Linear missing `data.issues.nodes`/requested rows produces the existing placeholder blocker path, while GitHub missing `node_id`/`blockedBy` produces an error and leaves only the affected Todo row unknown;
  - when blocker hydration succeeds, both blocker and no-blocker GitHub refresh entrypoints assign the same state outcome; the separate failure case may leave only Todo-like blocker-dependent rows unknown.

  Run:

  ```bash
  go test ./internal/tracker ./internal/gitea -run 'Test.*FetchIssueStates.*(Outcome|Partial|Unknown|Absent)' -count=1
  ```

  Expected: FAIL because the adapters currently skip ambiguous rows and abort with a nil map on per-reference failure.

- [ ] **Step 4: Implement the minimal adapter contract**

  In `internal/tracker/tracker.go`, add `IssueStateOutcome` and an `Outcome` field to `IssueState`, documenting that missing keys and the zero value are unknown. Add small predicates only if they remove repeated status comparisons.

  Keep the narrow Linear refresh implementation in `internal/tracker/linear_refresh.go` rather than pushing `linear.go` beyond the repository's 800-line production-file budget.

  In every adapter, prefill a deduplicated map entry for each non-empty requested ID with `Outcome: IssueStateOutcomeUnknown`. Then:

  - Linear: represent required GraphQL response fields with presence-aware pointers; only after validating a complete chunk mark missing IDs `Absent` and decoded nodes `Current`; on `data + errors` or incomplete structure, leave the entire chunk unknown, join a typed error, and preserve only earlier clean chunks. Classify both HTTP 429 and the documented HTTP 400 `RATELIMITED` GraphQL error code as `ErrRateLimited`, then break before later chunks; also stop after context cancellation/deadline.
  - GitHub/Gitea: distinguish current-repository cache resolution from identifier-only fallback; validate any cached issue number against a supplied valid identifier (and any `#N` ID) before network I/O; require the returned payload number to equal the requested endpoint number before caching; use presence-aware decode for required fields; mark a successful matching state lookup current; mark not-found absent only when a consistent current-client cache makes repository identity authoritative; leave fallback 404/410, malformed, unresolved, mismatched, partial, and failed rows unknown; continue independent references and return `errors.Join` of per-ref errors, except stop on rate-limit or context cancellation/deadline. A successful complete Gitea response with explicit `labels: []` and no recognized aiops state label is the `Absent` out-of-workflow case.
  - If GitHub's batch blocker hydration fails, leave Todo-like rows unknown and return the error; non-Todo/terminal rows do not consume blocker data and remain current. The no-blocker path may mark every successful state row current.
  - Validate nested label names and blocker-query structural fields, not only their containing arrays. Explicit empty arrays are authoritative empty sets; missing/null/empty required scalars are partial responses.

- [ ] **Step 5: Run adapter tests and the package baseline**

  ```bash
  gofmt -w internal/tracker/tracker.go internal/tracker/linear.go internal/tracker/linear_refresh.go internal/tracker/linear_test.go internal/tracker/github_refresh.go internal/tracker/github_test.go internal/gitea/tracker_client_refresh.go internal/gitea/tracker_client_test.go
  go test ./internal/tracker ./internal/gitea -count=1
  go test -run '^TestProductionGoFilesStayWithinSizeBudget$' -count=1 ./scripts
  ```

  Expected: PASS.

- [ ] **Step 6: Record the adapter mutation cases for the post-commit seam check**

  Record these post-commit mutations without applying them yet: change one authoritative not-found assignment from `Absent` to `Unknown`; change one failed Linear chunk from `Unknown` to `Current`; bypass one REST cache/identifier consistency check. Task 3 performs the mutations only after the complete atomic artifact is committed, so each test proves behavior rather than merely encountering the intentionally incomplete intermediate tree.

- [ ] **Step 7: Hold the adapter contract uncommitted for the atomic consumer switch**

  ```bash
  git diff --check
  git status --short
  ```

  Do not commit yet. Until Task 2 removes omission inference, the old `releaseVanishedContinuations` path would misclassify a new explicit `Unknown` row with an empty state as vanished. The adapter contract and every destructive consumer must first become safe in the same commit.

---

### Task 2: Reconcile confirmed absence without inferring it from omission

**Files:**

- Modify: `internal/orchestrator/poller.go`
- Modify: `internal/orchestrator/actor_reconcile.go`
- Modify: `internal/orchestrator/poller_reconciletick_test.go`
- Modify: `internal/orchestrator/actor_reconcile_inactive_test.go`
- Modify: test refresh fakes in `internal/orchestrator/poller_test.go`

- [ ] **Step 1: Add a failing real poller-to-actor state-matrix test**

  Build one poll tick with explicit outcome rows covering:

  - running + absent: cancel worker, release claim, preserve workspace, do not set terminal-cleanup/operator-stop state;
  - blocked + absent: release claim without cleanup;
  - continuation retry + absent: stop timer and release claim;
  - failure and quota/capacity retry + absent: preserve the retry and claim;
  - current active: retain claim and refresh stored state;
  - current terminal: use the existing cancel-and-cleanup path;
  - unknown row and a missing fake row: preserve all associated claims;
  - a mixed refresh error: still apply authoritative current/absent rows while preserving unknown rows, and surface the error.
  - a confirmed-absent running issue that also appears in the same tick's stale terminal/inactive group listing: the narrow absent verdict wins, no operator-terminal stop is recorded, and a pre-seeded `ReconcileCleanupWorkspace=true` flag is cleared before cancellation.

  Drive `Poller.reconcileClaimedTick` through the actual actor; do not unit-test only a leaf predicate. Run:

  ```bash
  go test ./internal/orchestrator -run 'TestPollerReconcileClaimedTick.*RefreshOutcome' -count=1
  ```

  Expected: FAIL because running/blocked absence has no actor transition and continuation still derives disappearance from omission.

- [ ] **Step 2: Add failing actor-operation tests**

  Add focused tests for a new `ReconcileAbsentTrackerIssuesAndWait` operation. Assert it releases only running, blocked, and `RetryKindContinuation` entries, waits for a running worker's `Done`, leaves workspace cleanup false, and leaves failure/quota/capacity retry timers untouched.

  Run:

  ```bash
  go test ./internal/orchestrator -run 'TestReconcileAbsentTrackerIssues' -count=1
  ```

  Expected: FAIL because the operation does not exist.

- [ ] **Step 3: Implement outcome normalization at the poller boundary**

  Update `fetchIssueStates` and `fetchIssueStatesWithoutBlockers` to prefill every non-empty requested ID as `Unknown` and overlay adapter results. This protects the orchestrator from non-compliant fakes or future adapters without converting missing rows to absent.

  Change `refreshRunningIssueStates` to return both current `tracker.Issue` rows and a set of explicitly absent refs. Remove `releaseVanishedContinuations` and all map-omission inference. Apply current rows through the existing patch/active/inactive flows, then pass only confirmed-absent refs to the new actor operation even when the adapter also returned a partial error.

  Treat the narrow per-reference result as stronger than a potentially stale terminal/inactive group listing from the same tick: remove explicitly absent IDs from the later `inactiveByID` set before invoking inactive reconciliation. Otherwise a still-exiting absent run could be reclassified as terminal and incorrectly gain workspace cleanup/operator-stop state after the absence operation.

- [ ] **Step 4: Implement the dedicated actor transition**

  Add one actor command that handles confirmed absence in a single serialized state mutation:

  - running: clear the claim, set reconcile cancellation without cleanup, invoke cancellation, and wait up to `WorkerExitTimeout`;
  - blocked: release the claim without cleanup;
  - retry: release only when `Kind == RetryKindContinuation`; retain failure/quota/capacity entries and timers.

  Do not reuse `ReconcileInactiveTrackerIssuesAndWait`, because that path deliberately releases all retry kinds and may drive terminal cleanup.

- [ ] **Step 5: Update existing refresh fakes to emit explicit current outcomes**

  Every test fake that intends to represent an observed state must set `Outcome: tracker.IssueStateOutcomeCurrent`. Tests intentionally exercising ambiguity should emit `Unknown` or omit the key and assert claim preservation.

- [ ] **Step 6: Run reconciliation tests**

  ```bash
  gofmt -w internal/orchestrator/poller.go internal/orchestrator/actor_reconcile.go internal/orchestrator/poller_reconciletick_test.go internal/orchestrator/actor_reconcile_inactive_test.go internal/orchestrator/poller_test.go
  go test ./internal/orchestrator -run 'Test(PollerReconcileClaimedTick|ReconcileAbsentTrackerIssues|Poller.*Reconcile)' -count=1
  ```

- [ ] **Step 7: Hold the reconciliation transition for the remaining consumer sweep**

  ```bash
  git diff --check
  go test ./internal/orchestrator ./internal/tracker ./internal/gitea -count=1
  ```

  Do not commit until Task 3 proves every secondary consumer is outcome-aware.

---

### Task 3: Make every secondary consumer outcome-aware

**Files:**

- Modify: `internal/orchestrator/poller_candidates.go`
- Modify: `internal/orchestrator/poller_candidates_test.go`
- Modify: `internal/orchestrator/runtime_poller.go`
- Modify: `internal/orchestrator/runtime_poller_test.go`
- Modify: `internal/orchestrator/actor_cleanup.go`
- Modify: `internal/orchestrator/actor_retry.go`
- Modify: `internal/orchestrator/actor_test.go`
- Modify: any remaining `IssueState`-producing orchestrator test fakes found by `rg`

- [ ] **Step 1: Add failing candidate and per-turn gate tests**

  Require dispatch revalidation to consume only `Current`: absent and unknown both skip dispatch, while a partial error preserves usable current rows for unrelated candidates. For the runner's per-turn gate, require current to set `Found:true`; absent and unknown-without-error to produce `Found:false, Active:true` with the prior issue fallback; and an actual adapter error to be surfaced.

  ```bash
  go test ./internal/orchestrator -run 'Test.*(Revalidate|CurrentIssue).*(Outcome|Absent|Unknown|Partial)' -count=1
  ```

  Expected: FAIL because these consumers currently equate map presence with current state.

- [ ] **Step 2: Add failing cleanup and retry ownership tests**

  Extend cleanup recheck tests so:

  - explicit current terminal still deletes the workspace;
  - explicit current nonterminal still resumes a continuation;
  - unknown/error continues the existing bounded recheck;
  - confirmed absent neither deletes nor retries nor resumes a continuation.

  Extend retry-timer tests to pin §8.4 ownership: a successful candidate lookup miss still releases failure/capacity claims; a candidate lookup error requeues; narrow refresh absence does not release them early; only an explicit current terminal result may request terminal cleanup.

  ```bash
  go test ./internal/orchestrator -run 'Test.*(CleanupRecheck|Retry).*(Absent|Unknown|Terminal|Missing)' -count=1
  ```

  Expected: FAIL on the new absent cleanup verdict and explicit outcome assertions.

- [ ] **Step 3: Implement outcome-aware secondary consumers**

  - Candidate revalidation: only `Current` may rebuild and gate a candidate; absent/unknown skip it without releasing any claim.
  - Runtime current-issue refresh: only `Current` is found; absent and unknown-without-error return the existing SPEC section 16.5 prior-issue fallback (`Found:false`, `Active:true`, nil error), while an actual adapter error is returned verbatim. The mutation tool guard still fails closed on `Found:false` without turning a benign unknown into a failed worker attempt.
  - Cleanup recheck: introduce a small verdict that distinguishes current-terminal, current-nonterminal, absent, and unknown/error. Absent exits without deletion or continuation; unknown/error uses the bounded retry.
  - Retry terminal resolution: only `Current` terminal state selects cleanup. Absent/unknown remain release-only at the timer-owned candidate decision; do not change successful candidate-miss behavior.

- [ ] **Step 4: Sweep every producer and consumer**

  ```bash
  rg -n 'IssueState\s*\{' --glob '*.go'
  rg -n 'statesByID\[|FetchIssueStates' internal/orchestrator internal/tracker internal/gitea --glob '*.go'
  ```

  Classify each occurrence. Set explicit `Current` in fakes/fixtures that mean a real observation, and verify no production consumer treats key presence or empty state as confirmed absence.

- [ ] **Step 5: Run package tests**

  ```bash
  gofmt -w internal/orchestrator/poller_candidates.go internal/orchestrator/poller_candidates_test.go internal/orchestrator/runtime_poller.go internal/orchestrator/runtime_poller_test.go internal/orchestrator/actor_cleanup.go internal/orchestrator/actor_retry.go internal/orchestrator/actor_test.go
  go test ./internal/orchestrator ./internal/tracker ./internal/gitea -count=1
  ```

- [ ] **Step 6: Commit the complete atomic contract and consumer switch**

  ```bash
  git add docs/superpowers/plans/2026-07-15-issue-state-refresh-outcomes.md internal/tracker internal/gitea internal/orchestrator
  git commit -m "fix(orchestrator): classify issue refresh outcomes" -m "Closes #1113"
  ```

  Inspect the committed tree and confirm there is no parent-to-child revision in the PR history where explicit unknown rows can reach the old omission-based release path.

- [ ] **Step 7: Mutation-check the committed wiring seams**

  Starting from a clean committed tree, use `apply_patch` for one compiling mutation at a time, run its focused test, require an assertion failure, then reverse the same patch and require `git diff --exit-code HEAD` before the next mutation:

  - tracker seam: authoritative cached not-found `Absent` → `Unknown`;
  - Linear partial-response seam: failed-chunk `Unknown` → `Current` for a returned partial node;
  - REST authority seam: bypass the cache/identifier consistency check before a not-found verdict;
  - poller/actor seam: allow `RetryKindFailure` through the confirmed-absence release condition;
  - stale-list precedence seam: stop excluding an explicitly absent ID from same-tick inactive listings;
  - cleanup seam: classify confirmed absent as terminal.

  The tests must fail on behavior assertions (outcome, retained retry/timer, cleanup/operator-stop call count), not compilation.

---

### Task 4: Run the repository gate, review the exact head, and deliver #1113

**Files:**

- Review: all changes since `054bab42ca9b477e6b93d6c0b94aa7fd2d2cf34d`
- Modify only files required to fix source-confirmed review findings

- [ ] **Step 1: Run focused race tests and inspect the diff budget**

  ```bash
  go test -race ./internal/tracker ./internal/gitea ./internal/orchestrator -count=1
  git diff --stat 054bab42ca9b477e6b93d6c0b94aa7fd2d2cf34d...HEAD
  git diff --check 054bab42ca9b477e6b93d6c0b94aa7fd2d2cf34d...HEAD
  ```

  Expected: PASS; classify the PR as `within budget` unless the production diff genuinely exceeds the repository guideline.

- [ ] **Step 2: Run every required local gate freshly**

  ```bash
  test -z "$(gofmt -l $(git ls-files '*.go'))"
  go mod tidy
  git diff --exit-code -- go.mod go.sum
  go vet ./...
  go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run --config=.golangci.yml --issues-exit-code=0
  go test -run '^TestProductionGoFilesStayWithinSizeBudget$' -count=1 ./scripts
  go test -race -covermode=atomic ./...
  go build ./cmd/worker ./cmd/tui
  go test -tags e2e -race -timeout 15m -count=1 ./test/e2e/...
  ```

  Expected: every command exits zero. CI quota is not a local blocker, but no failed local gate may be ignored.

- [ ] **Step 3: Run independent Standards and Spec reviews against the fixed base**

  Give separate reviewers the exact `BASE_SHA` and `HEAD_SHA`. Standards review checks `AGENTS.md`, clean-code rules, timeouts, goroutine safety, error classification, file/function budgets, and test quality. Spec review checks issue #1113, SPEC sections 8.4/8.5/11.2/11.4/16.3/16.5, the Elixir reference, adapter authority boundaries, and every acceptance criterion.

  Fix only source-confirmed findings, commit them, rerun the affected tests and full required gates, then re-run both reviews against the same base until both return PASS on the same head.

- [ ] **Step 4: Push and open the ready PR**

  ```bash
  git push -u origin codex/issue-1113-refresh-outcomes
  gh pr create --repo xrf9268-hue/aiops-platform --base main --head codex/issue-1113-refresh-outcomes --title 'fix(orchestrator): classify issue refresh outcomes' --body-file /tmp/aiops-pr-1113.md
  ```

  The PR body must include `Closes #1113`, `within budget` or an accurately justified size-gate classification, the SPEC-alignment evidence, and the exact local verification commands. Do not include memory citations.

- [ ] **Step 5: Follow the live PR to merge-confirmed closure**

  Re-read the current head SHA, checks, reviews, and unresolved threads. CI quota exhaustion may be documented and ignored only because the complete local gate passed; a real failed check or actionable current-head review must be fixed. Squash-merge only the exact reviewed head with head matching enabled, then verify GitHub reports `state: MERGED`, a non-null `mergedAt`, and issue #1113 closed before moving to #1114.
