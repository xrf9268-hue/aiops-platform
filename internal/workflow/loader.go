package workflow

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Workflow struct {
	Path           string
	Config         Config
	PromptTemplate string
	Source         Source
}

func Load(path string) (*Workflow, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	front, body := splitFrontMatter(string(b))
	cfg := DefaultConfig()
	hasFrontMatter := strings.TrimSpace(front) != ""
	if hasFrontMatter {
		frontBytes := []byte(front)
		if err := rejectRemovedFields(frontBytes); err != nil {
			return nil, err
		}
		logUnknownTopLevelKeys(frontBytes)
		hookFields := hookFieldPresence(frontBytes, "hooks")
		legacyHookFields := hookFieldPresence(frontBytes, "workspace", "hooks")
		workspaceRootSet := hasNestedKey(frontBytes, "workspace", "root")
		if err := yaml.Unmarshal(frontBytes, &cfg); err != nil {
			return nil, fmt.Errorf("parse workflow front matter: %w", err)
		}
		cfg.hookFields = hookFields
		cfg.Workspace.hookFields = legacyHookFields
		cfg.Workspace.rootSet = workspaceRootSet
		if hookFields.TimeoutMs {
			cfg.hooksTimeoutDefaulted = false
		}
		migratePollingInterval(frontBytes, &cfg)
	}
	var rawStateCaps map[string]int
	if hasFrontMatter && len(cfg.Agent.MaxConcurrentAgentsByState) > 0 {
		rawStateCaps = make(map[string]int, len(cfg.Agent.MaxConcurrentAgentsByState))
		for state, limit := range cfg.Agent.MaxConcurrentAgentsByState {
			rawStateCaps[state] = limit
		}
	}
	expandConfigForWorkflowPath(path, &cfg)
	if rawStateCaps != nil {
		cfg.Agent.MaxConcurrentAgentsByState = rawStateCaps
	}
	// Only validate when the file actually carries a front-matter block.
	// Prompt-only WORKFLOW.md files are a supported pattern for repos that
	// rely on the worker's built-in defaults (same semantic as
	// LoadOptional's no-file fallback). Forcing schema validation on those
	// would regress every repo that has not yet adopted the explicit
	// Symphony front matter.
	if hasFrontMatter {
		if err := validateConfig(path, cfg); err != nil {
			return nil, err
		}
	}
	cfg.Agent.MaxConcurrentAgentsByState = normalizeStateConcurrencyLimits(cfg.Agent.MaxConcurrentAgentsByState)
	source := SourceFile
	if !hasFrontMatter {
		source = SourcePromptOnly
	}
	return &Workflow{Path: path, Config: cfg, PromptTemplate: strings.TrimSpace(body), Source: source}, nil
}

func migratePollingInterval(frontBytes []byte, cfg *Config) {
	pollingPresent := hasNestedKey(frontBytes, "polling", "interval_ms")
	legacyPresent := hasNestedKey(frontBytes, "tracker", "poll_interval_ms")
	switch {
	case pollingPresent:
		if legacyPresent {
			log.Printf("workflow: tracker.poll_interval_ms is deprecated and ignored because polling.interval_ms is set")
		}
		cfg.Tracker.PollIntervalMs = cfg.Polling.IntervalMs
	case legacyPresent:
		log.Printf("workflow: tracker.poll_interval_ms is deprecated; use polling.interval_ms")
		cfg.Polling.IntervalMs = cfg.Tracker.PollIntervalMs
	default:
		cfg.Tracker.PollIntervalMs = cfg.Polling.IntervalMs
	}
}

// supportedTrackerKinds enumerates the tracker integrations the platform
// actually wires up today (see cmd/linear-poller and cmd/gitea-poller).
// Anything outside this set would parse as a typed config but could not be
// claimed by the worker.
var supportedTrackerKinds = map[string]struct{}{
	"gitea":  {},
	"github": {},
	"linear": {},
}

// supportedAgentDefaults mirrors the runner registry in
// internal/runner.New. Keeping the two lists in sync at the schema layer
// turns "unknown runner: X" — which today only surfaces after a task is
// claimed and the workspace prepared — into a load-time configuration
// error with the workflow file path attached.
var supportedAgentDefaults = map[string]struct{}{
	"mock":             {},
	"codex":            {},
	"codex-app-server": {},
	"claude":           {},
}

// supportedCodexProfiles enumerates the codex runner profile names the
// runner package knows how to dispatch. "safe" injects --full-auto +
// --skip-git-repo-check; "bypass" swaps in
// --dangerously-bypass-approvals-and-sandbox for already-isolated hosts;
// "custom" falls back to the operator-supplied codex.command via sh -lc.
var supportedCodexProfiles = map[string]struct{}{
	"safe":   {},
	"bypass": {},
	"custom": {},
}

var supportedSandboxBackends = map[string]struct{}{
	"none":       {},
	"bubblewrap": {},
	"firejail":   {},
}

var supportedSandboxNetworks = map[string]struct{}{
	"none":      {},
	"allowlist": {},
}

// validateConfig enforces the required-field and enum constraints that
// the typed YAML decoder cannot express on its own. It runs after
// expandConfig so env-var indirections (e.g. `clone_url: $REPO_URL`)
// are evaluated before non-empty checks. Errors include the workflow
// file path and the offending field/value so operators can fix the
// source rather than chasing runtime symptoms (issue #9).
func hasExplicitServiceRoute(route ServiceTrackerRouteConfig) bool {
	return strings.TrimSpace(route.ProjectSlug) != "" ||
		strings.TrimSpace(route.TeamKey) != "" ||
		len(route.Labels) > 0 ||
		len(route.CustomFields) > 0
}

func validateConfig(path string, cfg Config) error {
	if strings.TrimSpace(cfg.Repo.CloneURL) == "" {
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
	}
	if cfg.Tracker.Kind == "linear" {
		topLevelProjectSlug := strings.TrimSpace(cfg.Tracker.ProjectSlug)
		if topLevelProjectSlug == "" && len(cfg.Services) == 0 {
			return fmt.Errorf("%s: tracker.project_slug is required when tracker.kind is linear", path)
		}
		for i, service := range cfg.Services {
			serviceProjectSlug := strings.TrimSpace(service.Tracker.ProjectSlug)
			if topLevelProjectSlug == "" && serviceProjectSlug == "" {
				return fmt.Errorf("%s: services[%d].tracker.project_slug or tracker.project_slug is required for Linear service routing", path, i)
			}
			if !hasExplicitServiceRoute(service.Tracker) {
				return fmt.Errorf("%s: services[%d].tracker must define at least one Linear route predicate (project_slug, team_key, labels, or custom_fields)", path, i)
			}
		}
	}
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
	if _, ok := supportedTrackerKinds[cfg.Tracker.Kind]; !ok {
		return fmt.Errorf("%s: tracker.kind %q is not supported (allowed: gitea, github, linear)", path, cfg.Tracker.Kind)
	}
	if _, ok := supportedAgentDefaults[cfg.Agent.Default]; !ok {
		return fmt.Errorf("%s: agent.default %q is not supported (allowed: mock, codex, codex-app-server, claude)", path, cfg.Agent.Default)
	}
	if _, ok := supportedCodexProfiles[cfg.Codex.Profile]; !ok {
		return fmt.Errorf("%s: codex.profile %q is not supported (allowed: safe, bypass, custom)", path, cfg.Codex.Profile)
	}
	if _, ok := supportedSandboxBackends[cfg.Sandbox.Backend]; !ok {
		return fmt.Errorf("%s: sandbox.backend %q is not supported (allowed: none, bubblewrap, firejail)", path, cfg.Sandbox.Backend)
	}
	if cfg.Sandbox.Enabled && cfg.Sandbox.Backend == "none" {
		return fmt.Errorf("%s: sandbox.enabled requires sandbox.backend to be bubblewrap or firejail", path)
	}
	if cfg.Sandbox.Enabled && len(cfg.Sandbox.EnvAllowlist) == 0 {
		return fmt.Errorf("%s: sandbox.enabled requires sandbox.env_allowlist to explicitly scope child environment", path)
	}
	if _, ok := supportedSandboxNetworks[cfg.Sandbox.NetworkMode]; !ok {
		return fmt.Errorf("%s: sandbox.network %q is not supported (allowed: none, allowlist)", path, cfg.Sandbox.NetworkMode)
	}
	if cfg.Sandbox.NetworkMode == "allowlist" {
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
	}
	if cfg.Server.Port < -1 || cfg.Server.Port == 0 || cfg.Server.Port > 65535 {
		return fmt.Errorf("%s: server.port must be -1 to disable the local state server or between 1 and 65535", path)
	}
	if strings.TrimSpace(cfg.Claude.Profile) != "" {
		return fmt.Errorf("%s: claude.profile is not supported (only codex has profiles)", path)
	}
	if cfg.Agent.MaxRetryBackoffMs <= 0 {
		return fmt.Errorf("%s: agent.max_retry_backoff_ms must be positive", path)
	}
	if cfg.Agent.MaxTurns <= 0 {
		return fmt.Errorf("%s: agent.max_turns must be positive", path)
	}
	if cfg.Agent.MaxRetryAttempts != nil && *cfg.Agent.MaxRetryAttempts < 0 {
		return fmt.Errorf("%s: agent.max_retry_attempts must be non-negative", path)
	}
	seenStateCaps := make(map[string]string, len(cfg.Agent.MaxConcurrentAgentsByState))
	for state, limit := range cfg.Agent.MaxConcurrentAgentsByState {
		if strings.TrimSpace(state) == "" {
			return fmt.Errorf("%s: agent.max_concurrent_agents_by_state contains an empty state key", path)
		}
		if limit <= 0 {
			return fmt.Errorf("%s: agent.max_concurrent_agents_by_state[%q] must be positive", path, state)
		}
		key := normalizeTrackerStateKey(state)
		if first, ok := seenStateCaps[key]; ok {
			return fmt.Errorf("%s: agent.max_concurrent_agents_by_state[%q] duplicates %q after normalization to %q", path, state, first, key)
		}
		seenStateCaps[key] = state
	}
	if cfg.Hooks.TimeoutMs < 0 {
		return fmt.Errorf("%s: hooks.timeout_ms must be non-negative", path)
	}
	if cfg.Workspace.Hooks.TimeoutMs < 0 {
		return fmt.Errorf("%s: workspace.hooks.timeout_ms must be non-negative", path)
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

// rejectRemovedFields surfaces a clear error for keys that were once part
// of the schema but have been removed. The typed Unmarshal above silently
// drops unknown fields, which would let workflow authors keep believing
// the key still controls behavior. Targeted detection keeps existing
// benign extras working while flagging known footguns.
func hasNestedKey(front []byte, path ...string) bool {
	var raw map[string]any
	if err := yaml.Unmarshal(front, &raw); err != nil {
		return false
	}
	var current any = raw
	for _, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return false
		}
		current, ok = m[key]
		if !ok {
			return false
		}
	}
	return true
}

func hookFieldPresence(front []byte, path ...string) HookFieldPresence {
	return HookFieldPresence{
		AfterCreate:  hasNestedKey(front, append(path, "after_create")...),
		BeforeRun:    hasNestedKey(front, append(path, "before_run")...),
		AfterRun:     hasNestedKey(front, append(path, "after_run")...),
		BeforeRemove: hasNestedKey(front, append(path, "before_remove")...),
		TimeoutMs:    hasNestedKey(front, append(path, "timeout_ms")...),
	}
}

func rejectRemovedFields(front []byte) error {
	var raw map[string]any
	if err := yaml.Unmarshal(front, &raw); err != nil {
		return nil
	}
	agent, ok := raw["agent"].(map[string]any)
	if !ok {
		return nil
	}
	if _, present := agent["fallback"]; present {
		return fmt.Errorf("agent.fallback is no longer supported (issue #40); the worker never read this field. Remove it and set agent.default to a more reliable runner if you need a different default")
	}
	return nil
}

var knownTopLevelWorkflowKeys = map[string]struct{}{
	"agent":     {},
	"claude":    {},
	"codex":     {},
	"hooks":     {},
	"policy":    {},
	"polling":   {},
	"pr":        {},
	"repo":      {},
	"safety":    {},
	"sandbox":   {},
	"server":    {},
	"services":  {},
	"tracker":   {},
	"verify":    {},
	"workspace": {},
}

func logUnknownTopLevelKeys(front []byte) {
	var raw map[string]any
	if err := yaml.Unmarshal(front, &raw); err != nil {
		return
	}
	unknown := make([]string, 0)
	for key := range raw {
		if _, ok := knownTopLevelWorkflowKeys[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	for _, key := range unknown {
		log.Printf("workflow: unknown top-level key %s ignored", key)
	}
}

// LoadOptional loads a workflow from an explicit path, returning schema
// defaults when the file does not exist. New worker code should use
// Resolve(workdir), which handles repo-relative discovery and returns
// resolution metadata.
//
// Deprecated: use Resolve(workdir) for repo-relative discovery. Retained
// for callers that pass an explicit path (e.g. cmd/linear-poller has a
// related but separate loader contract).
func LoadOptional(path string) (*Workflow, error) {
	wf, err := Load(path)
	if err == nil {
		return wf, nil
	}
	if os.IsNotExist(err) {
		cfg := DefaultConfig()
		expandConfig(&cfg)
		return &Workflow{Path: path, Config: cfg, PromptTemplate: DefaultPrompt()}, nil
	}
	return nil, err
}

func splitFrontMatter(s string) (string, string) {
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return "", s
	}
	trimmed := strings.TrimPrefix(strings.TrimPrefix(s, "---\r\n"), "---\n")
	idx := strings.Index(trimmed, "\n---")
	if idx < 0 {
		return "", s
	}
	front := trimmed[:idx]
	body := trimmed[idx+len("\n---"):]
	body = strings.TrimPrefix(body, "\r\n")
	body = strings.TrimPrefix(body, "\n")
	return front, body
}

func expandConfig(cfg *Config) {
	expandConfigForWorkflowPath("", cfg)
}

func expandConfigForWorkflowPath(workflowPath string, cfg *Config) {
	cfg.Tracker.APIKey = os.ExpandEnv(cfg.Tracker.APIKey)
	cfg.Tracker.BaseURL = os.ExpandEnv(cfg.Tracker.BaseURL)
	expandRepoConfig(&cfg.Repo)
	for i := range cfg.Services {
		expandRepoConfig(&cfg.Services[i].Repo)
	}
	cfg.Workspace.Root = normalizeWorkflowPath(workflowPath, cfg.Workspace.Root)
	cfg.Codex.Command = os.ExpandEnv(cfg.Codex.Command)
	cfg.Claude.Command = os.ExpandEnv(cfg.Claude.Command)
	for i := range cfg.Sandbox.CredentialFiles {
		cfg.Sandbox.CredentialFiles[i] = expandPath(os.ExpandEnv(cfg.Sandbox.CredentialFiles[i]))
	}
	if cfg.Agent.Default == "" {
		cfg.Agent.Default = "mock"
	}
	if cfg.Agent.MaxConcurrentAgents <= 0 {
		cfg.Agent.MaxConcurrentAgents = 1
	}
	if cfg.Agent.Timeout <= 0 {
		cfg.Agent.Timeout = 30 * time.Minute
	}
	// MaxTimeoutRetries is *int so callers can distinguish "absent"
	// (nil → MaxTimeoutRetriesValue() returns the schema default of 1)
	// from "explicitly 0" (zero retries). We deliberately do not coerce
	// here: forcing 0 → 1 stripped users of the ability to disable the
	// runner-timeout retry budget entirely.
	if cfg.Polling.IntervalMs <= 0 {
		cfg.Polling.IntervalMs = cfg.Tracker.PollIntervalMs
	}
	if cfg.Polling.IntervalMs <= 0 {
		cfg.Polling.IntervalMs = 30000
	}
	cfg.Tracker.PollIntervalMs = cfg.Polling.IntervalMs
	// Tracker.Statuses defaults are applied per-field so a YAML override of
	// a single name (e.g. `statuses.in_progress: "Doing"`) does not require
	// the operator to also restate the unchanged ones. The defaults match
	// the Linear template the personal profile ships with.
	if cfg.Tracker.Statuses.InProgress == "" {
		cfg.Tracker.Statuses.InProgress = "In Progress"
	}
	if cfg.Tracker.Statuses.HumanReview == "" {
		cfg.Tracker.Statuses.HumanReview = "Human Review"
	}
	if cfg.Tracker.Statuses.Rework == "" {
		cfg.Tracker.Statuses.Rework = "Rework"
	}
	if cfg.Codex.Profile == "" {
		cfg.Codex.Profile = "safe"
	}
	if cfg.Sandbox.Backend == "" {
		cfg.Sandbox.Backend = "none"
	}
	if cfg.Sandbox.NetworkMode == "" {
		cfg.Sandbox.NetworkMode = "none"
	}
	if cfg.Codex.ThreadSandbox == "" {
		cfg.Codex.ThreadSandbox = "workspace-write"
	}
	if cfg.Codex.ApprovalPolicy == nil {
		cfg.Codex.ApprovalPolicy = map[string]any{"reject": map[string]any{"sandbox_approval": true, "rules": true, "mcp_elicitations": true}}
	}
}

func expandRepoConfig(repo *RepoConfig) {
	repo.CloneURL = os.ExpandEnv(repo.CloneURL)
	if repo.DefaultBranch == "" {
		repo.DefaultBranch = "main"
	}
}

func normalizeWorkflowPath(workflowPath, p string) string {
	expanded := expandPath(os.ExpandEnv(p))
	if expanded == "" || filepath.IsAbs(expanded) || workflowPath == "" {
		return expanded
	}
	log.Printf("workflow: relative workspace.root %s resolved relative to workflow file %s", expanded, workflowPath)
	return filepath.Join(filepath.Dir(workflowPath), expanded)
}

func expandPath(p string) string {
	if p == "" {
		return p
	}
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~/"))
		}
	}
	return p
}

func normalizeStateConcurrencyLimits(limits map[string]int) map[string]int {
	if len(limits) == 0 {
		return nil
	}
	normalized := make(map[string]int, len(limits))
	for state, limit := range limits {
		key := normalizeTrackerStateKey(state)
		if key == "" {
			normalized[state] = limit
			continue
		}
		normalized[key] = limit
	}
	return normalized
}

func normalizeTrackerStateKey(state string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(state)), " ", "_")
}
