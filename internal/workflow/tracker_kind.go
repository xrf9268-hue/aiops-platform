package workflow

// supportedTrackerKinds enumerates the tracker integrations the platform
// actually wires up today. Anything outside this set would parse as a typed
// config but could not be claimed by the worker.
var supportedTrackerKinds = map[string]struct{}{
	"gitea":  {},
	"github": {},
	"linear": {},
}

// IsSupportedTrackerKind reports whether kind is a tracker integration the
// platform wires up today. It is the single source of truth for the supported
// set: the loader's schema validation and the orchestrator's per-tick dispatch
// preflight both consult it, so the two cannot drift.
func IsSupportedTrackerKind(kind string) bool {
	_, ok := supportedTrackerKinds[kind]
	return ok
}
