package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// supportedTrackerKinds enumerates the tracker.kind values the orchestrator
// can actually drive a poll tick against. Matches the loader's parse-time
// validation (internal/workflow/loader.go) so the per-tick preflight does
// not silently accept kinds the rest of the system rejects.
var supportedTrackerKinds = map[string]struct{}{
	"linear": {},
	"gitea":  {},
	"github": {},
}

// validateDispatchPreflight is the SPEC §8.1 step 2 / §6.3 per-tick
// dispatch-preflight check. Startup loader validation only covers schema
// shape; this function re-validates the *resolved* runtime view of the
// workflow (post-`$VAR` expansion) on every poll tick so a token rotation,
// subprocess unset, or hot-edit of operator env that leaves a key empty
// at runtime surfaces as a typed `dispatch_preflight_failed` event rather
// than a tracker-specific transport error.
//
// Returned error joins every individual failure so the operator-visible
// event message carries the full reason set, not just the first one.
func validateDispatchPreflight(cfg workflow.Config) error {
	var errs []error
	kind := strings.TrimSpace(cfg.Tracker.Kind)
	if kind == "" {
		errs = append(errs, errors.New("tracker.kind missing"))
	} else if _, ok := supportedTrackerKinds[kind]; !ok {
		errs = append(errs, fmt.Errorf("tracker.kind unsupported: %q", kind))
	}
	if strings.TrimSpace(cfg.Tracker.APIKey) == "" {
		errs = append(errs, errors.New("tracker.api_key empty after $VAR resolution"))
	}
	if kind == "linear" && strings.TrimSpace(cfg.Tracker.ProjectSlug) == "" {
		errs = append(errs, errors.New("tracker.project_slug required for linear"))
	}
	if strings.TrimSpace(cfg.Codex.Command) == "" {
		errs = append(errs, errors.New("codex.command empty"))
	}
	return errors.Join(errs...)
}

// recordPreflightFailed submits an actor-serialized op that appends a
// `dispatch_preflight_failed` RuntimeEvent into the orchestrator state
// log. The event surfaces in /api/v1/state's recent_events so an operator
// can see the typed preflight reason rather than chasing a downstream
// tracker error message.
func (o *Orchestrator) recordPreflightFailed(ctx context.Context, reason error) error {
	if o == nil || reason == nil {
		return nil
	}
	return o.submit(ctx, opFunc(func(st *OrchestratorState) func() {
		st.RecordEvent(RuntimeEvent{
			Kind:    RuntimeEventDispatchPreflightFailed,
			Message: reason.Error(),
		})
		return nil
	}))
}
