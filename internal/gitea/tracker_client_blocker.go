package gitea

import (
	"context"
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
// whether the issue exists) from "the lookup failed" — callers must not treat
// a transient failure as a definitive absence. When no per-tick cache is
// installed (non-poll callers), it falls back to a direct fetch.
func (c *TrackerClient) cachedIssueByNumber(ctx context.Context, cache map[int]blockerCacheEntry, n int) (issue Issue, found, resolved bool) {
	if cache != nil {
		if entry, ok := cache[n]; ok {
			return entry.issue, entry.found, true
		}
	}
	fetched, found, err := c.getIssueByNumber(ctx, n)
	if err != nil {
		return Issue{}, false, false
	}
	if cache != nil {
		cache[n] = blockerCacheEntry{issue: fetched, found: found}
	}
	return fetched, found, true
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
func (c *TrackerClient) buildBlockedBy(ctx context.Context, body string) []tracker.BlockerRef {
	matches := dependsOnRegexp.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}
	cache := blockerCacheFrom(ctx)
	seen := map[int]struct{}{}
	var refs []tracker.BlockerRef
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		if ref, ok := c.blockerRefForNumber(ctx, cache, n); ok {
			refs = append(refs, ref)
		}
	}
	return refs
}

// blockerRefForNumber resolves one `Depends on #N` reference: a resolved
// blocker carries its derived state, a transient lookup failure fails closed
// as an empty-state placeholder (logged — without that a persistently failing
// lookup would starve the candidate indistinguishably from a genuine open
// blocker), and a definitively deleted blocker reports ok=false to drop out.
func (c *TrackerClient) blockerRefForNumber(ctx context.Context, cache map[int]blockerCacheEntry, n int) (tracker.BlockerRef, bool) {
	issue, found, resolved := c.cachedIssueByNumber(ctx, cache, n)
	if !resolved {
		if c.Logf != nil {
			c.Logf("gitea blocker lookup failed transiently for #%d; failing closed as an open placeholder", n)
		}
		return tracker.BlockerRef{Identifier: fmt.Sprintf("#%d", n)}, true
	}
	if !found {
		return tracker.BlockerRef{}, false
	}
	state, _ := IssueStateFromLabels(issue.Labels, DefaultStateLabelMappings())
	return tracker.BlockerRef{
		ID:         giteaIssueID(issue),
		Identifier: fmt.Sprintf("#%d", issue.Number),
		State:      state,
	}, true
}
