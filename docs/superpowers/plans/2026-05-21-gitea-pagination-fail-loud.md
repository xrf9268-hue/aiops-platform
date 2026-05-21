# Gitea pagination fail-loud Implementation Plan

> **For agentic workers:** Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Gitea label-scoped issue pagination fail loud on overflow (matching the GitHub adapter), so workers can no longer silently miss dispatchable issues.

**Architecture:** A single overflow branch in `internal/gitea/tracker_client.go:listIssuesByStateLabel` is replaced with the GitHub adapter's pattern (`!hasNext && len(batch) == 0` → clean exit, otherwise metric + error). One existing test is inverted to assert the error; the "exactly full max pages" empty-probe test is unchanged.

**Tech Stack:** Go 1.25 (module `github.com/xrf9268-hue/aiops-platform`), `net/http/httptest` for tracker mock servers.

**Spec:** [`docs/superpowers/specs/2026-05-21-gitea-pagination-fail-loud-design.md`](../specs/2026-05-21-gitea-pagination-fail-loud-design.md)

**Issue:** [#225](https://github.com/xrf9268-hue/aiops-platform/issues/225)

**Fork-routing reminder:** Push to `xrf-9527/aiops-platform`, open fork self-PR for CI, then cross-fork PR to upstream. See memory `project_pr_via_fork.md` + `reference_gh_accounts.md`.

---

## Task 1: Invert the test (TDD red)

**Files:**
- Modify: `internal/gitea/tracker_client_test.go:324-363`

- [ ] **Step 1: Replace `TestTrackerClientListIssuesByStatesReturnsCappedResultsInsteadOfFailingWhenPageLimitExceeded` with `TestTrackerClientListIssuesByStatesErrorsWhenIssuePaginationOverflows`**

```go
func TestTrackerClientListIssuesByStatesErrorsWhenIssuePaginationOverflows(t *testing.T) {
	var logs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			t.Fatalf("page query = %q: %v", r.URL.Query().Get("page"), err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("Link", fmt.Sprintf(`<%s%s?page=%d>; rel="next"`, serverURL(r), r.URL.Path, page+1))
		issues := make([]Issue, listIssuesPageSize)
		for i := range issues {
			number := (page-1)*listIssuesPageSize + i + 1
			issues[i] = Issue{ID: int64(number), Number: number, Title: "todo", HTMLURL: fmt.Sprintf("https://gitea.local/o/r/issues/%d", number), Labels: []Label{{Name: "aiops/todo"}}}
		}
		_ = json.NewEncoder(w).Encode(issues)
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	client.Logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	if got := client.PaginationCapHits(); got != 0 {
		t.Fatalf("initial PaginationCapHits = %d, want 0", got)
	}
	_, err := client.ListIssuesByStates(context.Background(), []string{"AI Ready"})
	if err == nil || !strings.Contains(err.Error(), "gitea issue pagination exceeded") {
		t.Fatalf("ListIssuesByStates err = %v, want pagination overflow error", err)
	}
	if len(logs) == 0 || !strings.Contains(logs[len(logs)-1], "gitea issue pagination exceeded") {
		t.Fatalf("logs = %#v, want pagination cap diagnostic", logs)
	}
	if got := client.PaginationCapHits(); got != 1 {
		t.Fatalf("PaginationCapHits = %d, want 1", got)
	}
}
```

- [ ] **Step 2: Run the test — expect it to fail**

```bash
go test -run TestTrackerClientListIssuesByStatesErrorsWhenIssuePaginationOverflows ./internal/gitea/...
```

Expected: FAIL with `ListIssuesByStates err = <nil>, want pagination overflow error` — the current silent-truncation behavior returns `nil`.

- [ ] **Step 3: Confirm the unchanged "exactly full max pages" test still passes**

```bash
go test -run TestTrackerClientListIssuesByStatesAllowsExactlyFullMaxPages ./internal/gitea/...
```

Expected: PASS — empty-probe clean exit is preserved by the upcoming code change.

---

## Task 2: Make the test pass (TDD green)

**Files:**
- Modify: `internal/gitea/tracker_client.go:161-167`

- [ ] **Step 1: Replace the overflow branch**

Locate the body of `listIssuesByStateLabel`. Replace:

```go
if page > listIssuesMaxPages {
    if len(batch) == 0 {
        return out, nil
    }
    c.recordPaginationCapHit(labelName)
    return out, nil
}
```

with:

```go
if page > listIssuesMaxPages {
    if !hasNext && len(batch) == 0 {
        return out, nil
    }
    c.recordPaginationCapHit(labelName)
    return nil, fmt.Errorf("gitea issue pagination exceeded %d pages for label %q", listIssuesMaxPages, labelName)
}
```

Two changes:

1. The clean-exit condition tightens from `len(batch) == 0` to `!hasNext && len(batch) == 0` (mirrors GitHub adapter). Even an empty probe page with a `Link: rel="next"` is now treated as overflow, not as a clean end-of-list.
2. The non-empty branch returns an error instead of a capped slice.

- [ ] **Step 2: Run the new test — expect it to pass**

```bash
go test -run TestTrackerClientListIssuesByStatesErrorsWhenIssuePaginationOverflows ./internal/gitea/...
```

Expected: PASS.

- [ ] **Step 3: Run the full Gitea package**

```bash
go test -race ./internal/gitea/...
```

Expected: every test passes.

---

## Task 3: Document in the runtime runbook

**Files:**
- Modify: `docs/runbooks/runtime-status.md` (append a subsection near the end, before any closing trailer)

- [ ] **Step 1: Add a subsection on tracker pagination overflow**

Add (placement: a logical neighbor of the "Event vocabulary" section — e.g. just after it):

```markdown
## Tracker pagination overflow

Both the GitHub adapter (`internal/tracker/github.go`) and the Gitea adapter
(`internal/gitea/tracker_client.go`) cap label-scoped issue listing at a small
number of pages so a pathological repository cannot spend the worker on a
single tracker call. When that cap is reached and the next page is still
non-empty (or has a `Link: rel="next"` header), the adapter:

1. increments `PaginationCapHits()` so the metric surfaces in operator
   dashboards;
2. logs `… issue pagination exceeded N pages for label "<label>" …`;
3. returns an error from `ListIssuesByStates` / `ListActiveIssues`.

The worker's multi-tracker aggregator (`cmd/worker/main.go`,
`multiTrackerRuntimeClient`) joins per-tracker errors via `errors.Join` and
continues with the other trackers' results, so an overflow on Gitea does not
stop a Linear/GitHub tracker on the same poll tick — but the per-tick error
is still reported and the affected tracker's candidate set is empty for that
tick.

### Triage

If you see this error in a poll tick:

- Identify the label from the error message (`label "<label>"`).
- Check the tracker for the count of issues currently carrying that label.
  If it exceeds the cap (Gitea: 1000 = 20 pages × 50/page; GitHub:
  similarly bounded), the project genuinely has too many active issues
  for the worker's cap to enumerate in one tick.
- Either reduce the active set on the tracker (move terminal issues out of
  active states) or, if the cap is wrong for your scale, raise the constant
  (`listIssuesMaxPages` / `githubMaxIssuePages`) in a follow-up PR — do not
  silence the error.

This was previously a silent truncation on Gitea (worker dispatched only the
first N pages and the rest were invisible). #225 changed it to match the
GitHub adapter's fail-loud semantics.
```

- [ ] **Step 2: Verify the doc still parses as Markdown (no orphan headers, no broken tables)**

```bash
head -200 docs/runbooks/runtime-status.md | grep -E '^(#|##|###) ' 
```

Expected: a sensible heading hierarchy with the new `## Tracker pagination overflow` appearing once.

---

## Task 4: Full local gate

**Files:** none.

- [ ] **Step 1: gofmt clean**

```bash
files="$(git ls-files '*.go' | xargs -r gofmt -l)"
if [ -n "$files" ]; then
  echo "GOFMT FAIL:"
  echo "$files"
  exit 1
fi
echo "gofmt: clean"
```

- [ ] **Step 2: go mod tidy clean**

```bash
go mod tidy
git diff --exit-code -- go.mod go.sum
```

Expected: no diff.

- [ ] **Step 3: Full test suite with race + covermode**

```bash
go test -race -covermode=atomic ./...
```

Expected: all packages pass. If `go test` complains `no such tool "covdata"`, follow memory `reference_homebrew_go_covdata.md` to rebuild covdata into GOTOOLDIR (this is a Homebrew Go bottle issue, not a code issue).

- [ ] **Step 4: Three binaries build**

```bash
for b in worker linear-poller gitea-poller; do
  go build -trimpath -ldflags="-s -w" -o /tmp/$b ./cmd/$b
done
rm -f /tmp/worker /tmp/linear-poller /tmp/gitea-poller
```

Expected: no compile errors.

---

## Task 5: Commit + dual-PR via fork

(Identical pattern to the #219 plan — fork CI PR + cross-fork upstream PR; see `docs/superpowers/plans/2026-05-21-release-workflow-fix.md` Tasks 4-7.)

- [ ] **Step 1: Stage + commit on the worktree branch**

```bash
git add internal/gitea/tracker_client.go internal/gitea/tracker_client_test.go docs/runbooks/runtime-status.md docs/superpowers/specs/2026-05-21-gitea-pagination-fail-loud-design.md docs/superpowers/plans/2026-05-21-gitea-pagination-fail-loud.md
git commit -m "$(cat <<'EOF'
fix(gitea): fail loud on label pagination overflow (#225)

listIssuesByStateLabel silently returned the first N pages when label-scoped
issue pagination exceeded listIssuesMaxPages (20 × 50 = 1000). Callers —
including the orchestrator poll tick and reconcile path — treated the
truncated list as authoritative, so issues beyond the cap were invisible
to dispatch and to terminal-cleanup reconciliation.

Mirror the GitHub adapter (internal/tracker/github.go:listIssuesForState):
tighten the clean-exit condition to `!hasNext && len(batch) == 0` and
return fmt.Errorf("gitea issue pagination exceeded …") with the metric
still firing. The multi-tracker aggregator
(multiTrackerRuntimeClient.ListIssuesByStates) already joins per-tracker
errors via errors.Join, so a Gitea overflow surfaces per-tick without
breaking parallel Linear/GitHub tracker results.

Test:
  * Existing capped-results test inverted + renamed to
    TestTrackerClientListIssuesByStatesErrorsWhenIssuePaginationOverflows,
    matching the GitHub adapter's naming.
  * Existing AllowsExactlyFullMaxPages empty-probe test unchanged.

Docs:
  * docs/runbooks/runtime-status.md gains a "Tracker pagination overflow"
    subsection describing the metric, log, error, and triage.

Refs #225
EOF
)"
```

- [ ] **Step 2: Push to fork**

```bash
gh auth switch -u xrf-9527
git push -u origin "$(git branch --show-current)"
```

- [ ] **Step 3: Open fork CI PR**

```bash
gh pr create \
  --repo xrf-9527/aiops-platform \
  --base main \
  --head "$(git branch --show-current)" \
  --title "[fork CI] fix(gitea): fail loud on label pagination overflow (#225)" \
  --body "Fork-internal PR to run CI for upstream xrf9268-hue/aiops-platform#225."
```

- [ ] **Step 4: Switch back; open upstream cross-fork PR (use `--no-maintainer-edit`)**

```bash
gh auth switch -u xrf9268-hue
gh pr create \
  --repo xrf9268-hue/aiops-platform \
  --base main \
  --head "xrf-9527:$(git branch --show-current)" \
  --title "fix(gitea): fail loud on label pagination overflow (#225)" \
  --body "Closes #225. Fork CI: <fork-pr-url>. See docs/superpowers/specs/2026-05-21-gitea-pagination-fail-loud-design.md." \
  --no-maintainer-edit
```

- [ ] **Step 5: gh-pr-follow-through on upstream PR**

Wait for CI green + Codex review (👀 reaction clearing on the trigger comment); address any review threads serially.

- [ ] **Step 6: Merge + cleanup**

```bash
gh pr merge <upstream-pr-number> --repo xrf9268-hue/aiops-platform --squash --match-head-commit <head-sha>
gh auth switch -u xrf-9527
gh api -X POST /repos/xrf-9527/aiops-platform/merge-upstream -f branch=main
gh pr close <fork-pr-number> --repo xrf-9527/aiops-platform --comment "Upstream merged; closing fork CI PR."
git push origin --delete <branch>
```

---

## Self-review checklist

- [x] Each step has the actual commands/code an engineer needs.
- [x] No `TBD` / `TODO` / "add appropriate error handling".
- [x] Spec coverage:
  - Fail-loud overflow → Task 2 Step 1.
  - Test asserts the error → Task 1 Step 1.
  - Metric preserved → assertion still in the inverted test.
  - Runbook updated → Task 3.
  - Full local gate → Task 4.
- [x] Multi-tracker resilience reasoning included in commit body so reviewers see why this doesn't break parallel trackers.
