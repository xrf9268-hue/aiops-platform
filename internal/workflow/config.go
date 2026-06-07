package workflow

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// MaskCloneURL strips embedded basic-auth from a clone URL so it is safe to log
// or display. The SSH-style git@host:path form carries no userinfo and is
// returned unchanged — url.Parse rejects it (a colon in the first path segment)
// and the fallback then finds no `scheme://` authority to strip. A bare
// username with no password is also stripped: by convention an embedded user in
// an HTTPS clone URL is a token alias (e.g. "oauth2", "x-access-token", or the
// token itself), not a real account name.
//
// Fail closed: a credentialed URL that does not parse as net/url (e.g. a
// malformed port or a space in the userinfo) must still not leak its credential
// into a log or error string, so a conservative string strip of
// `scheme://userinfo@` runs on the url.Parse error path. Returning the raw
// string there would leak the token — the same masking-must-not-leak class as
// #469/#483 (#676).
func MaskCloneURL(raw string) string {
	if raw == "" {
		return raw
	}
	if u, err := url.Parse(raw); err == nil {
		if u.User == nil {
			return raw
		}
		u.User = nil
		return u.String()
	}
	return stripCloneURLUserinfo(raw)
}

// stripCloneURLUserinfo removes a `userinfo@` prefix from the authority of a
// scheme://-form URL using only string operations, so it masks even when
// url.Parse rejects the input. It mirrors url.Parse's rule that userinfo ends at
// the last `@` in the authority, and it leaves an `@` in the path/query
// untouched. Inputs without a `scheme://` authority (the SSH scp form
// git@host:path, or a plain path) carry no basic-auth userinfo and are returned
// unchanged.
func stripCloneURLUserinfo(raw string) string {
	const sep = "://"
	schemeIdx := strings.Index(raw, sep)
	if schemeIdx < 0 {
		return raw
	}
	authStart := schemeIdx + len(sep)
	rest := raw[authStart:]
	authority := rest
	tail := ""
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		authority, tail = rest[:end], rest[end:]
	}
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return raw
	}
	return raw[:authStart] + authority[at+1:] + tail
}

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
	Sandbox               SandboxConfig `yaml:"sandbox" json:"sandbox"`
	Verify                VerifyConfig  `yaml:"verify" json:"verify"`
}

type ServerConfig struct {
	// Host is the bind address for the dashboard/state HTTP server. It
	// defaults to 127.0.0.1 (SPEC §15.3 loopback-only trust boundary). An
	// empty value is treated as the loopback default rather than a bind-all
	// wildcard so a blank override never silently widens exposure. Set it to
	// 0.0.0.0 only behind a loopback-scoped host port mapping (see the
	// dashboard Compose overlay) with AIOPS_STATE_API_TOKEN requiring auth for
	// every request.
	Host string `yaml:"host" json:"host"`
	Port int    `yaml:"port" json:"port"`
	// portSet records whether server.port was explicitly present in the
	// WORKFLOW.md front matter (vs. inherited from DefaultConfig). It lets
	// `worker --print-config` distinguish a `workflow` source from a
	// `default` one without re-parsing the file. Unexported so YAML never
	// populates it; the loader sets it after Unmarshal (mirrors
	// WorkspaceConfig.rootSet).
	portSet bool
}

// PortSet reports whether server.port was explicitly set in WORKFLOW.md
// front matter. False means the effective Port came from DefaultConfig.
func (s ServerConfig) PortSet() bool {
	return s.portSet
}

type RepoConfig struct {
	Owner         string `yaml:"owner" json:"owner"`
	Name          string `yaml:"name" json:"name"`
	CloneURL      string `yaml:"clone_url" json:"clone_url"`
	DefaultBranch string `yaml:"default_branch" json:"default_branch"`
}

type TrackerConfig struct {
	Kind         string `yaml:"kind" json:"kind"`
	APIKey       string `yaml:"api_key" json:"api_key"`
	apiKeyEnvVar string
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
	InactiveStates []string `yaml:"inactive_states" json:"inactive_states"`
	// RequiredLabels is the SPEC §4.1.1 / §6.4 opt-in dispatch gate
	// (upstream Symphony #88): an issue MUST carry every configured label
	// (matched case-insensitively after trimming) to be dispatched or to
	// keep running; removing a required label stops/releases in-flight work
	// on the next poll. Default `[]` disables the gate. A blank configured
	// label matches no issue. Normalized (trim + lowercase + de-dupe) at
	// load by normalizeRequiredLabels.
	//
	// Label projection ceiling (#705): the Linear adapter projects labels up
	// to the API's 250-per-issue single-page maximum (linear.go
	// listLinearIssuesQuery / issueStatesByIDsQuery). A required label sorted
	// past that window is outside the gate's evidence; since 250 is the API
	// page cap and a single issue carrying 250+ labels is pathological, keep
	// the marker set small rather than relying on labels beyond the ceiling.
	RequiredLabels []string `yaml:"required_labels" json:"required_labels"`
	PollIntervalMs int      `yaml:"poll_interval_ms" json:"poll_interval_ms"`
	// PaginationMaxPages caps one tracker pagination scan. Zero keeps the
	// selected adapter's default budget.
	PaginationMaxPages int                 `yaml:"pagination_max_pages" json:"pagination_max_pages,omitempty"`
	Statuses           TrackerStatusConfig `yaml:"statuses" json:"statuses"`
}

// normalizeLoadedConfig applies the post-parse normalizations that run after
// front-matter merge regardless of whether front matter was present: the
// per-state concurrency-limit canonicalization and the SPEC §6.4
// required-labels trim/lowercase/de-dupe gate normalization.
func normalizeLoadedConfig(cfg *Config) {
	cfg.Agent.MaxConcurrentAgentsByState = NormalizeStateConcurrencyLimits(cfg.Agent.MaxConcurrentAgentsByState)
	cfg.Tracker.RequiredLabels = normalizeRequiredLabels(cfg.Tracker.RequiredLabels)
}

// normalizeRequiredLabels mirrors the Elixir reference's
// config/schema.ex update_change for `required_labels`: trim, lowercase, and
// de-dupe each entry so the gate matches the equally-normalized issue labels.
// A configured blank is preserved as "" (SPEC: "a blank configured label
// matches no issue") rather than dropped, so `["" ]` keeps the gate active and
// blocks every issue instead of silently disabling it. A nil/empty input is
// returned unchanged so the default `[]` keeps the gate off.
func normalizeRequiredLabels(in []string) []string {
	if len(in) == 0 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, label := range in {
		norm := strings.ToLower(strings.TrimSpace(label))
		if _, dup := seen[norm]; dup {
			continue
		}
		seen[norm] = struct{}{}
		out = append(out, norm)
	}
	return out
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

func (c Config) WorkspaceHooks() WorkspaceHooks { //nolint:gocognit // baseline (#521)
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
	MaxContinuationTurns       int            `yaml:"max_continuation_turns" json:"max_continuation_turns"`
	MaxRetryBackoffMs          int            `yaml:"max_retry_backoff_ms" json:"max_retry_backoff_ms"`
	// Timeout caps a single runner invocation. When exceeded, the runner
	// subprocess is killed and the task records a `runner_timeout` event.
	// Configured via YAML as `agent.timeout: 10m`. Zero means use the
	// schema default of 30m.
	Timeout time.Duration `yaml:"timeout" json:"timeout"`
}

type CommandConfig struct {
	Command string `yaml:"command" json:"command"`
	// EnvPassthrough names worker environment variables the agent subprocess may
	// inherit in addition to the runner baseline. Tracker/repo API tokens stay
	// behind orchestrator-owned tools and are rejected here.
	EnvPassthrough []string `yaml:"env_passthrough,omitempty" json:"env_passthrough,omitempty"`
	// App-server runtime settings mirror the Symphony/Codex app-server
	// protocol fields. Keep Codex-owned wire shapes typed here so schema
	// drift fails at workflow load or runner tests instead of in a live run.
	ApprovalPolicy    any                `yaml:"approval_policy,omitempty" json:"approval_policy,omitempty"`
	ThreadSandbox     string             `yaml:"thread_sandbox,omitempty" json:"thread_sandbox,omitempty"`
	TurnSandboxPolicy CodexSandboxPolicy `yaml:"turn_sandbox_policy,omitempty" json:"turn_sandbox_policy,omitempty"`
	// turnSandboxPolicySet records whether codex.turn_sandbox_policy was
	// explicitly present in the front matter. DefaultConfig() pre-fills
	// TurnSandboxPolicy, so it cannot carry the "absent" signal through a YAML
	// overlay the way a nil-pointer field can. The
	// loader needs that signal to decide whether to derive the per-turn policy
	// from thread_sandbox; an operator who set turn_sandbox_policy explicitly
	// keeps full control.
	turnSandboxPolicySet bool
	TurnTimeoutMs        int `yaml:"turn_timeout_ms,omitempty" json:"turn_timeout_ms,omitempty"`
	ReadTimeoutMs        int `yaml:"read_timeout_ms,omitempty" json:"read_timeout_ms,omitempty"`
	StallTimeoutMs       int `yaml:"stall_timeout_ms,omitempty" json:"stall_timeout_ms,omitempty"`
	// LinearGraphQL narrows the agent-visible linear_graphql client-side tool
	// (SPEC §15.5 harness hardening). The field lives on the shared
	// CommandConfig type to avoid splitting a Codex-only config out for one
	// embed; the loader rejects a non-zero embed on Claude so a copy-paste
	// mistake fails loud at load time. See #298.
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

// PolicyConfig carries the workflow run mode. The worker-enforced path/diffstat
// gate (allow_paths / deny_paths / max_changed_*) was removed under #561: SPEC
// §3.2 homes scope/validation rules in the operator's WORKFLOW.md prompt, the
// gate ran post-push (could only flag, never prevent) and raced reconcile-cancel,
// and upstream has no such config. Hard path prevention belongs to the `sandbox`
// write restrictions; scope guidance belongs to the prompt.
type PolicyConfig struct {
	// Mode selects the run mode: "draft_pr" (default) or "analysis_only".
	Mode string `yaml:"mode" json:"mode"`
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

// VerifyConfig carries the operator-declared verification surface. Per SPEC §1
// verification is the coding agent's responsibility: Commands are surfaced to
// the agent in the rendered prompt (via AppendVerifyDirective) rather than run
// by the worker.
type VerifyConfig struct {
	Commands []string `yaml:"commands" json:"commands"`
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
		Agent: AgentConfig{
			Default:              "mock",
			MaxConcurrentAgents:  10,
			MaxTurns:             20,
			MaxContinuationTurns: 20,
			MaxRetryBackoffMs:    300000,
			Timeout:              30 * time.Minute,
		},
		Codex: CommandConfig{
			Command: "codex app-server",
			// See loader.go for why this is `granular:` with all flags
			// false instead of the obsolete `reject:` shape (#329).
			ApprovalPolicy: map[string]any{"granular": map[string]any{
				"sandbox_approval":    false,
				"rules":               false,
				"skill_approval":      false,
				"request_permissions": false,
				"mcp_elicitations":    false,
			}},
			ThreadSandbox: "workspace-write",
			// TurnSandboxPolicy pre-fills the workspace-write default so
			// callers that build a Config from DefaultConfig() without running
			// the loader (e.g. the worker's no-WORKFLOW.md startup path) still
			// send a valid turn sandbox. When a WORKFLOW.md is loaded and the
			// operator did NOT set codex.turn_sandbox_policy, the loader
			// re-derives this from codex.thread_sandbox so a single
			// thread_sandbox knob governs effective turn permissions
			// (DEVIATIONS D32); an explicit turn_sandbox_policy overrides.
			TurnSandboxPolicy: DefaultCodexSandboxPolicy(),
			TurnTimeoutMs:     3600000,
			ReadTimeoutMs:     5000,
			StallTimeoutMs:    300000,
		},
		Claude:  CommandConfig{Command: "claude"},
		Policy:  PolicyConfig{Mode: "draft_pr"},
		Sandbox: SandboxConfig{Backend: "none", NetworkMode: "none"},
		Verify:  VerifyConfig{Commands: []string{}},
	}
}
