package runner

import (
	"os"
	"strings"

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

var agentLoginPATH = workspace.LoginPATH

func agentEnv(passthrough []string, cfg workflow.Config) []string {
	return agentEnvWithLookup(passthrough, cfg, os.LookupEnv, agentLoginPATH)
}

func agentEnvWithLookup(passthrough []string, cfg workflow.Config, lookup func(string) (string, bool), loginPath func() string) []string {
	seen := make(map[string]struct{}, len(baselineAgentEnvAllowlist)+len(passthrough))
	env := make([]string, 0, len(baselineAgentEnvAllowlist)+len(passthrough))
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || strings.Contains(name, "=") {
			return
		}
		if workflow.AgentEnvPassthroughDenyReasonForConfig(name, cfg) != "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		if name == "PATH" {
			if value := loginPath(); value != "" {
				env = append(env, "PATH="+value)
				return
			}
		}
		if value, ok := lookup(name); ok {
			env = append(env, name+"="+value)
		}
	}
	for _, name := range baselineAgentEnvAllowlist {
		add(name)
	}
	for _, name := range passthrough {
		add(name)
	}
	return env
}
