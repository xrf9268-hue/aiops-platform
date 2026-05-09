package workflow

import "time"

type Config struct {
	Repo      RepoConfig      `yaml:"repo" json:"repo"`
	Tracker   TrackerConfig   `yaml:"tracker" json:"tracker"`
	Workspace WorkspaceConfig `yaml:"workspace" json:"workspace"`
	Agent     AgentConfig     `yaml:"agent" json:"agent"`
	Codex     CommandConfig   `yaml:"codex" json:"codex"`
	Claude    CommandConfig   `yaml:"claude" json:"claude"`
	Policy    PolicyConfig    `yaml:"policy" json:"policy"`
	Verify    VerifyConfig    `yaml:"verify" json:"verify"`
	PR        PRConfig        `yaml:"pr" json:"pr"`
}

type RepoConfig struct {
	Owner         string `yaml:"owner" json:"owner"`
	Name          string `yaml:"name" json:"name"`
	CloneURL      string `yaml:"clone_url" json:"clone_url"`
	DefaultBranch string `yaml:"default_branch" json:"default_branch"`
}

type TrackerConfig struct {
	Kind           string   `yaml:"kind" json:"kind"`
	APIKey         string   `yaml:"api_key" json:"api_key"`
	TeamKey        string   `yaml:"team_key" json:"team_key"`
	ProjectSlug    string   `yaml:"project_slug" json:"project_slug"`
	ActiveStates   []string `yaml:"active_states" json:"active_states"`
	TerminalStates []string `yaml:"terminal_states" json:"terminal_states"`
	PollIntervalMs int      `yaml:"poll_interval_ms" json:"poll_interval_ms"`
}

type WorkspaceConfig struct {
	Root string `yaml:"root" json:"root"`
}

type AgentConfig struct {
	Default             string `yaml:"default" json:"default"`
	MaxConcurrentAgents int    `yaml:"max_concurrent_agents" json:"max_concurrent_agents"`
	MaxTurns            int    `yaml:"max_turns" json:"max_turns"`
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

type CommandConfig struct {
	Command string `yaml:"command" json:"command"`
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
		Tracker: TrackerConfig{
			Kind:           "gitea",
			ActiveStates:   []string{"AI Ready", "In Progress", "Rework"},
			TerminalStates: []string{"Done", "Canceled"},
			PollIntervalMs: 30000,
		},
		Workspace: WorkspaceConfig{Root: "~/aiops-workspaces"},
		// Agent.MaxTimeoutRetries is intentionally left nil here so the
		// "absent" signal survives a YAML unmarshal that overlays this
		// default. The effective default of 1 retry is supplied by
		// MaxTimeoutRetriesValue().
		Agent: AgentConfig{
			Default:             "mock",
			MaxConcurrentAgents: 1,
			MaxTurns:            8,
			Timeout:             30 * time.Minute,
		},
		Codex:  CommandConfig{Command: "codex exec"},
		Claude: CommandConfig{Command: "claude"},
		Policy: PolicyConfig{Mode: "draft_pr", MaxChangedFiles: 12, MaxChangedLOC: 300},
		Verify: VerifyConfig{Commands: []string{}},
		// PR.Draft defaults to false so workflows that omit `pr.draft` keep
		// the historical worker behavior of opening ready-for-review PRs.
		// Profiles that want draft PRs (e.g. company-cautious) opt in by
		// setting `pr.draft: true` in their WORKFLOW.md front matter.
		PR: PRConfig{Draft: false, Labels: []string{"ai-generated", "needs-review"}},
	}
}
