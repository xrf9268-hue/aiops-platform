package gitea

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// dependsOnRegexp matches the documented Gitea cross-reference syntax
// `Depends on #N` (case-insensitive, anywhere in body) per SPEC §11.3.
// Gitea has no native priority field, so `Priority` on the normalized Issue
// stays zero for this tracker; the dispatch sort falls back to created_at
// per §8.2. Operators who want label-driven priority can wire it as a
// follow-up.
var dependsOnRegexp = regexp.MustCompile(`(?i)depends on #(\d+)`)

// giteaTrackerContextKey scopes context values set by this package so they
// cannot collide with keys from other packages.
type giteaTrackerContextKey int

const blockerCacheContextKey giteaTrackerContextKey = iota

// blockerLookupPolicy keeps candidate listing best-effort while letting the
// narrow refresh preserve rate-limit and deadline signals for its batch halt.
type blockerLookupPolicy int

const (
	blockerLookupBestEffort blockerLookupPolicy = iota
	blockerLookupHaltRefresh
)

// blockerCacheEntry memoizes one getIssueByNumber result so a `Depends on #N`
// blocker referenced by multiple source issues is fetched at most once per poll
// tick (#677). Only definitive results — a successful fetch or a 404 not-found —
// are cached; a transient fetch error is deliberately not cached so a later
// source issue referencing the same blocker can still retry it within the tick,
// preserving the original best-effort skip behavior without dropping a blocker
// on a transient error.
type blockerCacheEntry struct {
	issue Issue
	found bool
}

// withBlockerCache installs a fresh per-poll-tick blocker memoization cache on
// ctx. ListIssuesByStates calls it once per tick so buildBlockedBy dedupes
// blocker fetches across every source issue in the tick; the cache lives only
// for that context, so each blocker's state is re-read on the next tick.
func withBlockerCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, blockerCacheContextKey, map[int]blockerCacheEntry{})
}

func blockerCacheFrom(ctx context.Context) map[int]blockerCacheEntry {
	cache, _ := ctx.Value(blockerCacheContextKey).(map[int]blockerCacheEntry)
	return cache
}

// cachedIssueByNumber fetches blocker issue n via getIssueByNumber at most once
// per poll tick. A successful fetch and a definitive 404 are memoized; a
// transient error returns resolved=false without caching so a later source
// issue can retry. resolved distinguishes "the lookup answered" (found tells
// whether the issue exists) from "the lookup failed"; err preserves the lookup
// failure so callers can propagate global halt signals while ordinary failures
// still become fail-closed placeholders. When no per-tick cache is installed
// (non-poll callers), it falls back to a direct fetch.
func (c *TrackerClient) cachedIssueByNumber(ctx context.Context, cache map[int]blockerCacheEntry, n int) (issue Issue, found, resolved bool, err error) {
	if cache != nil {
		if entry, ok := cache[n]; ok {
			return entry.issue, entry.found, true, nil
		}
	}
	fetched, found, err := c.getIssueByNumber(ctx, n)
	if err != nil {
		return Issue{}, false, false, err
	}
	if found && fetched.Number != n {
		return Issue{}, false, false, fmt.Errorf("%w: Gitea blocker request #%d returned issue #%d", tracker.ErrIssueStateRefreshIncomplete, n, fetched.Number)
	}
	if cache != nil {
		cache[n] = blockerCacheEntry{issue: fetched, found: found}
	}
	return fetched, found, true, nil
}

// buildBlockedBy parses `Depends on #N` references from issue.Body and looks
// up each blocker's current workflow state via a follow-up Gitea fetch so the
// §8.2 Todo blocker rule can compare against the blocker's State. The
// per-poll-tick blocker cache (installed by ListIssuesByStates and the narrow
// refresh) ensures each distinct blocker is fetched at most once per tick
// across all source issues, instead of O(distinct refs) per source issue
// (#677).
//
// Reference resolution fails closed (#750 / PR #752 review): a transiently
// unresolvable reference becomes a placeholder ref with an empty State, which
// the shared tracker.BlockedByNonTerminal predicate treats as open — the Todo
// gate then blocks the candidate for this tick instead of dispatching past a
// blocker the lookup could not see; the next tick retries (the transient
// failure is deliberately not cached). A definitive 404 is a DELETED blocker:
// it can never become terminal, so blocking on it would starve the candidate
// forever — it drops out of the list instead.
func (c *TrackerClient) buildBlockedBy(ctx context.Context, body string, policy blockerLookupPolicy) ([]tracker.BlockerRef, error) {
	matches := dependsOnRegexp.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil, nil
	}
	cache := blockerCacheFrom(ctx)
	seen := map[int]struct{}{}
	var refs []tracker.BlockerRef
	for _, m := range matches {
		n, ok := parseUnseenBlockerNumber(m[1], seen)
		if !ok {
			continue
		}
		ref, ok, err := c.blockerRefForNumber(ctx, cache, n, policy)
		if err != nil {
			return nil, err
		}
		if ok {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

func parseUnseenBlockerNumber(raw string, seen map[int]struct{}) (int, bool) {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, false
	}
	if _, ok := seen[n]; ok {
		return 0, false
	}
	seen[n] = struct{}{}
	return n, true
}

// blockerRefForNumber resolves one `Depends on #N` reference: a resolved
// blocker carries its derived state, a transient lookup failure fails closed
// as an empty-state placeholder (logged — without that a persistently failing
// lookup would starve the candidate indistinguishably from a genuine open
// blocker), and a definitively deleted blocker reports ok=false to drop out.
func (c *TrackerClient) blockerRefForNumber(ctx context.Context, cache map[int]blockerCacheEntry, n int, policy blockerLookupPolicy) (tracker.BlockerRef, bool, error) {
	issue, found, resolved, lookupErr := c.cachedIssueByNumber(ctx, cache, n)
	if policy == blockerLookupHaltRefresh && lookupErr != nil && tracker.ShouldStopIssueStateRefresh(ctx, lookupErr) {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(lookupErr, ctxErr) {
			lookupErr = errors.Join(lookupErr, ctxErr)
		}
		return tracker.BlockerRef{}, false, lookupErr
	}
	if !resolved {
		if c.Logf != nil {
			c.Logf("gitea blocker lookup failed transiently for #%d; failing closed as an open placeholder", n)
		}
		return tracker.BlockerRef{Identifier: fmt.Sprintf("#%d", n)}, true, nil
	}
	if !found {
		return tracker.BlockerRef{}, false, nil
	}
	state, _ := IssueStateFromLabels(issue.Labels, DefaultStateLabelMappings())
	return tracker.BlockerRef{
		ID:         giteaIssueID(issue),
		Identifier: fmt.Sprintf("#%d", issue.Number),
		State:      state,
	}, true, nil
}
