package workflow

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Repo       RepoConfig      `yaml:"repo" json:"repo"`
	Server     ServerConfig    `yaml:"server" json:"server"`
	Tracker    TrackerConfig   `yaml:"tracker" json:"tracker"`
	Polling    PollingConfig   `yaml:"polling" json:"polling"`
	Services   []ServiceConfig `yaml:"services" json:"services"`
	Hooks      WorkspaceHooks  `yaml:"hooks" json:"hooks"`
	Workspace  WorkspaceConfig `yaml:"workspace" json:"workspace"`
	hookFields HookFieldPresence

	// hooksTimeoutDefaulted is true when Hooks.TimeoutMs came from DefaultConfig
	// rather than WORKFLOW.md. It lets WorkspaceHooks keep the SPEC default while
	// still honoring the legacy workspace.hooks.timeout_ms override during the
	// one-release migration window.
	hooksTimeoutDefaulted bool
	Agent                 AgentConfig   `yaml:"agent" json:"agent"`
	Codex                 CommandConfig `yaml:"codex" json:"codex"`
	Claude                CommandConfig `yaml:"claude" json:"claude"`
	Policy                PolicyConfig  `yaml:"policy" json:"policy"`
	Safety                SafetyConfig  `yaml:"safety" json:"safety"`
	Sandbox               SandboxConfig `yaml:"sandbox" json:"sandbox"`
	Verify                VerifyConfig  `yaml:"verify" json:"verify"`
	PR                    PRConfig      `yaml:"pr" json:"pr"`
}

type ServerConfig struct {
	Port int `yaml:"port" json:"port"`
}

type RepoConfig struct {
	Owner         string `yaml:"owner" json:"owner"`
	Name          string `yaml:"name" json:"name"`
	CloneURL      string `yaml:"clone_url" json:"clone_url"`
	DefaultBranch string `yaml:"default_branch" json:"default_branch"`
}

type ServiceConfig struct {
	Name    string                    `yaml:"name" json:"name"`
	Repo    RepoConfig                `yaml:"repo" json:"repo"`
	Tracker ServiceTrackerRouteConfig `yaml:"tracker" json:"tracker"`
}

// ServiceTrackerRouteConfig describes the tracker-side predicates that route a
// Linear issue to a configured service. The orchestrator only reads these
// fields during candidate selection; tracker writes remain agent/tool-side.
type ServiceTrackerRouteConfig struct {
	ProjectSlug  string            `yaml:"project_slug" json:"project_slug"`
	TeamKey      string            `yaml:"team_key" json:"team_key"`
	Labels       []string          `yaml:"labels" json:"labels"`
	CustomFields map[string]string `yaml:"custom_fields" json:"custom_fields"`
}

type TrackerConfig struct {
	Kind           string   `yaml:"kind" json:"kind"`
	APIKey         string   `yaml:"api_key" json:"api_key"`
	BaseURL        string   `yaml:"base_url" json:"base_url"`
	TeamKey        string   `yaml:"team_key" json:"team_key"`
	ProjectSlug    string   `yaml:"project_slug" json:"project_slug"`
	ActiveStates   []string `yaml:"active_states" json:"active_states"`
	TerminalStates []string `yaml:"terminal_states" json:"terminal_states"`
	// InactiveStates names non-terminal tracker states that make an already
	// running issue ineligible. Poll-tick reconciliation probes these states so
	// operator pauses such as Backlog/Human Review cancel in-flight workers
	// without treating issues missing from partial tracker listings as inactive.
	InactiveStates []string            `yaml:"inactive_states" json:"inactive_states"`
	PollIntervalMs int                 `yaml:"poll_interval_ms" json:"poll_interval_ms"`
	Statuses       TrackerStatusConfig `yaml:"statuses" json:"statuses"`
}

type PollingConfig struct {
	IntervalMs int `yaml:"interval_ms" json:"interval_ms"`
}

// TrackerStatusConfig names the workflow states used for tracker handoff
// updates. The defaults ("In Progress", "Human Review", "Rework") match the
// Linear template used by the personal profile; teams that customize their
// workflow states override the names without touching code. Per SPEC §1,
// tracker writes belong on the agent/tool side; transitional worker-side
// writes remain only until the app-server tool transport is complete.
type TrackerStatusConfig struct {
	InProgress  string `yaml:"in_progress" json:"in_progress"`
	HumanReview string `yaml:"human_review" json:"human_review"`
	Rework      string `yaml:"rework" json:"rework"`
}

type WorkspaceConfig struct {
	Root       string         `yaml:"root" json:"root"`
	Hooks      WorkspaceHooks `yaml:"hooks" json:"hooks"`
	hookFields HookFieldPresence
	rootSet    bool
}

func (w WorkspaceConfig) RootSet() bool {
	return w.rootSet
}

type WorkspaceHooks struct {
	AfterCreate  WorkspaceHook `yaml:"after_create" json:"after_create"`
	BeforeRun    WorkspaceHook `yaml:"before_run" json:"before_run"`
	AfterRun     WorkspaceHook `yaml:"after_run" json:"after_run"`
	BeforeRemove WorkspaceHook `yaml:"before_remove" json:"before_remove"`
	TimeoutMs    int           `yaml:"timeout_ms" json:"timeout_ms"`
	// EnvPassthrough names environment variables that hook subprocesses
	// inherit from the worker process. By default hooks see only a small
	// POSIX-shell baseline (PATH/HOME/USER/LANG/LC_*/TZ/TERM) — tracker
	// tokens, SSH credentials, and any other secret in the worker's env
	// are excluded. List names here to opt specific vars back in. See
	// docs/design/hook-verify-env-allowlist.md (#227).
	EnvPassthrough []string `yaml:"env_passthrough" json:"env_passthrough"`
}

type HookFieldPresence struct {
	AfterCreate    bool
	BeforeRun      bool
	AfterRun       bool
	BeforeRemove   bool
	TimeoutMs      bool
	EnvPassthrough bool
}

type WorkspaceHook struct {
	Commands []string `yaml:"commands" json:"commands"`
}

func (h *WorkspaceHook) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var command string
		if err := value.Decode(&command); err != nil {
			return err
		}
		if command != "" {
			h.Commands = []string{command}
		}
		return nil
	case yaml.SequenceNode:
		var commands []string
		if err := value.Decode(&commands); err != nil {
			return err
		}
		h.Commands = commands
		return nil
	case yaml.MappingNode:
		type workspaceHook WorkspaceHook
		var decoded workspaceHook
		if err := value.Decode(&decoded); err != nil {
			return err
		}
		*h = WorkspaceHook(decoded)
		return nil
	default:
		return fmt.Errorf("workspace hook must be a shell script string, command list, or commands object")
	}
}

func (h WorkspaceHook) HasCommands() bool {
	return len(h.Commands) > 0
}

func (h WorkspaceHooks) HasCommands() bool {
	return h.AfterCreate.HasCommands() || h.BeforeRun.HasCommands() || h.AfterRun.HasCommands() || h.BeforeRemove.HasCommands()
}

func (c Config) WorkspaceHooks() WorkspaceHooks {
	hooks := c.Hooks
	legacy := c.Workspace.Hooks
	if !c.hookFields.AfterCreate && legacy.AfterCreate.HasCommands() {
		hooks.AfterCreate = legacy.AfterCreate
	}
	if !c.hookFields.BeforeRun && legacy.BeforeRun.HasCommands() {
		hooks.BeforeRun = legacy.BeforeRun
	}
	if !c.hookFields.AfterRun && legacy.AfterRun.HasCommands() {
		hooks.AfterRun = legacy.AfterRun
	}
	if !c.hookFields.BeforeRemove && legacy.BeforeRemove.HasCommands() {
		hooks.BeforeRemove = legacy.BeforeRemove
	}
	if legacy.TimeoutMs > 0 && !c.hookFields.TimeoutMs {
		hooks.TimeoutMs = legacy.TimeoutMs
	}
	if !c.hookFields.EnvPassthrough && len(legacy.EnvPassthrough) > 0 {
		hooks.EnvPassthrough = legacy.EnvPassthrough
	}
	if hooks.TimeoutMs <= 0 {
		hooks.TimeoutMs = 60000
	}
	return hooks
}

type AgentConfig struct {
	Default                    string         `yaml:"default" json:"default"`
	MaxConcurrentAgents        int            `yaml:"max_concurrent_agents" json:"max_concurrent_agents"`
	MaxConcurrentAgentsByState map[string]int `yaml:"max_concurrent_agents_by_state" json:"max_concurrent_agents_by_state"`
	MaxTurns                   int            `yaml:"max_turns" json:"max_turns"`
	MaxRetryBackoffMs          int            `yaml:"max_retry_backoff_ms" json:"max_retry_backoff_ms"`
	// MaxRetryAttempts bounds failure-driven orchestrator retries after
	// retryable worker exits such as failed verification or missing summaries.
	// It counts scheduled retry entries, not the first run. A value of 1 means
	// "first run plus one retry"; an explicit 0 disables failure retries.
	MaxRetryAttempts *int `yaml:"max_retry_attempts" json:"max_retry_attempts"`
	// Timeout caps a single runner invocation. When exceeded, the runner
	// subprocess is killed and the task records a `runner_timeout` event.
	// Configured via YAML as `agent.timeout: 10m`. Zero means use the
	// schema default of 30m.
	Timeout time.Duration `yaml:"timeout" json:"timeout"`
	// MaxTimeoutRetries bounds how many times a task may be re-queued
	// after a runner timeout. This is intentionally separate from the
	// generic max_attempts (which covers verify/policy/other failures)
	// so a flaky runner cannot exhaust the global retry budget.
	//
	// Pointer-typed so we can distinguish "absent" (nil → schema default
	// of 1) from "explicitly set to 0" (no retry). Read via
	// MaxTimeoutRetriesValue() rather than dereferencing directly.
	MaxTimeoutRetries *int `yaml:"max_timeout_retries" json:"max_timeout_retries"`
	// PolicyViolationBudget bounds how many policy-violation feedback
	// entries an issue can accumulate before the worker fails the run
	// non-retryably. The default of 2 preserves the historical hardcoded
	// behavior; explicit 0 disables policy-violation-based suppression so
	// aggressive workflows can iterate until verify converges or another
	// non-retryable error fires. Read via PolicyViolationBudgetValue()
	// rather than dereferencing directly. Negative values are clamped to 0
	// (no policy-violation suppression). See #230 for the operator-visible
	// budget motivation.
	PolicyViolationBudget *int `yaml:"policy_violation_budget" json:"policy_violation_budget"`
}

// MaxTimeoutRetriesValue returns the effective runner-timeout retry
// budget. A nil pointer (field omitted from YAML) yields the schema
// default of 1 bonus retry; an explicit value—including 0—is honored
// as configured. Negative values are clamped to 0 since a negative
// retry budget is meaningless.
func (a AgentConfig) MaxTimeoutRetriesValue() int {
	if a.MaxTimeoutRetries == nil {
		return 1
	}
	if *a.MaxTimeoutRetries < 0 {
		return 0
	}
	return *a.MaxTimeoutRetries
}

// MaxRetryAttemptsValue returns the effective failure retry budget. A nil
// pointer yields the local automation default of one scheduled retry; explicit
// zero disables retryable worker failure retries. Negative values are rejected
// during validation and clamped defensively here.
func (a AgentConfig) MaxRetryAttemptsValue() int {
	if a.MaxRetryAttempts == nil {
		return 1
	}
	if *a.MaxRetryAttempts < 0 {
		return 0
	}
	return *a.MaxRetryAttempts
}

// DefaultPolicyViolationBudget is the historical hardcoded value preserved
// when `agent.policy_violation_budget` is absent from WORKFLOW.md.
const DefaultPolicyViolationBudget = 2

// PolicyViolationBudgetValue returns the effective per-issue policy-violation
// budget. A nil pointer (field omitted from YAML) yields
// DefaultPolicyViolationBudget; an explicit value — including 0 — is honored
// as configured. Negative values clamp to 0 (no suppression).
func (a AgentConfig) PolicyViolationBudgetValue() int {
	if a.PolicyViolationBudget == nil {
		return DefaultPolicyViolationBudget
	}
	if *a.PolicyViolationBudget < 0 {
		return 0
	}
	return *a.PolicyViolationBudget
}

type CommandConfig struct {
	Command string `yaml:"command" json:"command"`
	// Profile is consulted only by the codex runner. Allowed values: "safe"
	// (default), "bypass", "custom". The field lives on the shared
	// CommandConfig type to avoid splitting CodexConfig out for one field;
	// loader validation rejects non-empty Profile on the Claude embed so a
	// copy-paste mistake fails loud at load time instead of silently doing
	// nothing.
	Profile string `yaml:"profile,omitempty" json:"profile,omitempty"`
	// App-server runtime settings mirror the Symphony/Codex app-server
	// protocol fields. They are pass-through values for the Codex runtime,
	// so the loader validates only presence/timeout bounds rather than
	// maintaining Codex-owned enums.
	ApprovalPolicy    any            `yaml:"approval_policy,omitempty" json:"approval_policy,omitempty"`
	ThreadSandbox     string         `yaml:"thread_sandbox,omitempty" json:"thread_sandbox,omitempty"`
	TurnSandboxPolicy map[string]any `yaml:"turn_sandbox_policy,omitempty" json:"turn_sandbox_policy,omitempty"`
	TurnTimeoutMs     int            `yaml:"turn_timeout_ms,omitempty" json:"turn_timeout_ms,omitempty"`
	ReadTimeoutMs     int            `yaml:"read_timeout_ms,omitempty" json:"read_timeout_ms,omitempty"`
	StallTimeoutMs    int            `yaml:"stall_timeout_ms,omitempty" json:"stall_timeout_ms,omitempty"`
}

type PolicyConfig struct {
	Mode            string   `yaml:"mode" json:"mode"`
	AllowPaths      []string `yaml:"allow_paths" json:"allow_paths"`
	DenyPaths       []string `yaml:"deny_paths" json:"deny_paths"`
	MaxChangedFiles int      `yaml:"max_changed_files" json:"max_changed_files"`
	// MaxChangedLines bounds the total added+deleted lines reported by
	// `git diff --numstat`. The legacy YAML key `max_changed_loc` is still
	// honored via MaxChangedLOC below for back-compat.
	MaxChangedLines int `yaml:"max_changed_lines" json:"max_changed_lines"`
	MaxChangedLOC   int `yaml:"max_changed_loc" json:"max_changed_loc"`
}

// LineLimit returns the effective maximum changed lines, preferring the
// new MaxChangedLines field but falling back to the legacy MaxChangedLOC.
func (p PolicyConfig) LineLimit() int {
	if p.MaxChangedLines > 0 {
		return p.MaxChangedLines
	}
	return p.MaxChangedLOC
}

// SafetyConfig documents the policy envelope operators expect agents and
// reviewers to honor. It remains descriptive and operator-facing; SandboxConfig
// carries the opt-in worker-enforced controls.
type SafetyConfig struct {
	AllowedNetworks []string `yaml:"allowed_networks" json:"allowed_networks"`
	AllowedPaths    []string `yaml:"allowed_paths" json:"allowed_paths"`
	AllowedCommands []string `yaml:"allowed_commands" json:"allowed_commands"`
	Forbidden       []string `yaml:"forbidden" json:"forbidden"`
}

// SandboxConfig controls optional worker-side process hardening around agent
// invocation. It is disabled by default because Symphony does not mandate one
// universal sandbox posture; operators opt in per workflow when the host has a
// supported sandbox backend installed.
type SandboxConfig struct {
	Enabled               bool     `yaml:"enabled" json:"enabled"`
	Backend               string   `yaml:"backend" json:"backend"`
	NetworkMode           string   `yaml:"network" json:"network"`
	NetworkAllowlistCIDRs []string `yaml:"network_allowlist_cidrs" json:"network_allowlist_cidrs"`
	NetworkInterface      string   `yaml:"network_interface" json:"network_interface"`
	EnvAllowlist          []string `yaml:"env_allowlist" json:"env_allowlist"`
	CredentialFiles       []string `yaml:"credential_files" json:"credential_files"`
}

type VerifyConfig struct {
	Commands   []string         `yaml:"commands" json:"commands"`
	SecretScan SecretScanConfig `yaml:"secret_scan" json:"secret_scan"`
	// Timeout caps the entire verify phase. Zero (the default) means
	// unbounded so repos that have not opted in keep their previous
	// behavior. When exceeded, the in-flight command is killed via
	// context cancellation and remaining commands are skipped; the
	// task fails through the normal verify path unless AllowFailure
	// is set.
	Timeout time.Duration `yaml:"timeout" json:"timeout"`
	// AllowFailure, when true, lets the worker open a draft PR even
	// after verify reports failures, so the human can inspect what
	// the agent produced and what the verifier saw. The PR body is
	// annotated with a "verification failed (investigation mode)"
	// banner. Default false: failed verification blocks PR creation.
	AllowFailure bool `yaml:"allow_failure" json:"allow_failure"`
	// EnvPassthrough names environment variables that verify subprocesses
	// inherit from the worker process. Same allowlist semantics as
	// `hooks.env_passthrough` — typically holds build-tool env like
	// CARGO_HOME or GOMODCACHE. See
	// docs/design/hook-verify-env-allowlist.md (#227).
	EnvPassthrough []string `yaml:"env_passthrough" json:"env_passthrough"`
}

// SecretScanConfig describes an optional pre-push secret scanner that runs
// after verify commands and policy enforcement but before `git push`. The
// scanner is invoked in the workspace directory; a non-zero exit code is
// treated as a finding by default and blocks the push.
//
// Recommended tools (installed by the operator, not bundled here):
//
//   - gitleaks:   ["gitleaks", "detect", "--source", ".", "--no-banner"]
//   - trufflehog: ["trufflehog", "filesystem", "--no-update", "."]
//
// Leave Enabled=false (or omit the section) to keep the previous behavior.
type SecretScanConfig struct {
	// Enabled toggles the hook. When false, the worker skips the scan and
	// proceeds to push, preserving backward compatibility.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Command is argv to exec inside the workspace. The first element is
	// the binary; remaining elements are passed verbatim. No shell is used,
	// so quoting/expansion is not performed.
	Command []string `yaml:"command" json:"command"`
	// FailOnFinding controls whether a non-zero exit code blocks the push.
	// Defaults to true. Set to false to surface findings as a warning event
	// without aborting (useful while tuning false positives).
	FailOnFinding *bool `yaml:"fail_on_finding,omitempty" json:"fail_on_finding,omitempty"`
}

// ShouldFailOnFinding reports whether a non-zero exit from the scanner
// should block the push. The default is true; callers should pass through
// this method rather than reading FailOnFinding directly.
func (s SecretScanConfig) ShouldFailOnFinding() bool {
	if s.FailOnFinding == nil {
		return true
	}
	return *s.FailOnFinding
}

type PRConfig struct {
	Draft     bool     `yaml:"draft" json:"draft"`
	Labels    []string `yaml:"labels" json:"labels"`
	Reviewers []string `yaml:"reviewers" json:"reviewers"`
}

func DefaultConfig() Config {
	return Config{
		Server: ServerConfig{Port: 4000},
		Tracker: TrackerConfig{
			Kind:           "gitea",
			ActiveStates:   []string{"AI Ready", "In Progress", "Rework"},
			TerminalStates: []string{"Done", "Canceled", "Cancelled", "Closed", "Duplicate"},
			PollIntervalMs: 30000,
			Statuses: TrackerStatusConfig{
				InProgress:  "In Progress",
				HumanReview: "Human Review",
				Rework:      "Rework",
			},
		},
		Polling:               PollingConfig{IntervalMs: 30000},
		Hooks:                 WorkspaceHooks{TimeoutMs: 60000},
		hooksTimeoutDefaulted: true,
		Workspace:             WorkspaceConfig{Root: "~/aiops-workspaces"},
		// Agent.MaxRetryAttempts is intentionally left nil here so the
		// "absent" signal survives YAML overlay. The effective default of
		// one failure retry is supplied by MaxRetryAttemptsValue().
		// Agent.MaxTimeoutRetries is intentionally left nil here so the
		// "absent" signal survives a YAML unmarshal that overlays this
		// default. The effective default of 1 retry is supplied by
		// MaxTimeoutRetriesValue().
		Agent: AgentConfig{
			Default:             "mock",
			MaxConcurrentAgents: 1,
			MaxTurns:            20,
			MaxRetryBackoffMs:   300000,
			Timeout:             30 * time.Minute,
		},
		Codex: CommandConfig{
			Command:        "codex exec",
			Profile:        "safe",
			ApprovalPolicy: map[string]any{"reject": map[string]any{"sandbox_approval": true, "rules": true, "mcp_elicitations": true}},
			ThreadSandbox:  "workspace-write",
			TurnTimeoutMs:  3600000,
			ReadTimeoutMs:  5000,
			StallTimeoutMs: 300000,
		},
		Claude:  CommandConfig{Command: "claude"},
		Policy:  PolicyConfig{Mode: "draft_pr", MaxChangedFiles: 12, MaxChangedLOC: 300},
		Sandbox: SandboxConfig{Backend: "none", NetworkMode: "none"},
		Verify:  VerifyConfig{Commands: []string{}},
		// PR.Draft defaults to false so workflows that omit `pr.draft` keep
		// the historical worker behavior of opening ready-for-review PRs.
		// Profiles that want draft PRs (e.g. company-cautious) opt in by
		// setting `pr.draft: true` in their WORKFLOW.md front matter.
		PR: PRConfig{Draft: false, Labels: []string{"ai-generated", "needs-review"}},
	}
}
