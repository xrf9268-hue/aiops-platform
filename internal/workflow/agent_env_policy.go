package workflow

import (
	"os"
	"strings"
)

var deniedAgentEnvPassthroughNames = map[string]struct{}{
	"GH_TOKEN":        {},
	"GITEA_API_TOKEN": {},
	"GITEA_TOKEN":     {},
	"GITHUB_PAT":      {},
	"GITHUB_TOKEN":    {},
	"LINEAR_API_KEY":  {},
	"LINEAR_TOKEN":    {},
}

// AgentEnvPassthroughDenyReason returns a non-empty reason when an environment
// variable name must not be exposed to agent subprocesses. Model-runtime
// credentials such as OPENAI_API_KEY remain opt-in; tracker/repo API tokens
// stay behind the orchestrator-owned tool/proxy boundary.
func AgentEnvPassthroughDenyReason(name string) string {
	normalized := strings.ToUpper(strings.TrimSpace(name))
	if _, denied := deniedAgentEnvPassthroughNames[normalized]; denied {
		return "tracker/API token must stay behind orchestrator tools"
	}
	return ""
}

func AgentEnvPassthroughDenyReasonForConfig(name string, cfg Config) string {
	name = strings.TrimSpace(name)
	if reason := AgentEnvPassthroughDenyReason(name); reason != "" {
		return reason
	}
	if cfg.Tracker.apiKeyEnvVar != "" && strings.EqualFold(name, cfg.Tracker.apiKeyEnvVar) {
		return "tracker.api_key environment variable must stay behind orchestrator tools"
	}
	if cfg.Tracker.APIKey != "" {
		if value, ok := os.LookupEnv(name); ok && value == cfg.Tracker.APIKey {
			return "tracker.api_key value must stay behind orchestrator tools"
		}
	}
	return ""
}
