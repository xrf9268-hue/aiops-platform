package workflow

import "strings"

// NormalizeStateConcurrencyLimits canonicalizes the per-state concurrency
// cap map (`agent.max_concurrent_agents_by_state`). It is the single
// source of truth shared between the loader's initial-load path
// (internal/workflow/loader.go) and the orchestrator's snapshot-build
// path (internal/orchestrator/workflow_runtime.go); both call this
// helper so a `WORKFLOW.md` reload cannot produce a differently-shaped
// in-memory map than the initial load did.
//
// Semantics (closes #294):
//   - Whitespace/case-folded keys: state names are normalized via
//     [NormalizeStateConcurrencyKey] so `"In Progress"`, `"in progress"`,
//     and `"  in progress "` all map to the same bucket.
//   - Empty / whitespace-only keys are DROPPED. The orchestrator looks
//     up caps by `NormalizeStateConcurrencyKey(stateName)`, which can
//     never produce the empty string from a real tracker state, so any
//     preserved empty-key entry would be permanently dead.
//   - Non-positive limits (`<= 0`) are DROPPED. SPEC §5.3.5 caps are a
//     positive integer; `0` would silently mean "never dispatch this
//     state" but operators expressing that intent would set the issue
//     to a different active-states list, not a `0` cap.
//
// Both rules drop entries the orchestrator could not use anyway,
// trading a little operator-feedback granularity for shape parity
// across the load/reload boundary.
func NormalizeStateConcurrencyLimits(limits map[string]int) map[string]int {
	if len(limits) == 0 {
		return nil
	}
	out := make(map[string]int, len(limits))
	for state, limit := range limits {
		key := NormalizeStateConcurrencyKey(state)
		if key == "" || limit <= 0 {
			continue
		}
		out[key] = limit
	}
	return out
}

// NormalizeStateConcurrencyKey canonicalizes a single tracker state name
// for use as a key in the per-state concurrency cap map. The shape
// (`strings.ToLower` + trim + space→underscore) matches how the
// orchestrator looks up caps when a worker session is dispatching, so
// the keys produced by [NormalizeStateConcurrencyLimits] line up with
// the runtime lookup path.
func NormalizeStateConcurrencyKey(state string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(state)), " ", "_")
}
