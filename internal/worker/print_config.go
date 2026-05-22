package worker

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// printConfigOutput is the JSON shape of `worker --print-config <dir>`.
// Stable: external tooling may consume it.
//
// Source and ShadowedBy are top-level convenience copies of
// Resolution.Source and Resolution.ShadowedBy: SPEC treats WORKFLOW.md as a
// single-source file, and operators answering "which file is in effect, and
// is anything being shadowed?" should not need to drill into a nested
// resolution object. The nested Resolution is preserved as the
// authoritative shape; the top-level fields are a stable shortcut.
type printConfigOutput struct {
	Source         string                `json:"source"`
	ShadowedBy     []string              `json:"shadowed_by,omitempty"`
	Resolution     printConfigResolution `json:"resolution"`
	Config         configView            `json:"config"`
	PromptTemplate promptSummary         `json:"prompt_template"`
}

// configView mirrors workflow.Config for --print-config output, but
// substitutes typed-nanosecond fields with human-readable forms. It is
// deliberately local to this debug command: keeping the conversion here
// preserves the in-process workflow.Config representation (a typed
// time.Duration) while ensuring the published JSON contract carries the
// duration as a string that round-trips through time.ParseDuration.
//
// See issue #53 for the rationale and the rejected alternative
// (a wrapper Duration type with custom MarshalJSON).
type configView struct {
	Repo      workflow.RepoConfig      `json:"repo"`
	Tracker   workflow.TrackerConfig   `json:"tracker"`
	Workspace workflow.WorkspaceConfig `json:"workspace"`
	Agent     agentConfigView          `json:"agent"`
	Codex     workflow.CommandConfig   `json:"codex"`
	Claude    workflow.CommandConfig   `json:"claude"`
	Policy    workflow.PolicyConfig    `json:"policy"`
	Verify    workflow.VerifyConfig    `json:"verify"`
	PR        workflow.PRConfig        `json:"pr"`
	Sandbox   workflow.SandboxConfig   `json:"sandbox"`
}

// agentConfigView mirrors workflow.AgentConfig but renders Timeout as a
// duration string (e.g. "30m0s") rather than a raw nanosecond integer.
// The remaining fields keep their original JSON tags so external
// consumers see the same shape.
type agentConfigView struct {
	Default             string `json:"default"`
	MaxConcurrentAgents int    `json:"max_concurrent_agents"`
	MaxTurns            int    `json:"max_turns"`
	MaxRetryBackoffMs   int    `json:"max_retry_backoff_ms"`
	MaxRetryAttempts    *int   `json:"max_retry_attempts"`
	Timeout             string `json:"timeout"`
	MaxTimeoutRetries   *int   `json:"max_timeout_retries"`
}

func newConfigView(cfg workflow.Config) configView {
	return configView{
		Repo:      cfg.Repo,
		Tracker:   cfg.Tracker,
		Workspace: cfg.Workspace,
		Agent: agentConfigView{
			Default:             cfg.Agent.Default,
			MaxConcurrentAgents: cfg.Agent.MaxConcurrentAgents,
			MaxTurns:            cfg.Agent.MaxTurns,
			MaxRetryBackoffMs:   cfg.Agent.MaxRetryBackoffMs,
			MaxRetryAttempts:    cfg.Agent.MaxRetryAttempts,
			Timeout:             cfg.Agent.Timeout.String(),
			MaxTimeoutRetries:   cfg.Agent.MaxTimeoutRetries,
		},
		Codex:   cfg.Codex,
		Claude:  cfg.Claude,
		Policy:  cfg.Policy,
		Verify:  cfg.Verify,
		PR:      cfg.PR,
		Sandbox: cfg.Sandbox,
	}
}

type printConfigResolution struct {
	Source     string   `json:"source"`
	Path       string   `json:"path,omitempty"`
	ShadowedBy []string `json:"shadowed_by,omitempty"`
}

// promptSummary is intentionally not the full prompt body. See spec
// section "Why prompt body is summarized, not printed" for the rationale.
type promptSummary struct {
	Length    int    `json:"length"`
	FirstLine string `json:"first_line"`
}

const promptFirstLineMaxBytes = 200

// summarizePrompt produces the bounded prompt summary published by
// --print-config. Length is the byte length of the trimmed body so a
// reader can sanity-check completeness; FirstLine is truncated to keep
// debug output cheap to paste even when an author writes a long single
// line. The full body is never echoed (see spec safety contract).
func summarizePrompt(body string) promptSummary {
	trimmed := strings.TrimSpace(body)
	first := trimmed
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	if len(first) > promptFirstLineMaxBytes {
		first = first[:promptFirstLineMaxBytes]
	}
	return promptSummary{
		Length:    len(trimmed),
		FirstLine: first,
	}
}

const maskedSecret = "***"

// maskSecrets rewrites secret-bearing fields on a Config to a fixed
// placeholder before serialization. The function takes its argument by
// value; the workflow.Config used by the running worker is never
// touched.
//
// Policy contract: this function is the single point that enumerates
// every secret-bearing field on workflow.Config. Adding a new
// secret-bearing field to the schema without extending this function is
// a review-blocking gap — tests in print_config_test.go pin each known
// field against plaintext leaks. Current coverage:
//   - Tracker.APIKey            (token)
//   - Repo.CloneURL             (may embed https://user:token@... userinfo)
//   - Sandbox.CredentialFiles   (each path is itself an attacker pointer)
func maskSecrets(cfg workflow.Config) workflow.Config {
	if cfg.Tracker.APIKey != "" {
		cfg.Tracker.APIKey = maskedSecret
	}
	cfg.Repo.CloneURL = maskCloneURL(cfg.Repo.CloneURL)
	if n := len(cfg.Sandbox.CredentialFiles); n > 0 {
		masked := make([]string, n)
		for i := range masked {
			masked[i] = maskedSecret
		}
		cfg.Sandbox.CredentialFiles = masked
	}
	return cfg
}

// maskCloneURL strips embedded basic-auth from a clone URL. URLs that
// do not parse as net/url (notably the SSH-style git@host:path form)
// return unchanged, since that form does not carry userinfo. A bare
// username with no password is also stripped: by convention an embedded
// user in an HTTPS clone URL is a token alias (e.g. "oauth2",
// "x-access-token", or the token itself), not a real account name.
func maskCloneURL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

// printConfig writes the effective workflow for workdir as JSON to
// stdout. Returns the process exit code (0 on success, 1 on schema
// validation error). Used both by main()'s --print-config dispatch and
// by tests; stdout/stderr are explicit io.Writer parameters so tests
// can capture the output without subprocessing.
func printConfig(workdir string, stdout, stderr io.Writer) int {
	wf, res, err := workflow.Resolve(workdir)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	out := printConfigOutput{
		Source:     string(res.Source),
		ShadowedBy: res.ShadowedBy,
		Resolution: printConfigResolution{
			Source:     string(res.Source),
			Path:       res.Path,
			ShadowedBy: res.ShadowedBy,
		},
		Config:         newConfigView(maskSecrets(wf.Config)),
		PromptTemplate: summarizePrompt(wf.PromptTemplate),
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	return 0
}
