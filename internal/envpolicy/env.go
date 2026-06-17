package envpolicy

import (
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// BuildSanitizedEnv returns an explicit cmd.Env list from the allowed baseline
// plus workflow-configured passthrough names. Tracker credentials are rejected
// before lookup values are copied into a child process environment.
func BuildSanitizedEnv(
	baseline []string,
	passthrough []string,
	cfg workflow.Config,
	lookup func(string) (string, bool),
	loginPath func() string,
) []string {
	b := envBuilder{
		cfg:    cfg,
		lookup: lookup,
		path:   loginPath,
		seen:   make(map[string]struct{}, len(baseline)+len(passthrough)),
		env:    make([]string, 0, len(baseline)+len(passthrough)),
	}
	b.addNames(baseline)
	b.addNames(passthrough)
	return b.env
}

type envBuilder struct {
	cfg    workflow.Config
	lookup func(string) (string, bool)
	path   func() string
	seen   map[string]struct{}
	env    []string
}

func (b *envBuilder) addNames(names []string) {
	for _, name := range names {
		b.add(name)
	}
}

func (b *envBuilder) add(raw string) {
	name, ok := b.allowedName(raw)
	if !ok {
		return
	}
	value, ok := b.value(name)
	if !ok {
		return
	}
	b.seen[name] = struct{}{}
	b.env = append(b.env, name+"="+value)
}

func (b *envBuilder) allowedName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" || strings.Contains(name, "=") {
		return "", false
	}
	if _, ok := b.seen[name]; ok {
		return "", false
	}
	if workflow.AgentEnvPassthroughDenyReasonForConfigWithLookup(name, b.cfg, b.lookup) != "" {
		return "", false
	}
	return name, true
}

func (b *envBuilder) value(name string) (string, bool) {
	if name == "PATH" && b.path != nil {
		if value := b.path(); value != "" {
			return value, true
		}
	}
	return b.lookup(name)
}
