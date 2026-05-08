package workflow

import "time"

type Config struct {
	Repo      RepoConfig      `yaml:"repo"`
	Tracker   TrackerConfig   `yaml:"tracker"`
	Workspace WorkspaceConfig `yaml:"workspace"`
	Agent     AgentConfig     `yaml:"agent"`
	Codex     CommandConfig   `yaml:"codex"`
	Claude    CommandConfig   `yaml:"claude"`
	Policy    PolicyConfig    `yaml:"policy"`
	Verify    VerifyConfig    `yaml:"verify"`
	PR        PRConfig        `yaml:"pr"`
}

type RepoConfig struct {
	Owner         string `yaml:"owner"`
	Name          string `yaml:"name"`
	CloneURL      string `yaml:"clone_url"`
	DefaultBranch string `yaml:"default_branch"`
}

type TrackerConfig struct {
	Kind           string   `yaml:"kind"`
	APIKey         string   `yaml:"api_key"`
	TeamKey        string   `yaml:"team_key"`
	ProjectSlug    string   `yaml:"project_slug"`
	ActiveStates   []string `yaml:"active_states"`
	TerminalStates []string `yaml:"terminal_states"`
	PollIntervalMs int      `yaml:"poll_interval_ms"`
}

type WorkspaceConfig struct {
	Root string `yaml:"root"`
}

type AgentConfig struct {
	Default             string `yaml:"default"`
	Fallback            string `yaml:"fallback"`
	MaxConcurrentAgents int    `yaml:"max_concurrent_agents"`
	MaxTurns            int    `yaml:"max_turns"`
	// Timeout caps a single runner invocation. When exceeded, the runner
	// subprocess is killed and the task records a `runner_timeout` event.
	// Configured via YAML as `agent.timeout: 10m`. Zero means use the
	// schema default of 30m.
	Timeout time.Duration `yaml:"timeout"`
	// MaxTimeoutRetries bounds how many times a task may be re-queued
	// after a runner timeout. This is intentionally separate from the
	// generic max_attempts (which covers verify/policy/other failures)
	// so a flaky runner cannot exhaust the global retry budget.
	MaxTimeoutRetries int `yaml:"max_timeout_retries"`
}

type CommandConfig struct {
	Command string `yaml:"command"`
}

type PolicyConfig struct {
	Mode            string   `yaml:"mode"`
	AllowPaths      []string `yaml:"allow_paths"`
	DenyPaths       []string `yaml:"deny_paths"`
	MaxChangedFiles int      `yaml:"max_changed_files"`
	// MaxChangedLines bounds the total added+deleted lines reported by
	// `git diff --numstat`. The legacy YAML key `max_changed_loc` is still
	// honored via MaxChangedLOC below for back-compat.
	MaxChangedLines int `yaml:"max_changed_lines"`
	MaxChangedLOC   int `yaml:"max_changed_loc"`
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
	Commands []string `yaml:"commands"`
}

type PRConfig struct {
	Draft     bool     `yaml:"draft"`
	Labels    []string `yaml:"labels"`
	Reviewers []string `yaml:"reviewers"`
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
		Agent: AgentConfig{
			Default:             "mock",
			Fallback:            "claude",
			MaxConcurrentAgents: 1,
			MaxTurns:            8,
			Timeout:             30 * time.Minute,
			MaxTimeoutRetries:   1,
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
