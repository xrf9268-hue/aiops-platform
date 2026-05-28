package worker

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
	"github.com/xrf9268-hue/aiops-platform/internal/workspace"
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
	Provenance     configProvenance      `json:"provenance"`
	PromptTemplate promptSummary         `json:"prompt_template"`
}

// Provenance source tokens (#375). Every effective value annotated by
// --print-config carries exactly one of these, naming the resolution
// layer that won:
//   - sourceDefault:  the built-in schema default (no override took effect)
//   - sourceEnv:      a worker environment variable (the field's EnvVar
//     names which one; EnvVarDeprecated flags a legacy
//     unprefixed alias per #368)
//   - sourceWorkflow: an explicit value in WORKFLOW.md front matter
//   - sourceCLI:      a command-line override (currently only --port)
const (
	sourceDefault  = "default"
	sourceEnv      = "env"
	sourceWorkflow = "workflow"
	sourceCLI      = "cli"
)

// configProvenance annotates the effective value and origin of the
// config fields whose value can come from more than one resolution layer
// (env / WORKFLOW.md / CLI / default). It is a debug aid layered on top of
// the masked Config block, which alone cannot show that e.g. the effective
// workspace root came from AIOPS_WORKSPACE_ROOT rather than the file.
//
// None of these fields is secret-bearing (filesystem paths, a port, a
// workflow path), so they are reported verbatim; the maskSecrets contract
// for Config is unchanged (#375).
type configProvenance struct {
	WorkspaceRoot fieldProvenance `json:"workspace_root"`
	MirrorRoot    fieldProvenance `json:"mirror_root"`
	ServerPort    fieldProvenance `json:"server_port"`
	WorkflowPath  fieldProvenance `json:"workflow_path"`
}

// fieldProvenance is the effective value of one field plus where it came
// from. Value is rendered as a string for a uniform shape across a path, a
// port, and a workflow path. EnvVar/EnvVarDeprecated are populated only
// when Source == sourceEnv.
type fieldProvenance struct {
	Value            string `json:"value"`
	Source           string `json:"source"`
	EnvVar           string `json:"env_var,omitempty"`
	EnvVarDeprecated bool   `json:"env_var_deprecated,omitempty"`
}

// resolveProvenance derives the per-field provenance for --print-config.
// workerCfg carries the env-layer values (LoadConfigFromEnv); wcfg is the
// resolved workflow config; res is how the workflow file itself resolved;
// portOverride is the CLI --port flag (nil when not passed).
func resolveProvenance(workerCfg Config, wcfg workflow.Config, res workflow.Resolution, portOverride *int) configProvenance {
	return configProvenance{
		WorkspaceRoot: workspaceRootProvenance(workerCfg, wcfg),
		MirrorRoot:    mirrorRootProvenance(),
		ServerPort:    serverPortProvenance(wcfg, portOverride),
		WorkflowPath:  workflowPathProvenance(res),
	}
}

// workspaceRootProvenance mirrors EffectiveWorkspaceRoot's SPEC §6.4
// precedence (workflow > env > default) and labels the winning layer. The
// effective Value always comes from EffectiveWorkspaceRoot so the reported
// value cannot drift from the value the worker actually uses.
func workspaceRootProvenance(workerCfg Config, wcfg workflow.Config) fieldProvenance {
	effective := EffectiveWorkspaceRoot(workerCfg, wcfg)
	if wcfg.Workspace.RootSet() && strings.TrimSpace(wcfg.Workspace.Root) != "" {
		return fieldProvenance{Value: effective, Source: sourceWorkflow}
	}
	envRes := ResolveEnv(workspaceRootEnv, workspaceRootEnvLegacy)
	if strings.TrimSpace(envRes.Value) != "" {
		return envProvenance(effective, envRes)
	}
	return fieldProvenance{Value: effective, Source: sourceDefault}
}

// mirrorRootProvenance reports where the bare-mirror cache root resolves
// from. There is no WORKFLOW.md field for it, so the only layers are the
// AIOPS_MIRROR_ROOT env (with its legacy alias) and the computed default.
func mirrorRootProvenance() fieldProvenance {
	envRes := ResolveEnv(mirrorRootEnv, mirrorRootEnvLegacy)
	if strings.TrimSpace(envRes.Value) != "" {
		return envProvenance(workspace.MirrorRoot(envRes.Value), envRes)
	}
	return fieldProvenance{Value: workspace.MirrorRoot(""), Source: sourceDefault}
}

// serverPortProvenance mirrors desiredPortForLoop's precedence: the CLI
// --port override wins over the workflow snapshot, which wins over the
// DefaultConfig port. PortSet() distinguishes a WORKFLOW.md value from the
// inherited default.
func serverPortProvenance(wcfg workflow.Config, portOverride *int) fieldProvenance {
	if portOverride != nil {
		return fieldProvenance{Value: strconv.Itoa(*portOverride), Source: sourceCLI}
	}
	if wcfg.Server.PortSet() {
		return fieldProvenance{Value: strconv.Itoa(wcfg.Server.Port), Source: sourceWorkflow}
	}
	return fieldProvenance{Value: strconv.Itoa(wcfg.Server.Port), Source: sourceDefault}
}

// workflowPathProvenance reports whether the effective config came from a
// resolved WORKFLOW.md (sourceWorkflow, with the repo-relative path) or
// from the built-in defaults (sourceDefault, no path). The file-vs-prompt
// distinction is preserved by the top-level `source`/`resolution` fields.
func workflowPathProvenance(res workflow.Resolution) fieldProvenance {
	switch res.Source {
	case workflow.SourceFile, workflow.SourcePromptOnly:
		return fieldProvenance{Value: res.Path, Source: sourceWorkflow}
	default:
		return fieldProvenance{Value: "", Source: sourceDefault}
	}
}

// envProvenance builds an env-sourced fieldProvenance, flagging the used
// name as deprecated when it is a legacy alias rather than the canonical
// AIOPS_-prefixed name (#368).
func envProvenance(value string, res EnvResolution) fieldProvenance {
	return fieldProvenance{
		Value:            value,
		Source:           sourceEnv,
		EnvVar:           res.UsedName,
		EnvVarDeprecated: res.UsedName != res.Canonical,
	}
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
	Default               string `json:"default"`
	MaxConcurrentAgents   int    `json:"max_concurrent_agents"`
	MaxTurns              int    `json:"max_turns"`
	MaxRetryBackoffMs     int    `json:"max_retry_backoff_ms"`
	MaxRetryAttempts      *int   `json:"max_retry_attempts"`
	Timeout               string `json:"timeout"`
	MaxTimeoutRetries     *int   `json:"max_timeout_retries"`
	PolicyViolationBudget int    `json:"policy_violation_budget"`
}

func newConfigView(cfg workflow.Config) configView {
	return configView{
		Repo:      cfg.Repo,
		Tracker:   cfg.Tracker,
		Workspace: cfg.Workspace,
		Agent: agentConfigView{
			Default:               cfg.Agent.Default,
			MaxConcurrentAgents:   cfg.Agent.MaxConcurrentAgents,
			MaxTurns:              cfg.Agent.MaxTurns,
			MaxRetryBackoffMs:     cfg.Agent.MaxRetryBackoffMs,
			MaxRetryAttempts:      cfg.Agent.MaxRetryAttempts,
			Timeout:               cfg.Agent.Timeout.String(),
			MaxTimeoutRetries:     cfg.Agent.MaxTimeoutRetries,
			PolicyViolationBudget: cfg.Agent.PolicyViolationBudgetValue(),
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
// validation error). portOverride carries the CLI --port flag (nil when
// not passed) so the provenance block can attribute server.port to `cli`.
// Used both by main()'s --print-config dispatch and by tests;
// stdout/stderr are explicit io.Writer parameters so tests can capture the
// output without subprocessing.
func printConfig(workdir string, portOverride *int, stdout, stderr io.Writer) int {
	wf, res, err := workflow.Resolve(workdir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err.Error())
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
		Provenance:     resolveProvenance(LoadConfigFromEnv(), wf.Config, *res, portOverride),
		PromptTemplate: summarizePrompt(wf.PromptTemplate),
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		_, _ = fmt.Fprintln(stderr, err.Error())
		return 1
	}
	return 0
}
