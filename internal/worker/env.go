package worker

import (
	"fmt"
	"log"
	"os"
)

// EnvResolution records which environment variable supplied a worker
// configuration value, so callers can both read the value and surface a
// deprecation/misconfiguration warning when it came from a non-canonical name.
//
// Worker-owned operational env vars use the AIOPS_ prefix as the single
// convention (AIOPS_WORKSPACE_ROOT / AIOPS_MIRROR_ROOT / AIOPS_WORKFLOW_PATH).
// The legacy unprefixed forms are still honored as deprecated aliases so
// existing deployments do not break, but using one emits a warning instead of
// silently falling back to the code default (#368).
type EnvResolution struct {
	// Canonical is the preferred (AIOPS_-prefixed) variable name.
	Canonical string
	// Value is the resolved value; empty when none of the names is set.
	Value string
	// UsedName is the variable actually read; empty when nothing was set.
	UsedName string
}

// Warning returns a deprecation notice when the value came from a non-canonical
// alias, or the empty string when the canonical name (or nothing) was used.
func (r EnvResolution) Warning() string {
	if r.UsedName == "" || r.UsedName == r.Canonical {
		return ""
	}
	return fmt.Sprintf("env %s is deprecated; rename it to %s (its value is being honored for now)", r.UsedName, r.Canonical)
}

// LogWarning emits a structured deprecation warning (SPEC §13.1) when the
// value came from a non-canonical alias.
func (r EnvResolution) LogWarning() {
	if r.Warning() == "" {
		return
	}
	log.Printf("event=config_env_deprecated_alias used=%s canonical=%s", r.UsedName, r.Canonical)
}

// ResolveEnv reads canonical first, then each alias in declaration order; the
// first non-empty value wins. An unset or empty variable is skipped so an
// explicitly blank alias never masks a later non-empty one — and, crucially, a
// wrong-prefix variant is honored with a warning rather than silently dropped.
func ResolveEnv(canonical string, aliases ...string) EnvResolution {
	if v := os.Getenv(canonical); v != "" {
		return EnvResolution{Canonical: canonical, Value: v, UsedName: canonical}
	}
	for _, alias := range aliases {
		if v := os.Getenv(alias); v != "" {
			return EnvResolution{Canonical: canonical, Value: v, UsedName: alias}
		}
	}
	return EnvResolution{Canonical: canonical}
}
