package orchestrator

// poller_candidates.go holds the poll tick's candidate-selection pipeline:
// eligibility filtering, ordering, overflow carry-over, and the pre-dispatch
// tracker revalidation that keeps a long tick from dispatching stale
// candidates (#740). PollOnce itself lives in poller.go.

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// dispatchRevalidationTimeout caps the pre-dispatch candidate revalidation
// fetch. PollOnce's ctx carries no deadline of its own, so an unresponsive
// tracker would otherwise pin the poll loop inside the revalidation call
// indefinitely.
var dispatchRevalidationTimeout = 45 * time.Second

// revalidateDispatchCandidates refreshes the tracker state of every candidate
// the dispatch loop could actually spawn this tick and drops candidates that
// are no longer dispatchable, porting upstream revalidate_issue_for_dispatch
// (orchestrator.ex:909-924, 995-1013). The candidate listing is read at tick
// start, and the reconcile pass between that read and the dispatch loop can
// block on worker-exit waits long enough for the listing to go stale — a
// freed slot then burns a full agent run on an ineligible issue until the
// next reconcile pass corrects it (#740).
//
// Candidates the claim gate would deny anyway (running, blocked, queued
// failure retries, not-yet-due continuations) are dropped without a refresh —
// the same trim upstream gets from should_dispatch_issue? running before
// revalidate_issue_for_dispatch — so the refresh cost stays proportional to
// what can spawn, not to the active listing. A refreshed candidate is dropped
// when the refresh omits it (upstream {:skip, :missing}; the Gitea adapter
// also omits issues whose aiops/* state labels were stripped), when its
// refreshed state left the configured active set, or when its refreshed
// labels no longer satisfy the SPEC §6.4 required-labels gate, or when it is
// a Todo issue whose refreshed blockers are not all terminal (upstream
// retry_candidate_issue?'s !todo_issue_blocked_by_non_terminal?,
// orchestrator.ex:1602-1604, #750) — the same gates the retry-fire path
// re-applies at fire time via eligibleActiveIssueLister. Survivors carry the
// refreshed state, labels, and blocker data so the per-state capacity gate
// and the spawned worker see live tracker data, mirroring upstream's
// dispatch of the refreshed issue. On fetch failure the candidates the
// refresh did return still dispatch and the rest are skipped (upstream skips
// dispatch on refresh error); the next tick retries from a fresh listing.
// Pollers without a narrow-refresh state tracker skip revalidation.
func (p *Poller) revalidateDispatchCandidates(ctx context.Context, candidates []tracker.Issue) ([]tracker.Issue, error) {
	refresher, ok := p.stateTracker.(IssueStateRefresher)
	if !ok || len(candidates) == 0 {
		return candidates, nil
	}
	claimable, refs, err := p.claimableDispatchRefs(ctx, candidates)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, nil
	}
	fetchCtx, cancel := context.WithTimeout(ctx, dispatchRevalidationTimeout)
	defer cancel()
	statesByID, fetchErr := fetchIssueStates(fetchCtx, refresher, refs)
	if fetchErr != nil {
		fetchErr = fmt.Errorf("revalidate dispatch candidates: %w", fetchErr)
	}
	return p.filterRevalidatedCandidates(candidates, claimable, statesByID), fetchErr
}

// claimableDispatchRefs asks the orchestrator which candidates the dispatch
// claim gate would currently allow and returns the narrow-refresh refs for
// exactly that subset.
func (p *Poller) claimableDispatchRefs(ctx context.Context, candidates []tracker.Issue) (map[IssueID]struct{}, []tracker.IssueRef, error) {
	ids := make([]IssueID, 0, len(candidates))
	for _, issue := range candidates {
		ids = append(ids, IssueID(issue.ID))
	}
	claimable, err := p.orchestrator.DispatchClaimableIssueIDs(ctx, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("revalidate dispatch candidates: %w", err)
	}
	refs := make([]tracker.IssueRef, 0, len(claimable))
	for _, issue := range candidates {
		if _, ok := claimable[IssueID(issue.ID)]; ok {
			refs = append(refs, tracker.IssueRef{ID: issue.ID, Identifier: issue.Identifier})
		}
	}
	return claimable, refs, nil
}

// filterRevalidatedCandidates keeps the claimable candidates whose refreshed
// tracker row still passes the dispatch gates, carrying the refreshed state
// and labels onto each survivor. Dropping is skip-only: a dropped candidate's
// queued continuation (if any) is released by the reconcile pass's
// vanished-continuation sweep when the row is missing, or by the inactive
// reconcile when the row is present but ineligible — never here, so the
// release stays single-sourced.
func (p *Poller) filterRevalidatedCandidates(candidates []tracker.Issue, claimable map[IssueID]struct{}, statesByID map[string]tracker.IssueState) []tracker.Issue {
	activeStateKeys := normalizedStates(p.reconcile.ActiveStates)
	terminalStateKeys := normalizedStates(p.reconcile.TerminalStates)
	kept := make([]tracker.Issue, 0, len(claimable))
	for _, issue := range candidates {
		if _, ok := claimable[IssueID(issue.ID)]; !ok {
			continue
		}
		if revalidated, keep := revalidatedCandidate(issue, statesByID, activeStateKeys, terminalStateKeys, p.reconcile.RequiredLabels); keep {
			kept = append(kept, revalidated)
		}
	}
	return kept
}

// revalidatedCandidate applies the per-issue revalidation verdict: missing
// from the refresh (upstream {:skip, :missing}), refreshed out of the active
// set, refreshed past the SPEC §6.4 required-labels gate, or a refreshed Todo
// issue whose blockers reopened all drop the candidate; otherwise the
// refreshed state, labels, and blocker data are carried onto it. The blocker
// recheck matches upstream retry_candidate_issue?'s
// !todo_issue_blocked_by_non_terminal? on the refreshed issue
// (orchestrator.ex:1602-1604, #750); when the refresh supplies no blocker
// knowledge (nil BlockedBy — see tracker.IssueState), the gate re-runs on the
// listing-time blockers, which the tick-start eligibility filter already
// passed, so it cannot newly drop the candidate.
func revalidatedCandidate(issue tracker.Issue, statesByID map[string]tracker.IssueState, activeStateKeys, terminalStateKeys map[string]struct{}, requiredLabels []string) (tracker.Issue, bool) {
	refreshed, ok := statesByID[issue.ID]
	if !ok || strings.TrimSpace(refreshed.State) == "" {
		logStaleDispatchSkipped(issue, "", "missing_from_refresh")
		return tracker.Issue{}, false
	}
	issue.State = refreshed.State
	issue.Labels = refreshed.Labels
	if refreshed.BlockedBy != nil {
		issue.BlockedBy = refreshed.BlockedBy
	}
	if !isActiveTrackerState(issue.State, activeStateKeys) {
		logStaleDispatchSkipped(issue, issue.State, "state_left_active_set")
		return tracker.Issue{}, false
	}
	if !issueHasRequiredLabels(issue, requiredLabels) {
		logStaleDispatchSkipped(issue, issue.State, "required_labels_missing")
		return tracker.Issue{}, false
	}
	if todoIssueBlockedByOpenDependency(issue, terminalStateKeys) {
		logStaleDispatchSkipped(issue, issue.State, "todo_blocker_not_terminal")
		return tracker.Issue{}, false
	}
	return issue, true
}

func logStaleDispatchSkipped(issue tracker.Issue, state, reason string) {
	log.Printf("event=stale_dispatch_skipped issue=%q state=%q reason=%s", issue.Identifier, state, reason)
}

func filterIssuesNotInMap(issues []tracker.Issue, excluded map[string]tracker.Issue) []tracker.Issue {
	if len(excluded) == 0 {
		return issues
	}
	out := make([]tracker.Issue, 0, len(issues))
	for _, issue := range issues {
		if _, ok := excluded[issue.ID]; ok {
			continue
		}
		out = append(out, issue)
	}
	return out
}

func filterEligibleCandidates(issues []tracker.Issue, terminalStates, requiredLabels []string) []tracker.Issue {
	// Honor exactly what the caller supplied per SPEC §5.3.1. Callers that
	// want the SPEC 5-state default get it from workflow.DefaultConfig at
	// construction time (NewPoller seeds it; workflow.Load supplies it for
	// omitted YAML). An explicit empty slice from
	// NewPollerWithReconciliation disables the blocker rule entirely, which
	// is the operator's call.
	terminal := normalizedStates(terminalStates)
	out := make([]tracker.Issue, 0, len(issues))
	for _, issue := range issues {
		if !issueHasRequiredCandidateFields(issue) {
			continue
		}
		if todoIssueBlockedByOpenDependency(issue, terminal) {
			continue
		}
		if !issueHasRequiredLabels(issue, requiredLabels) {
			continue
		}
		out = append(out, issue)
	}
	return out
}

// issueHasRequiredLabels reports whether issue carries every label in required
// (SPEC §4.1.1 / §6.4). See labelsSatisfyRequired for the matching semantics.
func issueHasRequiredLabels(issue tracker.Issue, required []string) bool {
	return labelsSatisfyRequired(issue.Labels, required)
}

// labelsSatisfyRequired reports whether labels contains every entry in required
// (SPEC §4.1.1 / §6.4). Both sides are trimmed and lowercased so matching is
// case-insensitive regardless of tracker label casing. Empty required disables
// the gate (returns true); a blank required entry ("") can never match a
// tracker's non-empty labels, so it blocks every issue as SPEC mandates. Shared
// by the dispatch/reconcile predicate and the §16.5 per-turn continue gate so
// they cannot diverge.
func labelsSatisfyRequired(labels, required []string) bool {
	if len(required) == 0 {
		return true
	}
	have := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		have[strings.ToLower(strings.TrimSpace(label))] = struct{}{}
	}
	for _, label := range required {
		if _, ok := have[strings.ToLower(strings.TrimSpace(label))]; !ok {
			return false
		}
	}
	return true
}

func issueHasRequiredCandidateFields(issue tracker.Issue) bool {
	return strings.TrimSpace(issue.ID) != "" &&
		strings.TrimSpace(issue.Identifier) != "" &&
		strings.TrimSpace(issue.Title) != "" &&
		strings.TrimSpace(issue.State) != ""
}

func todoIssueBlockedByOpenDependency(issue tracker.Issue, terminalStates map[string]struct{}) bool {
	if !strings.EqualFold(strings.TrimSpace(issue.State), "Todo") {
		return false
	}
	return tracker.BlockedByNonTerminal(issue.BlockedBy, terminalStates)
}

func sortCandidates(issues []tracker.Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		left, right := issues[i], issues[j]
		leftPriority := linearPrioritySortKey(left.Priority)
		rightPriority := linearPrioritySortKey(right.Priority)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		if compareCreatedAt(left.CreatedAt, right.CreatedAt) < 0 {
			return true
		}
		if compareCreatedAt(left.CreatedAt, right.CreatedAt) > 0 {
			return false
		}
		return left.Identifier < right.Identifier
	})
}

func compareCreatedAt(left, right time.Time) int {
	switch {
	case left.IsZero() && right.IsZero():
		return 0
	case left.IsZero():
		return 1
	case right.IsZero():
		return -1
	case left.Before(right):
		return -1
	case left.After(right):
		return 1
	default:
		return 0
	}
}

func linearPrioritySortKey(priority int) int {
	if priority == 0 {
		return 1 << 30
	}
	return priority
}

func mergeOverflowCandidates(overflow, fresh []tracker.Issue) []tracker.Issue {
	if len(overflow) == 0 {
		return fresh
	}
	candidates := make([]tracker.Issue, 0, len(overflow)+len(fresh))
	freshByID := make(map[string]tracker.Issue, len(fresh))
	seen := make(map[string]struct{}, len(overflow)+len(fresh))
	for _, issue := range fresh {
		if issue.ID == "" {
			continue
		}
		freshByID[issue.ID] = issue
	}
	for _, issue := range overflow {
		freshIssue, ok := freshByID[issue.ID]
		if !ok {
			continue
		}
		seen[issue.ID] = struct{}{}
		candidates = append(candidates, freshIssue)
	}
	for _, issue := range fresh {
		if _, ok := seen[issue.ID]; ok {
			continue
		}
		candidates = append(candidates, issue)
	}
	return candidates
}
