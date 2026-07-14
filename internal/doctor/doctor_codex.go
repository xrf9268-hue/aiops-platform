package doctor

// doctor_codex.go holds the Codex agent subsystem checks: CLI presence and
// version-pin drift, auth mode (API key vs ChatGPT login) and model config,
// the codex sandbox self-test, and the app-server JSON-RPC launch probe, plus
// the codex-specific config/version helpers they share. The report framework
// (reportBuilder, run helpers, shared output formatting) lives in doctor.go.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func (r *reportBuilder) checkCodex(ctx context.Context, cfg workflow.Config) { //nolint:gocognit // baseline (#521)
	if !requiresCodex(cfg) {
		r.pass("Codex", "not required for mock runner")
		return
	}
	if usesDefaultCodexCLI(cfg) {
		if _, err := exec.LookPath("codex"); err != nil {
			r.fail("Codex CLI", "codex binary not found on PATH", "Install Codex CLI in this host/container or set codex.command to a runnable app-server wrapper.")
			return
		}
		if out, err := r.run(ctx, "codex", []string{"--version"}); err != nil {
			r.fail("Codex version", trimOutput(out, err), "Fix the Codex installation before dispatching real agents.")
		} else {
			installed := strings.TrimSpace(string(out))
			got, gotOK := codexVersionFromOutput(installed)
			want, _ := parseGoVersion(runner.CodexProtocolVersion)
			if gotOK && codexVersionMatches(got, want) {
				r.pass("Codex version", fmt.Sprintf("%s (matches pinned %s)", installed, runner.CodexProtocolVersion))
			} else {
				status := Warn
				if r.realMode() {
					status = Fail
				}
				r.add(status, "Codex version",
					fmt.Sprintf("%s (pinned schema is %s)", installed, runner.CodexProtocolVersion),
					fmt.Sprintf("The vendored app-server schema is generated for codex %s exactly; any other version — older or newer — can add or rename required fields the contract test cannot see. Regenerate via scripts/refresh-codex-schema.sh after a deliberate upgrade, or align the installed codex.", runner.CodexProtocolVersion))
			}
		}
		if out, err := r.run(ctx, "codex", []string{"login", "status"}); err != nil {
			r.fail("Codex auth", trimOutput(out, err), "Run codex --login in the same CODEX_HOME/container user context.")
		} else {
			r.pass("Codex auth", firstLine(out))
		}
	} else {
		r.warn("Codex CLI", "version/auth checks skipped for custom codex.command", "Ensure the wrapper uses an authenticated Codex context; the app-server probe validates launch.")
	}
	if cfg.Agent.Default != runner.NameCodexAppServer {
		return
	}
	if err := r.probeCodexAppServer(ctx, cfg); err != nil {
		r.fail("Codex app-server", err.Error(), "Check CODEX_HOME, codex.command, and app-server support in the installed Codex version.")
	} else {
		r.pass("Codex app-server", "started and answered a JSON-RPC probe")
	}
	r.checkCodexAuthModel(cfg)
}

// checkCodexAuthModel reports the production auth mode and model configuration
// the app-server worker will use, so a preflight distinguishes a missing model
// API key from an expired ChatGPT login and surfaces model selection instead of
// leaving it hidden in a copied host config.toml (#465). Secrets are never
// printed: API-key presence is checked without echoing the value, and only the
// non-secret model/provider/effort keys are read from config.toml.
func (r *reportBuilder) checkCodexAuthModel(cfg workflow.Config) {
	home := codexHomeDir()
	if codexAPIKeyAuthSelected(cfg) {
		r.checkCodexAPIKeyAuth()
	} else {
		r.checkCodexLoginAuth(home)
	}
	r.checkCodexModelConfig(home)
}

func (r *reportBuilder) checkCodexAPIKeyAuth() {
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" {
		r.pass("Codex auth mode", "API key via OPENAI_API_KEY passthrough")
		return
	}
	status := Warn
	if r.realMode() {
		status = Fail
	}
	r.add(status, "Codex auth mode", "OPENAI_API_KEY is in codex.env_passthrough but resolved empty",
		"Provide the model API key in OPENAI_API_KEY via the worker environment (a Docker secret or systemd EnvironmentFile); never pass it on a command line.")
}

func (r *reportBuilder) checkCodexLoginAuth(home string) {
	detail := "ChatGPT/Codex login"
	if home != "" {
		detail += " (auth.json under " + home + ")"
	}
	r.pass("Codex auth mode", detail)
	if !r.realMode() || home == "" {
		return
	}
	if err := codexHomeWritable(home); err != nil {
		r.warn("Codex auth refresh", "CODEX_HOME is not writable: "+err.Error(),
			"Mount CODEX_HOME as a restricted writable volume so ChatGPT-login token refresh can persist for long-lived workers.")
	}
}

func (r *reportBuilder) checkCodexModelConfig(home string) {
	if home == "" {
		return
	}
	configPath := filepath.Join(home, "config.toml")
	model, provider, effort, ok := readCodexModelConfig(configPath)
	if !ok {
		r.warn("Codex model config", "no model declared in "+configPath,
			"Declare model (and optional model_provider/model_reasoning_effort) in a tracked, non-secret config.toml so model selection is auditable instead of relying on a copied host config.")
		return
	}
	detail := "model=" + model
	if provider != "" {
		detail += ", provider=" + provider
	}
	if effort != "" {
		detail += ", reasoning_effort=" + effort
	}
	r.pass("Codex model config", detail)
}

// effectiveSandboxMode returns the policy the app-server actually applies per
// turn: the explicit codex.turn_sandbox_policy when set (it overrides
// thread_sandbox in startTurn), else the thread_sandbox-derived default. Gating
// must follow the effective policy — e.g. a turn_sandbox_policy override to
// dangerFullAccess or externalSandbox (with thread_sandbox left at
// workspace-write) runs agent commands WITHOUT codex's own userns sandbox, so
// probing would be a false FAIL (codex review #549).
func effectiveSandboxMode(cfg workflow.Config) string {
	switch cfg.Codex.TurnSandboxPolicy.Type {
	case workflow.CodexSandboxDangerFullAccess:
		return "danger-full-access"
	case workflow.CodexSandboxExternalSandbox:
		return "external"
	case workflow.CodexSandboxReadOnly:
		return "read-only"
	case workflow.CodexSandboxWorkspaceWrite:
		return "workspace-write"
	}
	switch cfg.Codex.ThreadSandbox {
	case "danger-full-access":
		return "danger-full-access"
	case "read-only":
		return "read-only"
	default:
		return "workspace-write"
	}
}

// codexUsesUserNamespaceSandbox reports whether the effective mode makes codex
// run agent commands in its own bwrap user-namespace sandbox (read-only or
// workspace-write). danger-full-access (no sandbox) and external (host-provided
// isolation; codex skips its own) do not, so the userns probe is moot for them.
func codexUsesUserNamespaceSandbox(mode string) bool {
	return mode == "read-only" || mode == "workspace-write"
}

func (r *reportBuilder) checkSandbox(ctx context.Context, cfg workflow.Config) {
	if !requiresCodex(cfg) {
		return
	}
	mode := effectiveSandboxMode(cfg)
	if runningInContainerFn() && codexUsesUserNamespaceSandbox(mode) {
		r.warn("Codex sandbox", "containerized Codex may not support workspace-write namespaces", "Use the documented Docker-isolated profile or enable the required kernel/userns support.")
		return
	}
	if !codexUsesUserNamespaceSandbox(mode) {
		r.pass("Codex sandbox", "effective sandbox ("+mode+") runs agent commands without a user-namespace sandbox")
		return
	}
	// The live probe is authoritative only for a real-mode binary deploy on
	// Linux: codex sandboxes agent shell commands with a bwrap user namespace
	// there, so a host that blocks unprivileged user namespaces is exactly what
	// the agent will hit. On macOS codex uses the seatbelt sandbox (no user
	// namespaces), under --deploy=docker the sandbox runs inside the container
	// (a different userns policy than this host), and mock mode dispatches
	// nothing — keep the static classification in those cases.
	if !r.realMode() || !r.binaryDeploy() || runtime.GOOS != "linux" {
		r.pass("Codex sandbox", "selected profile is compatible with this preflight")
		return
	}
	r.probeCodexSandbox(ctx, cfg)
}

// userNamespaceRemediation explains how to restore the host capability codex's
// bwrap sandbox needs. The safer options lead; the host-wide sysctl is last
// because it weakens unprivileged user namespaces for every process. Phrased
// "most commonly" because the probe runs codex's real sandbox and surfaces its
// actual error, which is authoritative even if the cause is not the AppArmor
// userns restriction.
const userNamespaceRemediation = "codex sandboxes agent shell commands in a bubblewrap (bwrap) user namespace, which this host most commonly blocks because unprivileged user namespaces are restricted (Ubuntu 24.04+: kernel.apparmor_restrict_unprivileged_userns=1) — see the codex error above for the exact failure. Prefer: install an AppArmor profile that permits codex's bwrap user namespaces, or run the worker in a container that allows them, or set codex.thread_sandbox: danger-full-access only in an already-isolated environment. Last resort (weakens host security — re-enables unprivileged user namespaces process-wide): `sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`, persisted via /etc/sysctl.d/99-userns.conf."

// probeCodexSandbox runs a trivial command through codex's OWN sandbox
// (`codex sandbox -- /bin/true`) so a host where codex cannot start a sandboxed
// command fails preflight instead of after a full agent turn is dispatched and
// tokens burned. The static check passes even when
// kernel.apparmor_restrict_unprivileged_userns=1 blocks codex's bwrap, so every
// agent command then dies at runtime (e.g. "bwrap: setting up uid map:
// Permission denied") (#542).
//
// This drives codex's vendored bwrap with codex's exact flags — not a
// hand-rolled system-bwrap proxy — so it is authoritative: it reproduces the
// real failure mode (uid-map, netns, or an AppArmor profile that covers a
// different binary path), which a system-bwrap proxy can miss (codex review
// #549). codex 0.135's `sandbox` subcommand does not forward the root
// `--sandbox` option, so the probe takes no mode argument; its default
// sandboxed mode uses the same bwrap user namespace every sandboxed profile
// needs, and checkSandbox has already skipped the only unsandboxed profile
// (effective danger-full-access) before reaching here (codex review #549).
// /bin/true needs no model turn, auth, or network. Routed through r.run so the
// probe is unit-testable. A custom codex.command can't be assumed to support
// `codex sandbox`, so it is WARN.
func (r *reportBuilder) probeCodexSandbox(ctx context.Context, cfg workflow.Config) {
	if !usesDefaultCodexCLI(cfg) {
		r.warn("Codex sandbox", "custom codex.command: skipped the codex sandbox self-test",
			"Verify the wrapper sandboxes agent commands, e.g. `codex sandbox -- /bin/true`.")
		return
	}
	out, err := r.run(ctx, "codex", []string{"sandbox", "--", "/bin/true"})
	detail := trimOutput(out, err)
	switch {
	case err == nil:
		r.pass("Codex sandbox", "codex sandbox started a command on this host")
	case errors.Is(err, exec.ErrNotFound):
		r.warn("Codex sandbox", "codex not found on PATH; cannot run the codex sandbox self-test",
			"Install Codex CLI on this host, then rerun worker --doctor --deploy=binary --mode=real.")
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		// A slow codex cold-start (not a sandbox denial) should not block
		// preflight with a misleading userns remediation.
		r.warn("Codex sandbox", "codex sandbox self-test timed out before completing; could not determine sandbox health",
			"Re-run worker --doctor; if it persists, check codex startup time on this host.")
	case codexLacksSandboxSubcommand(detail):
		// An older/forked codex without `codex sandbox` is a version problem,
		// not a host userns problem — warn to upgrade rather than FAIL.
		r.warn("Codex sandbox", "this codex build does not support `codex sandbox`; cannot self-test the sandbox: "+detail,
			"Upgrade Codex CLI to a build with the `sandbox` subcommand, or verify the sandbox manually: codex sandbox -- /bin/true.")
	default:
		r.fail("Codex sandbox", "codex sandbox cannot start commands: "+detail, userNamespaceRemediation)
	}
}

// codexLacksSandboxSubcommand reports whether codex's output is a CLI usage
// error for an unrecognized `sandbox` subcommand (an older/forked codex) rather
// than a real sandbox-startup failure, so a version mismatch warns (upgrade
// codex) instead of FAILing with the userns remediation.
func codexLacksSandboxSubcommand(out string) bool {
	out = strings.ToLower(out)
	return strings.Contains(out, "unrecognized subcommand") ||
		strings.Contains(out, "no such subcommand") ||
		strings.Contains(out, "unknown command") ||
		(strings.Contains(out, "unexpected argument") && strings.Contains(out, "sandbox"))
}

func (r *reportBuilder) probeCodexAppServer(ctx context.Context, cfg workflow.Config) error { //nolint:gocognit // baseline (#521)
	probe := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"aiops-doctor","title":"aiops doctor","version":"0.1.0"}}}` + "\n"
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	env := os.Environ()
	cmd, _, err := runner.NewCodexAppServerCommand(probeCtx, cfg, env)
	if err != nil {
		return err
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		terminate(cmd)
		_ = cmd.Wait()
	}()
	if _, err := io.WriteString(stdin, probe); err != nil {
		return err
	}
	defer closeBody(stdin)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		ok, err := validateAppServerProbeLine(sc.Bytes())
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if probeCtx.Err() != nil {
		return probeCtx.Err()
	}
	return fmt.Errorf("no JSON-RPC response: %s", strings.TrimSpace(stderr.String()))
}

func validateAppServerProbeLine(line []byte) (bool, error) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return false, nil
	}
	if strings.TrimSpace(string(msg.ID)) != "1" {
		return false, nil
	}
	if len(msg.Error) > 0 && strings.TrimSpace(string(msg.Error)) != "null" {
		return false, fmt.Errorf("app-server initialize error: %s", strings.TrimSpace(string(msg.Error)))
	}
	if len(msg.Result) == 0 || strings.TrimSpace(string(msg.Result)) == "null" {
		return false, fmt.Errorf("unexpected app-server response: %s", strings.TrimSpace(string(line)))
	}
	return true, nil
}

func terminate(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func requiresCodex(cfg workflow.Config) bool {
	return cfg.Agent.Default == runner.NameCodexAppServer
}

// codexAPIKeyAuthSelected reports whether the workflow opts the agent into
// model API-key auth by passing OPENAI_API_KEY through to the Codex
// subprocess. Without the passthrough the key never reaches the agent and the
// app-server falls back to the ChatGPT/Codex login in CODEX_HOME.
func codexAPIKeyAuthSelected(cfg workflow.Config) bool {
	for _, name := range cfg.Codex.EnvPassthrough {
		if strings.EqualFold(strings.TrimSpace(name), "OPENAI_API_KEY") {
			return true
		}
	}
	return false
}

// codexHomeDir resolves the directory Codex reads auth and config from, the
// same way the CLI does: CODEX_HOME wins, otherwise $HOME/.codex.
func codexHomeDir() string {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		return home
	}
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		return filepath.Join(home, ".codex")
	}
	return ""
}

// codexHomeWritable confirms the worker can create files in CODEX_HOME, which
// a long-lived ChatGPT-login deployment needs so Codex can persist refreshed
// tokens. The probe file is removed before returning.
func codexHomeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".aiops-doctor-write-*")
	if err != nil {
		return err
	}
	name := f.Name()
	closeBody(f)
	_ = os.Remove(name)
	return nil
}

// readCodexModelConfig extracts the non-secret top-level model selection keys
// from a Codex config.toml. It deliberately ignores every `[table]` section so
// provider blocks (which may name secret env keys) are never surfaced. ok is
// true only when a model is declared.
func readCodexModelConfig(path string) (model, provider, effort string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", false
	}
	values := topLevelTOMLScalars(string(data))
	model = values["model"]
	return model, values["model_provider"], values["model_reasoning_effort"], model != ""
}

// topLevelTOMLScalars returns the bare top-level `key = value` scalars in a
// TOML document, skipping every `[table]` section so nested provider blocks
// (which can name secret env keys) are never surfaced.
func topLevelTOMLScalars(data string) map[string]string {
	values := map[string]string{}
	inTable := false
	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "["):
			inTable = true
		case inTable:
		default:
			if key, val, found := strings.Cut(line, "="); found {
				values[strings.TrimSpace(key)] = tomlScalarString(val)
			}
		}
	}
	return values
}

// tomlScalarString reads a quoted or bare TOML scalar, dropping a trailing
// inline comment on bare values. It is intentionally minimal: doctor only
// surfaces the value for display, so full TOML parsing is not warranted.
func tomlScalarString(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 0 && (v[0] == '"' || v[0] == '\'') {
		if j := strings.IndexByte(v[1:], v[0]); j >= 0 {
			return v[1 : 1+j]
		}
		return strings.Trim(v, string(v[0]))
	}
	if i := strings.IndexByte(v, '#'); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

func usesDefaultCodexCLI(cfg workflow.Config) bool {
	command := strings.TrimSpace(cfg.Codex.Command)
	return command == "" || command == "codex app-server" || strings.HasPrefix(command, "codex app-server ")
}

// codexVersionFromOutput extracts the first exact `major.minor.patch` token from
// `codex --version` output (e.g. "codex-cli 0.142.0"), reusing parseGoVersion's
// generic numeric parse.
func codexVersionFromOutput(output string) (goVersion, bool) {
	for _, field := range strings.Fields(output) {
		normalized := strings.TrimPrefix(field, "v")
		v, ok := parseGoVersion(normalized)
		if ok && v.patchSet && normalized == fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch) {
			return v, true
		}
	}
	return goVersion{}, false
}

// codexVersionMatches reports whether the installed codex version equals the
// pinned one. The vendored app-server schema is generated for exactly
// CodexProtocolVersion, so any other version — older OR newer — can drift: a
// newer release can add required fields or rename enums just as an older one
// lacks current ones, and the contract test (pinned to the vendored schema)
// cannot see it. Anything but an exact match fails real-mode preflight.
func codexVersionMatches(got, want goVersion) bool {
	return got.major == want.major && got.minor == want.minor && got.patch == want.patch
}

func firstLine(out []byte) string {
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if line == "" {
		return "ok"
	}
	return line
}
