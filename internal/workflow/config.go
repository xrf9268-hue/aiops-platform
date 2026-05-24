package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// defaultWorkspaceRoot resolves SPEC §6.4's `<system-temp>/symphony_workspaces`
// default at call time so it tracks the running process's TMPDIR (Elixir uses
// `Path.join(System.tmp_dir!(), "symphony_workspaces")` — schema.ex:93).
// Resolving in DefaultConfig keeps it absolute so operators on non-root hosts
// can write to it on first run; the previous bare `/symphony_workspaces`
// shipped a path only root could create.
func defaultWorkspaceRoot() string {
	return filepath.Join(os.TempDir(), "symphony_workspaces")
}

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
	// Host is the bind address for the dashboard/state HTTP server. It
	// defaults to 127.0.0.1 (SPEC §15.3 loopback-only trust boundary). An
	// empty value is treated as the loopback default rather than a bind-all
	// wildcard so a blank override never silently widens exposure. Set it to
	// 0.0.0.0 only behind a loopback-scoped host port mapping (see the
	// dashboard Compose overlay); the loopback Host-header guard is not
	// authentication, so a routable bind needs auth that this server lacks.
	Host string `yaml:"host" json:"host"`
	Port int    `yaml:"port" json:"port"`
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
	Kind   string `yaml:"kind" json:"kind"`
	APIKey string `yaml:"api_key" json:"api_key"`
	// Endpoint is the tracker base/GraphQL URL (SPEC §5.3.1). For
	// `kind: linear` the default is `https://api.linear.app/graphql`;
	// GitHub Enterprise / Gitea installs name their REST root here.
	// `tracker.base_url` is accepted as a deprecated alias and migrated
	// by the loader (see migrateTrackerEndpoint in loader.go).
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	// BaseURL is the pre-#242 field name kept for one release as a
	// deprecated alias. Reads/writes outside the loader should use
	// Endpoint; this field exists so legacy WORKFLOW.md files keep
	// parsing while the loader emits a deprecation log.
	BaseURL        string   `yaml:"base_url" json:"base_url,omitempty"`
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
	// It counts scheduled retry entries, not the first run.
	//
	// SPEC §8.4 / §16.6 / §4.1.8 do not budget retry attempts: an orchestrator
	// keeps tapping the exponential-backoff wall (capped at
	// agent.max_retry_backoff_ms) until the tracker takes the issue out of
	// active work. Leaving this field absent (nil) preserves that SPEC default
	// — failure retries are unbounded. An explicit positive integer opts into
	// SPEC §15.5 harness-hardening: caps the number of scheduled retries and
	// pins the issue under OrchestratorState.Failed until the tracker's
	// `state` or `updated_at` changes. An explicit 0 means "no failure
	// retries at all" — also an opt-in harness extension, surfaced for
	// operators who want a single shot per tracker transition. Read via
	// MaxRetryAttemptsValue() rather than dereferencing directly.
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
	// SPEC §8.4 expresses runner-timeout recovery purely through backoff
	// (no attempt-count budget), so leaving this field absent (nil) means
	// "re-queue forever, bounded only by tracker state changes" — the SPEC
	// default. An explicit positive integer opts into the SPEC §15.5
	// harness-hardening cap; an explicit 0 disables runner-timeout
	// re-queues entirely. Read via MaxTimeoutRetriesValue() rather than
	// dereferencing directly.
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

// UnboundedRetryBudget is the sentinel MaxTimeoutRetriesValue() and
// MaxRetryAttemptsValue() return when the corresponding YAML field is
// absent. It signals "no cap" to downstream callers (orchestrator,
// queue.FailTimeout) so the SPEC §8.4 default — keep retrying with
// exponential backoff until the tracker takes the issue out of active
// work — is what an out-of-the-box workflow gets. Any value < 0 is
// equivalent to UnboundedRetryBudget; callers should compare with
// `< 0` rather than `== UnboundedRetryBudget` so future sentinel
// renumbering does not require a sweep.
const UnboundedRetryBudget = -1

// MaxTimeoutRetriesValue returns the effective runner-timeout retry
// budget. A nil pointer (field omitted from YAML) yields
// UnboundedRetryBudget so SPEC §8.4 backoff is the only ceiling; an
// explicit non-negative value opts into a harness-hardening cap and
// is honored as configured (including explicit 0, which disables the
// re-queue path entirely). Negative explicit values are rejected at
// load; this accessor clamps defensively to UnboundedRetryBudget for
// any caller that constructed a config in-process with a negative
// pointer.
func (a AgentConfig) MaxTimeoutRetriesValue() int {
	if a.MaxTimeoutRetries == nil {
		return UnboundedRetryBudget
	}
	if *a.MaxTimeoutRetries < 0 {
		return UnboundedRetryBudget
	}
	return *a.MaxTimeoutRetries
}

// MaxRetryAttemptsValue returns the effective failure retry budget.
// A nil pointer yields UnboundedRetryBudget so the SPEC §8.4 / §16.6
// "retry forever, bounded only by backoff and tracker state" contract
// is the default; an explicit non-negative value opts into the SPEC
// §15.5 harness-hardening cap. Explicit 0 means "no failure retries at
// all" (a deliberate single-shot mode), not "use the default". Negative
// explicit values are rejected during validation; this accessor clamps
// defensively to UnboundedRetryBudget.
func (a AgentConfig) MaxRetryAttemptsValue() int {
	if a.MaxRetryAttempts == nil {
		return UnboundedRetryBudget
	}
	if *a.MaxRetryAttempts < 0 {
		return UnboundedRetryBudget
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
	// LinearGraphQL narrows the agent-visible linear_graphql client-side tool
	// (SPEC §15.5 harness hardening). The field lives on the shared
	// CommandConfig type for the same pragmatic reason as Profile above; the
	// loader rejects a non-zero embed on Claude so a copy-paste mistake fails
	// loud at load time. See #298.
	LinearGraphQL LinearGraphQLConfig `yaml:"linear_graphql,omitempty" json:"linear_graphql,omitempty"`
}

// LinearGraphQLConfig narrows the agent-visible linear_graphql tool surface
// to operator-chosen GraphQL operations. With the zero value (the default),
// the runner rejects every GraphQL mutation issued through the tool before
// any request leaves the process; the agent can still read everything Linear
// permits. Operators that rely on agent-side tracker writes (issueUpdate for
// state moves, commentCreate for handoff comments) flip AllowMutations to
// true once in WORKFLOW.md and optionally constrain the mutation field names
// via AllowedMutations.
type LinearGraphQLConfig struct {
	// AllowMutations turns the mutation gate off when true. With the
	// zero value the runner returns a typed error for any mutation and
	// no HTTP request is dispatched.
	AllowMutations bool `yaml:"allow_mutations,omitempty" json:"allow_mutations,omitempty"`
	// AllowedMutations is the optional per-operation allow-list applied
	// once AllowMutations is true. Entries are top-level GraphQL field
	// names on Linear's Mutation root (e.g. "issueUpdate",
	// "commentCreate"). When the list is empty, every mutation is
	// accepted; when populated, mutations whose first selected field is
	// not in the list are rejected with a typed error.
	AllowedMutations []string `yaml:"allowed_mutations,omitempty" json:"allowed_mutations,omitempty"`
}

// IsZero reports whether the config carries no operator-set narrowing.
// Used by the loader to validate that the Claude embed of CommandConfig
// stays empty.
func (c LinearGraphQLConfig) IsZero() bool {
	return !c.AllowMutations && len(c.AllowedMutations) == 0
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
		Server: ServerConfig{Host: "127.0.0.1", Port: 4000},
		// Tracker.ActiveStates / TerminalStates mirror SPEC §6.4's
		// cheat-sheet defaults (Todo/In Progress active; Closed,
		// Cancelled, Canceled, Duplicate, Done terminal). Workflows that
		// run on a non-SPEC vocabulary (e.g. the personal profile's
		// "AI Ready"/"Rework") declare the override in WORKFLOW.md front
		// matter — see examples/WORKFLOW.md.
		// Tracker.Kind is intentionally left empty: SPEC §6.4 marks it
		// REQUIRED, so DefaultConfig must not silently default it. A
		// WORKFLOW.md that declares front matter must set `tracker.kind`
		// explicitly; the loader rejects an empty kind with a SPEC-aware
		// error (see validateConfig in loader.go).
		Tracker: TrackerConfig{
			ActiveStates:   []string{"Todo", "In Progress"},
			TerminalStates: []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"},
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
		Workspace:             WorkspaceConfig{Root: defaultWorkspaceRoot()},
		// Agent.MaxRetryAttempts is intentionally left nil here so the
		// "absent" signal survives YAML overlay. The effective default of
		// one failure retry is supplied by MaxRetryAttemptsValue().
		// Agent.MaxTimeoutRetries is intentionally left nil here so the
		// "absent" signal survives a YAML unmarshal that overlays this
		// default. The effective default of 1 retry is supplied by
		// MaxTimeoutRetriesValue().
		Agent: AgentConfig{
			Default:             "mock",
			MaxConcurrentAgents: 10,
			MaxTurns:            20,
			MaxRetryBackoffMs:   300000,
			Timeout:             30 * time.Minute,
		},
		Codex: CommandConfig{
			Command: "codex app-server",
			Profile: "safe",
			// See loader.go for why this is `granular:` with all flags
			// false instead of the obsolete `reject:` shape (#329).
			ApprovalPolicy: map[string]any{"granular": map[string]any{
				"sandbox_approval":    false,
				"rules":               false,
				"skill_approval":      false,
				"request_permissions": false,
				"mcp_elicitations":    false,
			}},
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
