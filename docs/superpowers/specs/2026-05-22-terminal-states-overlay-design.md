# Drop hardcoded terminal-state overlay — design

**Date:** 2026-05-22
**Issue:** [#232](https://github.com/xrf9268-hue/aiops-platform/issues/232)

## Problem

`filterEligibleCandidates` (`internal/orchestrator/poller.go:317`) unions the operator-configured `terminal_states` with a hardcoded English set `{Done, Canceled, Cancelled, Closed, Duplicate}`. Operators can only **add** terminal states; they can never **remove** any of the hardcoded five. The same merged set flows into the `Todo` blocker rule (`todoIssueBlockedByOpenDependency`), so blocker semantics also ignore explicit subset configs.

This violates SPEC §5.3.1, which makes `terminal_states` fully operator-configurable with a *default* of `["Closed", "Cancelled", "Canceled", "Duplicate", "Done"]` — a default, not a floor.

## Decision

Two coordinated changes:

1. **Drop the overlay** in `filterEligibleCandidates`. Trust the configured `terminal_states`.
2. **Expand `DefaultConfig().Tracker.TerminalStates`** from `["Done", "Canceled"]` to the SPEC §5.3.1 default `["Done", "Canceled", "Cancelled", "Closed", "Duplicate"]`. This preserves back-compat for omit-config deployments — they still see the same 5-state coverage they got via the overlay, just through the proper "default config" channel instead of an unconditional union.

After this PR:

- An explicit `terminal_states: ["Released"]` is honored exactly. `Closed` issues now flow through dispatch (where SPEC §8.2 active-state check still excludes them if Closed isn't an active state).
- An omit-config deployment (no `terminal_states` in `WORKFLOW.md`) gets the SPEC 5-state default — behavior unchanged.
- The `Todo` blocker rule observes only the operator-configured terminal_states.

### Why both changes (and not just dropping the overlay)

The issue body's literal fix is "drop the overlay" and asserts that "the SPEC default already populates the same English set when the workflow omits the field." But `DefaultConfig` currently has only 2 states (`Done, Canceled`). Dropping the overlay alone would silently regress omit-config deployments: a `Todo` issue blocked by a `Closed` blocker would suddenly stay blocked because Closed is no longer considered terminal under the 2-state default. Expanding the default to the SPEC 5-state set fixes the underlying SPEC-alignment gap and prevents that regression.

### Why not deprecate `DefaultConfig` and require explicit config

That's a much bigger change with operator-visible breakage. Out of scope.

### Why not log a warning when operators omit the default English set

Suggested by the issue body as "additional safety net". Adds noise for legitimate non-English configs (Japanese, Spanish, Russian) and is hard to phrase without sounding paternalistic. Skip.

## What changes

| File | Change |
| --- | --- |
| `internal/orchestrator/poller.go:317-321` | Drop the `for state := range normalizedStates([]string{"Done", "Canceled", "Cancelled", "Closed", "Duplicate"})` block. `terminal` is now exactly `normalizedStates(terminalStates)`. |
| `internal/workflow/config.go:376` | Expand `TerminalStates: []string{"Done", "Canceled"}` to `TerminalStates: []string{"Done", "Canceled", "Cancelled", "Closed", "Duplicate"}` to match SPEC §5.3.1. |
| `internal/orchestrator/poller_test.go` (or wherever filterEligibleCandidates is tested) | Add `TestFilterEligibleCandidatesHonorsConfiguredTerminalStates`: workflow with `terminal_states: ["Released"]`, issue in `Closed` state passes the filter. |
| Same file | Add `TestFilterEligibleCandidatesTodoBlockerObservesOnlyConfiguredTerminalStates`: `Todo` issue with a `Closed` blocker; when configured terminal_states is `["Done"]`, the blocker counts as open → issue is blocked. |

## Non-goals

- Don't audit other callers of `terminal_states` for SPEC alignment — `internal/gitea/tracker_client.go:340` falls back to `DefaultConfig().Tracker.TerminalStates`, which now contains the SPEC 5-state set; behavior preserved.
- Don't expose a separate "force-treat-as-terminal" config knob — operators who want extra safety states just list them.
- Don't add a DEVIATIONS.md row — this PR closes the deviation.

## Acceptance criteria

- [ ] `filterEligibleCandidates` no longer unions in any hardcoded English set.
- [ ] `DefaultConfig().Tracker.TerminalStates` matches SPEC §5.3.1 5-state default.
- [ ] Existing tests pass.
- [ ] New test: explicit `terminal_states: [Released]` does not filter `Closed`.
- [ ] New test: `Todo` blocker rule observes only operator-configured terminal_states.

## Refs

- Issue: https://github.com/xrf9268-hue/aiops-platform/issues/232
- Code: `internal/orchestrator/poller.go:317-329, 342-`
- DefaultConfig: `internal/workflow/config.go:370-385`
- SPEC §5.3.1 (terminal_states default + override semantics), §8.2 (Todo blocker rule)
