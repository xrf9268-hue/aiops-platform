package workflow

import (
	"os"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/trackerprofiles"
)

// AgentEnvPassthroughDenyReason returns a non-empty reason when an environment
// variable name is denied independently of workflow configuration. Tracker
// secrets are adapter-owned and therefore handled by the config-aware helper.
func AgentEnvPassthroughDenyReason(name string) string {
	for _, secretName := range trackerprofiles.SecretEnvironmentNames() {
		if strings.EqualFold(strings.TrimSpace(name), secretName) {
			return "tracker provider secret must stay behind orchestrator tools"
		}
	}
	return ""
}

func AgentEnvPassthroughDenyReasonForConfig(name string, cfg Config) string {
	return AgentEnvPassthroughDenyReasonForConfigWithLookup(name, cfg, os.LookupEnv)
}

// AgentEnvPassthroughDenyReasonForConfigWithLookup is
// AgentEnvPassthroughDenyReasonForConfig with an injectable environment lookup
// so env construction tests can pin value-based tracker secret denial.
func AgentEnvPassthroughDenyReasonForConfigWithLookup(name string, cfg Config, lookup func(string) (string, bool)) string {
	name = strings.TrimSpace(name)
	if reason := AgentEnvPassthroughDenyReason(name); reason != "" {
		return reason
	}
	profile := cfg.Tracker.ProviderProfile()
	var secretNames, secretValues []string
	if profile != nil {
		secretNames = append(secretNames, profile.SecretEnvironmentNames()...)
	}
	if IsSupportedTrackerKind(cfg.Tracker.Kind) {
		resolved, err := trackerprofiles.Resolve(cfg.Tracker.Kind, cfg.Tracker.Provider, cfg.Repo.Owner, cfg.Repo.Name, lookup)
		if err == nil {
			profile = resolved
		}
	}
	if profile != nil {
		secretNames = append(secretNames, profile.SecretEnvironmentNames()...)
		secretValues = profile.SecretValues()
	}
	for _, secretName := range secretNames {
		if strings.EqualFold(name, secretName) {
			return "tracker provider secret must stay behind orchestrator tools"
		}
	}
	if value, ok := lookup(name); ok && value != "" {
		for _, secret := range secretValues {
			if value == secret {
				return "tracker provider secret value must stay behind orchestrator tools"
			}
		}
	}
	return ""
}
