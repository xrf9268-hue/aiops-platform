package workflow

import (
	"errors"
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

type Category string

const (
	CategoryMissingWorkflowFile       Category = "missing_workflow_file"
	CategoryWorkflowParseError        Category = "workflow_parse_error"
	CategoryWorkflowFrontMatterNotMap Category = "workflow_front_matter_not_a_map"
	CategoryTemplateParseError        Category = "template_parse_error"
	CategoryTemplateRenderError       Category = "template_render_error"
)

var (
	ErrMissingWorkflowFile       = &Error{Category: CategoryMissingWorkflowFile}
	ErrWorkflowParse             = &Error{Category: CategoryWorkflowParseError}
	ErrWorkflowFrontMatterNotMap = &Error{Category: CategoryWorkflowFrontMatterNotMap}
	ErrTemplateParse             = &TemplateParseError{}
	ErrTemplateRender            = &TemplateRenderError{}
)

type Error struct {
	Category Category
	Path     string
	Message  string
	Err      error
}

func NewError(category Category, message string, err error) *Error {
	return &Error{Category: category, Message: message, Err: err}
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	msg := e.Message
	if msg == "" {
		msg = string(e.Category)
	}
	if e.Path != "" {
		msg = e.Path + ": " + msg
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *Error) Is(target error) bool {
	return categoryMatches(e.category(), target)
}

func (e *Error) category() Category {
	if e == nil {
		return ""
	}
	return e.Category
}

type TemplateParseError struct {
	Err error
}

func (e *TemplateParseError) Error() string {
	if e == nil || e.Err == nil {
		return string(CategoryTemplateParseError)
	}
	return fmt.Sprintf("%s: %v", CategoryTemplateParseError, e.Err)
}

func (e *TemplateParseError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *TemplateParseError) Is(target error) bool {
	return categoryMatches(e.category(), target)
}

func (e *TemplateParseError) category() Category {
	return CategoryTemplateParseError
}

type categorized interface {
	category() Category
}

func ErrorCategory(err error) (Category, bool) {
	var categorizedErr categorized
	if errors.As(err, &categorizedErr) {
		category := categorizedErr.category()
		return category, category != ""
	}
	return "", false
}

func categoryMatches(category Category, target error) bool {
	if category == "" || target == nil {
		return false
	}
	var targetCategory categorized
	return errors.As(target, &targetCategory) && targetCategory.category() == category
}

func Load(path string) (*Workflow, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &Error{Category: CategoryMissingWorkflowFile, Path: path, Message: "read workflow file", Err: err}
		}
		return nil, fmt.Errorf("%s: read workflow file: %w", path, err)
	}
	front, body := splitFrontMatter(string(b))
	cfg := DefaultConfig()
	hasFrontMatter := strings.TrimSpace(front) != ""
	if hasFrontMatter {
		frontBytes := []byte(front)
		if err := validateFrontMatterRoot(path, frontBytes); err != nil {
			return nil, err
		}
		if err := rejectRemovedFields(frontBytes); err != nil {
			return nil, err
		}
		logUnknownTopLevelKeys(frontBytes)
		hookFields := hookFieldPresence(frontBytes, "hooks")
		legacyHookFields := hookFieldPresence(frontBytes, "workspace", "hooks")
		workspaceRootSet := hasNestedKey(frontBytes, "workspace", "root")
		if err := yaml.Unmarshal(frontBytes, &cfg); err != nil {
			return nil, &Error{Category: CategoryWorkflowParseError, Path: path, Message: "parse workflow front matter", Err: err}
		}
		cfg.hookFields = hookFields
		cfg.Workspace.hookFields = legacyHookFields
		cfg.Workspace.rootSet = workspaceRootSet
		if hookFields.TimeoutMs {
			cfg.hooksTimeoutDefaulted = false
		}
		migratePollingInterval(frontBytes, &cfg)
		migrateTrackerEndpoint(frontBytes, &cfg)
	}
	var rawStateCaps map[string]int
	if hasFrontMatter && len(cfg.Agent.MaxConcurrentAgentsByState) > 0 {
		rawStateCaps = make(map[string]int, len(cfg.Agent.MaxConcurrentAgentsByState))
		for state, limit := range cfg.Agent.MaxConcurrentAgentsByState {
			rawStateCaps[state] = limit
		}
	}
	if err := expandConfigForWorkflowPath(path, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
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
	cfg.Agent.MaxConcurrentAgentsByState = NormalizeStateConcurrencyLimits(cfg.Agent.MaxConcurrentAgentsByState)
	source := SourceFile
	if !hasFrontMatter {
		source = SourcePromptOnly
	}
	return &Workflow{Path: path, Config: cfg, PromptTemplate: strings.TrimSpace(body), Source: source}, nil
}

func validateFrontMatterRoot(path string, frontBytes []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(frontBytes, &doc); err != nil {
		return &Error{Category: CategoryWorkflowParseError, Path: path, Message: "parse workflow front matter", Err: err}
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return &Error{Category: CategoryWorkflowFrontMatterNotMap, Path: path, Message: "workflow front matter must decode to a map"}
	}
	return nil
}

// migrateTrackerEndpoint reconciles the SPEC §5.3.1 `tracker.endpoint`
// field with the pre-#242 `tracker.base_url` alias: prefer endpoint when
// set, fall back to base_url with a deprecation log, and finally clear
// BaseURL on the resolved config so downstream code reads only Endpoint.
func migrateTrackerEndpoint(frontBytes []byte, cfg *Config) {
	endpointPresent := hasNestedKey(frontBytes, "tracker", "endpoint")
	legacyPresent := hasNestedKey(frontBytes, "tracker", "base_url")
	switch {
	case endpointPresent:
		if legacyPresent {
			log.Printf("workflow: tracker.base_url is deprecated and ignored because tracker.endpoint is set")
		}
	case legacyPresent:
		log.Printf("workflow: tracker.base_url is deprecated; use tracker.endpoint (SPEC §5.3.1)")
		cfg.Tracker.Endpoint = cfg.Tracker.BaseURL
	}
	cfg.Tracker.BaseURL = ""
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
	if !cfg.Claude.LinearGraphQL.IsZero() {
		return fmt.Errorf("%s: claude.linear_graphql is not supported (linear_graphql narrowing is a codex-side tool gate; declare it under codex.linear_graphql)", path)
	}
	for i, name := range cfg.Codex.LinearGraphQL.AllowedMutations {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("%s: codex.linear_graphql.allowed_mutations[%d] is empty", path, i)
		}
		if !isLinearGraphQLMutationName(name) {
			return fmt.Errorf("%s: codex.linear_graphql.allowed_mutations[%d] %q is not a valid GraphQL field name", path, i, name)
		}
	}
	if len(cfg.Codex.LinearGraphQL.AllowedMutations) > 0 && !cfg.Codex.LinearGraphQL.AllowMutations {
		return fmt.Errorf("%s: codex.linear_graphql.allowed_mutations requires codex.linear_graphql.allow_mutations: true", path)
	}
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
		AfterCreate:    hasNestedKey(front, append(path, "after_create")...),
		BeforeRun:      hasNestedKey(front, append(path, "before_run")...),
		AfterRun:       hasNestedKey(front, append(path, "after_run")...),
		BeforeRemove:   hasNestedKey(front, append(path, "before_remove")...),
		TimeoutMs:      hasNestedKey(front, append(path, "timeout_ms")...),
		EnvPassthrough: hasNestedKey(front, append(path, "env_passthrough")...),
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
	if errors.Is(err, ErrMissingWorkflowFile) || os.IsNotExist(err) {
		cfg := DefaultConfig()
		if err := expandConfig(&cfg); err != nil {
			return nil, err
		}
		return &Workflow{Path: path, Config: cfg, PromptTemplate: DefaultPrompt()}, nil
	}
	return nil, err
}

// splitFrontMatter peels the SPEC §5.2 YAML front-matter block off
// the start of a workflow file. The opening fence is `---` followed
// by a newline; the closing fence is a line that is **exactly**
// `---` (with an optional CR before the LF) and nothing else. The
// earlier substring-based scan would mis-match `---` lines that
// appear inside YAML block scalars or quoted strings — see #231,
// where `description: |` blocks legitimately contained a `---` line
// and silently truncated the parsed Config.
//
// Returns (front, body). When no opening fence is present or no
// closing fence can be found, returns ("", s) so the caller treats
// the whole file as the prompt body.
func splitFrontMatter(s string) (string, string) {
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return "", s
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(s, "---\r\n"), "---\n")
	lines := strings.SplitAfter(rest, "\n")
	var front strings.Builder
	for i, line := range lines {
		// Strip only the line-ending (CR/LF) for the fence comparison.
		// Trailing spaces, indentation, or any other content disqualify
		// the line from being the closing fence.
		if strings.TrimRight(line, "\r\n") == "---" {
			return front.String(), strings.Join(lines[i+1:], "")
		}
		front.WriteString(line)
	}
	return "", s
}

func expandConfig(cfg *Config) error {
	return expandConfigForWorkflowPath("", cfg)
}

func expandConfigForWorkflowPath(workflowPath string, cfg *Config) error {
	var err error
	if cfg.Tracker.APIKey, err = resolveExplicitEnv("tracker.api_key", cfg.Tracker.APIKey); err != nil {
		return err
	}
	if cfg.Tracker.Endpoint, err = resolveExplicitEnv("tracker.endpoint", cfg.Tracker.Endpoint); err != nil {
		return err
	}
	if err := expandRepoConfig("repo.clone_url", &cfg.Repo); err != nil {
		return err
	}
	for i := range cfg.Services {
		if err := expandRepoConfig(fmt.Sprintf("services[%d].repo.clone_url", i), &cfg.Services[i].Repo); err != nil {
			return err
		}
	}
	if cfg.Workspace.Root, err = normalizeWorkflowPath(workflowPath, cfg.Workspace.Root); err != nil {
		return err
	}
	if cfg.Codex.Command, err = resolveExplicitEnv("codex.command", cfg.Codex.Command); err != nil {
		return err
	}
	if cfg.Claude.Command, err = resolveExplicitEnv("claude.command", cfg.Claude.Command); err != nil {
		return err
	}
	for i := range cfg.Sandbox.CredentialFiles {
		field := fmt.Sprintf("sandbox.credential_files[%d]", i)
		resolved, err := resolveExplicitEnv(field, cfg.Sandbox.CredentialFiles[i])
		if err != nil {
			return err
		}
		cfg.Sandbox.CredentialFiles[i] = expandPath(resolved)
	}
	if cfg.Agent.Default == "" {
		cfg.Agent.Default = "mock"
	}
	// agent.max_concurrent_agents: SPEC §6.4 default of 10 is supplied by
	// DefaultConfig() and survives YAML overlay when the field is absent.
	// An explicit `max_concurrent_agents: 0` (or any non-positive value)
	// is rejected by validateConfig rather than silently coerced — Elixir
	// `validate_number(:max_concurrent_agents, greater_than: 0)`
	// (schema.ex:131,145) makes 0 a validation error, not a request for
	// the default.
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
	return nil
}

func expandRepoConfig(field string, repo *RepoConfig) error {
	var err error
	if repo.CloneURL, err = resolveExplicitEnv(field, repo.CloneURL); err != nil {
		return err
	}
	if repo.DefaultBranch == "" {
		repo.DefaultBranch = "main"
	}
	return nil
}

func normalizeWorkflowPath(workflowPath, p string) (string, error) {
	resolved, err := resolveExplicitEnv("workspace.root", p)
	if err != nil {
		return "", err
	}
	expanded := expandPath(resolved)
	if expanded == "" || filepath.IsAbs(expanded) || workflowPath == "" {
		return expanded, nil
	}
	log.Printf("workflow: relative workspace.root %s resolved relative to workflow file %s", expanded, workflowPath)
	return filepath.Join(filepath.Dir(workflowPath), expanded), nil
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

type MissingEnvValueError struct {
	Field  string
	EnvVar string
}

func (e *MissingEnvValueError) Error() string {
	category := "workflow_config_missing_value"
	if e.Field == "tracker.api_key" {
		category = "missing_tracker_api_key"
	}
	return fmt.Sprintf("%s: %s references $%s but the environment variable is unset or empty", category, e.Field, e.EnvVar)
}

func resolveExplicitEnv(field, value string) (string, error) {
	envName, ok := explicitEnvReferenceName(value)
	if !ok {
		return value, nil
	}
	resolved, ok := os.LookupEnv(envName)
	if !ok || resolved == "" {
		return "", &MissingEnvValueError{Field: field, EnvVar: envName}
	}
	return resolved, nil
}

func explicitEnvReferenceName(value string) (string, bool) {
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		name := strings.TrimSuffix(strings.TrimPrefix(value, "${"), "}")
		return name, isExplicitEnvName(name)
	}
	if strings.HasPrefix(value, "$") {
		name := strings.TrimPrefix(value, "$")
		return name, isExplicitEnvName(name)
	}
	return "", false
}

func isExplicitEnvName(name string) bool {
	if name == "" {
		return false
	}
	isLetter := func(r rune) bool {
		return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
	}
	for i, r := range name {
		switch {
		case i == 0 && (r == '_' || isLetter(r)):
			continue
		case i > 0 && (r == '_' || isLetter(r) || (r >= '0' && r <= '9')):
			continue
		default:
			return false
		}
	}
	return true
}

// NormalizeStateConcurrencyLimits canonicalizes the per-state concurrency
// cap map (`agent.max_concurrent_agents_by_state`). It is the single
// source of truth shared between the loader's initial-load path
// (internal/workflow/loader.go) and the orchestrator's snapshot-build
// path (internal/orchestrator/workflow_runtime.go); both call this
// helper so a `WORKFLOW.md` reload cannot produce a differently-shaped
// in-memory map than the initial load did.
//
// Semantics (closes #294):
//   - Whitespace/case-folded keys: state names are normalized via
//     [NormalizeStateConcurrencyKey] so `"In Progress"`, `"in progress"`,
//     and `"  in progress "` all map to the same bucket.
//   - Empty / whitespace-only keys are DROPPED. The orchestrator looks
//     up caps by `NormalizeStateConcurrencyKey(stateName)`, which can
//     never produce the empty string from a real tracker state, so any
//     preserved empty-key entry would be permanently dead.
//   - Non-positive limits (`<= 0`) are DROPPED. SPEC §5.3.5 caps are a
//     positive integer; `0` would silently mean "never dispatch this
//     state" but operators expressing that intent would set the issue
//     to a different active-states list, not a `0` cap.
//
// Both rules drop entries the orchestrator could not use anyway,
// trading a little operator-feedback granularity for shape parity
// across the load/reload boundary.
func NormalizeStateConcurrencyLimits(limits map[string]int) map[string]int {
	if len(limits) == 0 {
		return nil
	}
	out := make(map[string]int, len(limits))
	for state, limit := range limits {
		key := NormalizeStateConcurrencyKey(state)
		if key == "" || limit <= 0 {
			continue
		}
		out[key] = limit
	}
	return out
}

// isLinearGraphQLMutationName reports whether s is a valid GraphQL Name
// per the spec (https://spec.graphql.org/October2021/#sec-Names): an ASCII
// letter or underscore followed by ASCII letters, digits, or underscores.
// Used by the codex.linear_graphql.allowed_mutations validator so a typo
// like " issueUpdate" (with leading space) fails at load time rather than
// at the first attempted mutation.
func isLinearGraphQLMutationName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		letter := (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z')
		digit := ch >= '0' && ch <= '9'
		if i == 0 {
			if !letter && ch != '_' {
				return false
			}
			continue
		}
		if !letter && !digit && ch != '_' {
			return false
		}
	}
	return true
}

// NormalizeStateConcurrencyKey canonicalizes a single tracker state name
// for use as a key in the per-state concurrency cap map. The shape
// (`strings.ToLower` + trim + space→underscore) matches how the
// orchestrator looks up caps when a worker session is dispatching, so
// the keys produced by [NormalizeStateConcurrencyLimits] line up with
// the runtime lookup path.
func NormalizeStateConcurrencyKey(state string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(state)), " ", "_")
}
