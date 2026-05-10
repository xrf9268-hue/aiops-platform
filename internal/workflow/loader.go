package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Workflow struct {
	Path           string
	Config         Config
	PromptTemplate string
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
		if err := rejectRemovedFields([]byte(front)); err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal([]byte(front), &cfg); err != nil {
			return nil, fmt.Errorf("parse workflow front matter: %w", err)
		}
	}
	expandConfig(&cfg)
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
	return &Workflow{Path: path, Config: cfg, PromptTemplate: strings.TrimSpace(body)}, nil
}

// supportedTrackerKinds enumerates the tracker integrations the platform
// actually wires up today (see cmd/linear-poller and the Gitea webhook
// path in cmd/api). Anything outside this set would parse as a typed
// string but go nowhere at runtime, so reject it at Load.
var supportedTrackerKinds = map[string]struct{}{
	"gitea":  {},
	"linear": {},
}

// supportedAgentDefaults mirrors the runner registry in
// internal/runner.New. Keeping the two lists in sync at the schema layer
// turns "unknown runner: X" — which today only surfaces after a task is
// claimed and the workspace prepared — into a load-time configuration
// error with the workflow file path attached.
var supportedAgentDefaults = map[string]struct{}{
	"mock":   {},
	"codex":  {},
	"claude": {},
}

// validateConfig enforces the required-field and enum constraints that
// the typed YAML decoder cannot express on its own. It runs after
// expandConfig so env-var indirections (e.g. `clone_url: $REPO_URL`)
// are evaluated before non-empty checks. Errors include the workflow
// file path and the offending field/value so operators can fix the
// source rather than chasing runtime symptoms (issue #9).
func validateConfig(path string, cfg Config) error {
	if strings.TrimSpace(cfg.Repo.CloneURL) == "" {
		return fmt.Errorf("%s: repo.clone_url is required", path)
	}
	if _, ok := supportedTrackerKinds[cfg.Tracker.Kind]; !ok {
		return fmt.Errorf("%s: tracker.kind %q is not supported (allowed: gitea, linear)", path, cfg.Tracker.Kind)
	}
	if _, ok := supportedAgentDefaults[cfg.Agent.Default]; !ok {
		return fmt.Errorf("%s: agent.default %q is not supported (allowed: mock, codex, claude)", path, cfg.Agent.Default)
	}
	return nil
}

// rejectRemovedFields surfaces a clear error for keys that were once part
// of the schema but have been removed. The typed Unmarshal above silently
// drops unknown fields, which would let workflow authors keep believing
// the key still controls behavior. Targeted detection keeps existing
// benign extras working while flagging known footguns.
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
	cfg.Tracker.APIKey = os.ExpandEnv(cfg.Tracker.APIKey)
	cfg.Repo.CloneURL = os.ExpandEnv(cfg.Repo.CloneURL)
	cfg.Workspace.Root = expandPath(os.ExpandEnv(cfg.Workspace.Root))
	cfg.Codex.Command = os.ExpandEnv(cfg.Codex.Command)
	cfg.Claude.Command = os.ExpandEnv(cfg.Claude.Command)
	if cfg.Repo.DefaultBranch == "" {
		cfg.Repo.DefaultBranch = "main"
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
	if cfg.Tracker.PollIntervalMs <= 0 {
		cfg.Tracker.PollIntervalMs = 30000
	}
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
