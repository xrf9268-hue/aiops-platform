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
	if strings.TrimSpace(front) != "" {
		if err := yaml.Unmarshal([]byte(front), &cfg); err != nil {
			return nil, fmt.Errorf("parse workflow front matter: %w", err)
		}
	}
	expandConfig(&cfg)
	return &Workflow{Path: path, Config: cfg, PromptTemplate: strings.TrimSpace(body)}, nil
}

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
	// MaxTimeoutRetries defaults to 1 when unspecified or negative so
	// callers do not silently lose their one bonus retry budget on a
	// transient agent hang. Set explicitly to 0 in YAML (or any other
	// concrete non-negative value) to override.
	if cfg.Agent.MaxTimeoutRetries == 0 || cfg.Agent.MaxTimeoutRetries < 0 {
		cfg.Agent.MaxTimeoutRetries = 1
	}
	if cfg.Tracker.PollIntervalMs <= 0 {
		cfg.Tracker.PollIntervalMs = 30000
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
