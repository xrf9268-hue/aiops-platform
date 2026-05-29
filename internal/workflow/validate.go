package workflow

import (
	"fmt"
	"net"
	"strings"
)

func hasExplicitServiceRoute(route ServiceTrackerRouteConfig) bool {
	return strings.TrimSpace(route.ProjectSlug) != "" ||
		strings.TrimSpace(route.TeamKey) != "" ||
		len(route.Labels) > 0
}

// validateConfig enforces the required-field and enum constraints that the
// typed YAML decoder cannot express on its own. It runs after expandConfig so
// env-var indirections (e.g. `clone_url: $REPO_URL`) are evaluated before
// non-empty checks, and every error includes the workflow file path and the
// offending field/value so operators can fix the source rather than chasing
// runtime symptoms (issue #9).
//
// It runs the per-section validators in a fixed order and returns the first
// failure. The order is load-bearing: a config that is invalid in several ways
// surfaces the earliest validator's message, so the operator-facing precedence
// is exactly the slice order below. tracker.kind is validated first because
// later validators branch on it (SPEC §6.4). Add new checks to the validator
// whose slot preserves that precedence rather than appending blindly.
func validateConfig(path string, cfg Config) error {
	for _, validate := range []func(string, Config) error{
		validateTrackerAndRepo,
		validateLinearServiceRoutes,
		validateServices,
		validateSupportedValues,
		validateSandbox,
		validateServerPort,
		validateCodexClaude,
		validateAgentLimits,
		validateTimeouts,
	} {
		if err := validate(path, cfg); err != nil {
			return err
		}
	}
	return nil
}

// validateTrackerAndRepo enforces the tracker.kind/repo.clone_url
// prerequisites. tracker.kind is REQUIRED per SPEC §6.4 and is checked before
// any branch that reads cfg.Tracker.Kind so an operator who omits the field
// sees the SPEC contract first rather than a follow-on error like
// "repo.clone_url is required unless tracker.kind is linear".
func validateTrackerAndRepo(path string, cfg Config) error {
	if strings.TrimSpace(cfg.Tracker.Kind) == "" {
		return fmt.Errorf("%s: tracker.kind is required per SPEC §6.4 (allowed: gitea, github, linear)", path)
	}
	if strings.TrimSpace(cfg.Repo.CloneURL) != "" {
		return nil
	}
	if len(cfg.Services) == 0 {
		return fmt.Errorf("%s: repo.clone_url is required", path)
	}
	if cfg.Tracker.Kind != "linear" {
		return fmt.Errorf("%s: repo.clone_url is required unless tracker.kind is linear and services provide routed repos", path)
	}
	for i, service := range cfg.Services {
		if strings.TrimSpace(cfg.Tracker.ProjectSlug) == "" && strings.TrimSpace(service.Tracker.ProjectSlug) == "" {
			return fmt.Errorf("%s: services[%d].tracker.project_slug or tracker.project_slug is required for service-only Linear workflows", path, i)
		}
	}
	return nil
}

// validateLinearServiceRoutes enforces the Linear-only routing predicates so
// a misconfigured service is rejected at load time rather than silently
// dropping work (AGENTS.md "Failures are configuration problems").
func validateLinearServiceRoutes(path string, cfg Config) error {
	if cfg.Tracker.Kind != "linear" {
		return nil
	}
	if strings.TrimSpace(cfg.Tracker.ProjectSlug) == "" && len(cfg.Services) == 0 {
		return fmt.Errorf("%s: tracker.project_slug is required when tracker.kind is linear", path)
	}
	for i := range cfg.Services {
		if err := validateLinearServiceRoute(path, cfg, i); err != nil {
			return err
		}
	}
	return nil
}

// validateLinearServiceRoute validates a single Linear service route: it must
// resolve a project slug (its own or the top-level one), declare at least one
// route predicate, and must not rely on custom_fields.
func validateLinearServiceRoute(path string, cfg Config, i int) error {
	service := cfg.Services[i]
	topLevelProjectSlug := strings.TrimSpace(cfg.Tracker.ProjectSlug)
	if topLevelProjectSlug == "" && strings.TrimSpace(service.Tracker.ProjectSlug) == "" {
		return fmt.Errorf("%s: services[%d].tracker.project_slug or tracker.project_slug is required for Linear service routing", path, i)
	}
	if !hasExplicitServiceRoute(service.Tracker) {
		return fmt.Errorf("%s: services[%d].tracker must define at least one Linear route predicate (project_slug, team_key, or labels)", path, i)
	}
	// Linear's GraphQL schema does not expose any custom-field data on Issue
	// (introspection: only `customerTicketCount` matches `custom*`); the
	// upstream Elixir reference omits the fragment for the same reason. A
	// `custom_fields:` predicate can never match a polled issue and would
	// silently drop work, so reject it at load time per AGENTS.md "Failures
	// are configuration problems". See #326.
	if len(service.Tracker.CustomFields) > 0 {
		return fmt.Errorf("%s: services[%d].tracker.custom_fields is not supported — Linear's GraphQL schema does not expose Issue custom fields (see #326)", path, i)
	}
	return nil
}

// validateServices enforces non-empty, case-insensitively unique service
// names and a per-service repo.clone_url.
func validateServices(path string, cfg Config) error {
	seenServiceNames := make(map[string]int, len(cfg.Services))
	for i, service := range cfg.Services {
		name := strings.TrimSpace(service.Name)
		if name == "" {
			return fmt.Errorf("%s: services[%d].name is required", path, i)
		}
		nameKey := strings.ToLower(name)
		if first, ok := seenServiceNames[nameKey]; ok {
			return fmt.Errorf("%s: services[%d].name %q duplicates services[%d].name", path, i, service.Name, first)
		}
		seenServiceNames[nameKey] = i
		if strings.TrimSpace(service.Repo.CloneURL) == "" {
			return fmt.Errorf("%s: services[%d].repo.clone_url is required", path, i)
		}
	}
	return nil
}

// validateSupportedValues rejects enum-valued fields set outside their
// supported set, a negative pagination cap, and agent env-passthrough names
// that the exposure policy denies.
func validateSupportedValues(path string, cfg Config) error {
	if _, ok := supportedTrackerKinds[cfg.Tracker.Kind]; !ok {
		return fmt.Errorf("%s: tracker.kind %q is not supported (allowed: gitea, github, linear)", path, cfg.Tracker.Kind)
	}
	if cfg.Tracker.PaginationMaxPages < 0 {
		return fmt.Errorf("%s: tracker.pagination_max_pages must be zero for the adapter default or greater than zero", path)
	}
	if _, ok := supportedAgentDefaults[cfg.Agent.Default]; !ok {
		return fmt.Errorf("%s: agent.default %q is not supported (allowed: mock, codex, codex-app-server, claude)", path, cfg.Agent.Default)
	}
	if _, ok := supportedCodexProfiles[cfg.Codex.Profile]; !ok {
		return fmt.Errorf("%s: codex.profile %q is not supported (allowed: safe, bypass, custom)", path, cfg.Codex.Profile)
	}
	if err := validateAgentEnvPassthrough(path, "codex", cfg.Codex.EnvPassthrough, cfg); err != nil {
		return err
	}
	if err := validateAgentEnvPassthrough(path, "claude", cfg.Claude.EnvPassthrough, cfg); err != nil {
		return err
	}
	return nil
}

// validateSandbox enforces the sandbox backend/enable/network invariants,
// including the Firejail-only IPv4 allowlist constraints.
func validateSandbox(path string, cfg Config) error {
	if _, ok := supportedSandboxBackends[cfg.Sandbox.Backend]; !ok {
		return fmt.Errorf("%s: sandbox.backend %q is not supported (allowed: none, bubblewrap, firejail)", path, cfg.Sandbox.Backend)
	}
	if cfg.Sandbox.Enabled && cfg.Sandbox.Backend == "none" {
		return fmt.Errorf("%s: sandbox.enabled requires sandbox.backend to be bubblewrap or firejail", path)
	}
	if cfg.Sandbox.Enabled && len(cfg.Sandbox.EnvAllowlist) == 0 {
		return fmt.Errorf("%s: sandbox.enabled requires sandbox.env_allowlist to explicitly scope child environment", path)
	}
	if err := validateAgentEnvExposure(path, "sandbox.env_allowlist", cfg.Sandbox.EnvAllowlist, cfg); err != nil {
		return err
	}
	if _, ok := supportedSandboxNetworks[cfg.Sandbox.NetworkMode]; !ok {
		return fmt.Errorf("%s: sandbox.network %q is not supported (allowed: none, allowlist)", path, cfg.Sandbox.NetworkMode)
	}
	if cfg.Sandbox.NetworkMode == "allowlist" {
		return validateSandboxNetworkAllowlist(path, cfg)
	}
	return nil
}

// validateSandboxNetworkAllowlist enforces the Firejail-only constraints for
// sandbox.network=allowlist: a firejail backend, a non-empty IPv4 CIDR list,
// and an explicit host interface for --netfilter to attach to.
func validateSandboxNetworkAllowlist(path string, cfg Config) error {
	if cfg.Sandbox.Backend != "firejail" {
		return fmt.Errorf("%s: sandbox.network=allowlist requires sandbox.backend firejail", path)
	}
	if len(cfg.Sandbox.NetworkAllowlistCIDRs) == 0 {
		return fmt.Errorf("%s: sandbox.network=allowlist requires sandbox.network_allowlist_cidrs", path)
	}
	if strings.TrimSpace(cfg.Sandbox.NetworkInterface) == "" {
		return fmt.Errorf("%s: sandbox.network=allowlist requires sandbox.network_interface so Firejail can attach --netfilter to an explicit host interface", path)
	}
	for _, cidr := range cfg.Sandbox.NetworkAllowlistCIDRs {
		ip, _, err := net.ParseCIDR(strings.TrimSpace(cidr))
		if err != nil {
			return fmt.Errorf("%s: sandbox.network_allowlist_cidrs contains invalid CIDR %q: %w", path, cidr, err)
		}
		if ip.To4() == nil {
			return fmt.Errorf("%s: sandbox.network_allowlist_cidrs contains non-IPv4 CIDR %q; Firejail netfilter allowlists currently support IPv4 only", path, cidr)
		}
	}
	return nil
}

// validateServerPort enforces the local state server port range, including
// the -1 sentinel that disables the server.
func validateServerPort(path string, cfg Config) error {
	if cfg.Server.Port < -1 || cfg.Server.Port == 0 || cfg.Server.Port > 65535 {
		return fmt.Errorf("%s: server.port must be -1 to disable the local state server or between 1 and 65535", path)
	}
	return nil
}

// validateCodexClaude validates the Codex turn sandbox policy, rejects
// Claude-side options that only Codex supports, and checks the linear_graphql
// allowed-mutations opt-in.
func validateCodexClaude(path string, cfg Config) error {
	if err := cfg.Codex.TurnSandboxPolicy.Validate("codex.turn_sandbox_policy"); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if strings.TrimSpace(cfg.Claude.Profile) != "" {
		return fmt.Errorf("%s: claude.profile is not supported (only codex has profiles)", path)
	}
	if !cfg.Claude.LinearGraphQL.IsZero() {
		return fmt.Errorf("%s: claude.linear_graphql is not supported (linear_graphql narrowing is a codex-side tool gate; declare it under codex.linear_graphql)", path)
	}
	return validateAllowedMutations(path, cfg)
}

// validateAllowedMutations checks the codex.linear_graphql.allowed_mutations
// opt-in: each name must be a non-empty, unique, valid GraphQL field name, and
// a non-empty list requires allow_mutations: true.
func validateAllowedMutations(path string, cfg Config) error {
	seenAllowedMutations := make(map[string]int, len(cfg.Codex.LinearGraphQL.AllowedMutations))
	for i, name := range cfg.Codex.LinearGraphQL.AllowedMutations {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("%s: codex.linear_graphql.allowed_mutations[%d] is empty", path, i)
		}
		if !isLinearGraphQLMutationName(name) {
			return fmt.Errorf("%s: codex.linear_graphql.allowed_mutations[%d] %q is not a valid GraphQL field name", path, i, name)
		}
		if first, ok := seenAllowedMutations[name]; ok {
			return fmt.Errorf("%s: codex.linear_graphql.allowed_mutations[%d] %q duplicates allowed_mutations[%d]", path, i, name, first)
		}
		seenAllowedMutations[name] = i
	}
	if len(cfg.Codex.LinearGraphQL.AllowedMutations) > 0 && !cfg.Codex.LinearGraphQL.AllowMutations {
		return fmt.Errorf("%s: codex.linear_graphql.allowed_mutations requires codex.linear_graphql.allow_mutations: true", path)
	}
	return nil
}

// validateAgentLimits enforces the positive/non-negative agent concurrency
// and retry caps, including the per-state concurrency overrides.
func validateAgentLimits(path string, cfg Config) error {
	if cfg.Agent.MaxRetryBackoffMs <= 0 {
		return fmt.Errorf("%s: agent.max_retry_backoff_ms must be positive", path)
	}
	if cfg.Agent.MaxTurns <= 0 {
		return fmt.Errorf("%s: agent.max_turns must be positive", path)
	}
	if cfg.Agent.MaxConcurrentAgents <= 0 {
		return fmt.Errorf("%s: agent.max_concurrent_agents must be a positive integer (SPEC §6.4 default 10; explicit 0 is not allowed — Elixir validate_number greater_than: 0)", path)
	}
	if cfg.Agent.MaxRetryAttempts != nil && *cfg.Agent.MaxRetryAttempts < 0 {
		return fmt.Errorf("%s: agent.max_retry_attempts must be non-negative", path)
	}
	return validateStateConcurrencyCaps(path, cfg)
}

// validateStateConcurrencyCaps enforces that each
// agent.max_concurrent_agents_by_state override has a non-empty state key, a
// positive limit, and no duplicate after state-key normalization.
func validateStateConcurrencyCaps(path string, cfg Config) error {
	seenStateCaps := make(map[string]string, len(cfg.Agent.MaxConcurrentAgentsByState))
	for state, limit := range cfg.Agent.MaxConcurrentAgentsByState {
		if strings.TrimSpace(state) == "" {
			return fmt.Errorf("%s: agent.max_concurrent_agents_by_state contains an empty state key", path)
		}
		if limit <= 0 {
			return fmt.Errorf("%s: agent.max_concurrent_agents_by_state[%q] must be positive", path, state)
		}
		key := NormalizeStateConcurrencyKey(state)
		if first, ok := seenStateCaps[key]; ok {
			return fmt.Errorf("%s: agent.max_concurrent_agents_by_state[%q] duplicates %q after normalization to %q", path, state, first, key)
		}
		seenStateCaps[key] = state
	}
	return nil
}

// validateTimeouts enforces positive hook timeouts and the Codex app-server
// turn/read/stall timeout settings.
func validateTimeouts(path string, cfg Config) error {
	if cfg.hookFields.TimeoutMs && cfg.Hooks.TimeoutMs <= 0 {
		return fmt.Errorf("%s: hooks.timeout_ms must be a positive integer", path)
	}
	if cfg.Workspace.hookFields.TimeoutMs && cfg.Workspace.Hooks.TimeoutMs <= 0 {
		return fmt.Errorf("%s: workspace.hooks.timeout_ms must be a positive integer", path)
	}
	var invalid []string
	if cfg.Codex.TurnTimeoutMs <= 0 {
		invalid = append(invalid, "codex.turn_timeout_ms")
	}
	if cfg.Codex.ReadTimeoutMs <= 0 {
		invalid = append(invalid, "codex.read_timeout_ms")
	}
	if cfg.Codex.StallTimeoutMs < 0 {
		invalid = append(invalid, "codex.stall_timeout_ms")
	}
	if len(invalid) > 0 {
		return fmt.Errorf("%s: invalid codex app-server timeout settings: %s", path, strings.Join(invalid, ", "))
	}
	return nil
}

func validateAgentEnvPassthrough(path, section string, names []string, cfg Config) error {
	return validateAgentEnvExposure(path, section+".env_passthrough", names, cfg)
}

func validateAgentEnvExposure(path, field string, names []string, cfg Config) error {
	for i, name := range names {
		if reason := AgentEnvPassthroughDenyReasonForConfig(name, cfg); reason != "" {
			return fmt.Errorf("%s: %s[%d] %q is not allowed: %s", path, field, i, name, reason)
		}
	}
	return nil
}
