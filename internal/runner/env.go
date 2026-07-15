package runner

import (
	"os"

	"github.com/xrf9268-hue/aiops-platform/internal/envpolicy"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
	"github.com/xrf9268-hue/aiops-platform/internal/workspace"
)

var baselineAgentEnvAllowlist = []string{
	"PATH",
	"HOME",
	"USER",
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"TZ",
	"TERM",
}

// Codex resolves the workspace-write TMPDIR root from its own process env, so
// the app-server must see the same temp root used by worker-injected Go caches.
var codexAppServerEnvAllowlist = append(append([]string{}, baselineAgentEnvAllowlist...), "CODEX_HOME", "TMPDIR")

var agentLoginPATH = workspace.LoginPATH

func agentEnv(passthrough []string, cfg workflow.Config) []string {
	return agentEnvWithLookup(passthrough, cfg, os.LookupEnv, agentLoginPATH)
}

func codexAppServerEnv(passthrough []string, cfg workflow.Config) []string {
	return codexAppServerEnvWithLookup(passthrough, cfg, os.LookupEnv, agentLoginPATH)
}

// AgentEnvForPreflight returns the same sanitized environment used by the
// selected agent runner so operator preflights cannot pass on worker-only
// credentials that the agent subprocess will never inherit.
func AgentEnvForPreflight(agent string, cfg workflow.Config) []string {
	switch agent {
	case NameCodexAppServer:
		return codexAppServerEnv(cfg.Codex.EnvPassthrough, cfg)
	case "claude":
		return agentEnv(cfg.Claude.EnvPassthrough, cfg)
	default:
		return agentEnv(nil, cfg)
	}
}

func agentEnvWithLookup(passthrough []string, cfg workflow.Config, lookup func(string) (string, bool), loginPath func() string) []string {
	return envpolicy.BuildSanitizedEnv(baselineAgentEnvAllowlist, passthrough, cfg, lookup, loginPath)
}

func codexAppServerEnvWithLookup(passthrough []string, cfg workflow.Config, lookup func(string) (string, bool), loginPath func() string) []string {
	return envpolicy.BuildSanitizedEnv(codexAppServerEnvAllowlist, passthrough, cfg, lookup, loginPath)
}
