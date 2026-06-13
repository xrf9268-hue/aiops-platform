package workflow

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func expandConfig(cfg *Config) error {
	return expandConfigForWorkflowPath("", cfg)
}

func expandConfigForWorkflowPath(workflowPath string, cfg *Config) error { //nolint:gocognit,funlen // baseline (#521)
	var err error
	if envName, ok := explicitEnvReferenceName(cfg.Tracker.APIKey); ok {
		cfg.Tracker.apiKeyEnvVar = envName
	} else {
		cfg.Tracker.apiKeyEnvVar = ""
	}
	if cfg.Tracker.APIKey, err = resolveExplicitEnv("tracker.api_key", cfg.Tracker.APIKey); err != nil {
		return err
	}
	if cfg.Tracker.Endpoint, err = resolveExplicitEnv("tracker.endpoint", cfg.Tracker.Endpoint); err != nil {
		return err
	}
	if err := expandRepoConfig("repo.clone_url", &cfg.Repo); err != nil {
		return err
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
	if cfg.Polling.IntervalMs <= 0 {
		cfg.Polling.IntervalMs = cfg.Tracker.PollIntervalMs
	}
	if cfg.Polling.IntervalMs <= 0 {
		cfg.Polling.IntervalMs = 30000
	}
	cfg.Tracker.PollIntervalMs = cfg.Polling.IntervalMs
	if cfg.Sandbox.Backend == "" {
		cfg.Sandbox.Backend = "none"
	}
	if cfg.Sandbox.NetworkMode == "" {
		cfg.Sandbox.NetworkMode = "none"
	}
	if cfg.Codex.ThreadSandbox == "" {
		cfg.Codex.ThreadSandbox = "workspace-write"
	}
	// Derive the per-turn sandbox policy from thread_sandbox unless the
	// operator set codex.turn_sandbox_policy explicitly. ThreadSandbox is
	// defaulted just above, so this sees the resolved thread sandbox. Deriving
	// here (rather than pinning workspace-write in DefaultConfig) keeps
	// thread_sandbox as the single knob governing effective turn permissions
	// (#472; DEVIATIONS D32).
	if shouldDeriveTurnSandboxPolicy(cfg.Codex) {
		cfg.Codex.TurnSandboxPolicy = defaultTurnSandboxPolicyForThread(cfg.Codex.ThreadSandbox)
	}
	if cfg.Codex.ApprovalPolicy == nil {
		// Default mirrors codex's `granular` policy with every flag set to
		// false, i.e. "auto-reject every approval / elicitation prompt".
		// Codex renamed the variant from `reject` → `granular` and flipped
		// the field polarity (true = allow) in
		// codex commit b7dba72db (#14516); aiops-platform had been sending
		// the obsolete `reject:` payload, which made every thread/start
		// return `-32600 unknown variant ` (#329).
		cfg.Codex.ApprovalPolicy = map[string]any{"granular": map[string]any{
			"sandbox_approval":    false,
			"rules":               false,
			"skill_approval":      false,
			"request_permissions": false,
			"mcp_elicitations":    false,
		}}
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
