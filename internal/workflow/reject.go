package workflow

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// rejectRemovedFields surfaces a clear error for keys that were once part of
// the schema but have been removed. The typed Unmarshal in Load silently drops
// unknown fields, which would let workflow authors keep believing the key still
// controls behavior. Targeted detection keeps existing benign extras working
// while flagging known footguns. Each helper covers one removed surface so the
// dispatcher stays a flat list of calls (gocognit).
func rejectRemovedFields(front []byte) error {
	var raw map[string]any
	if err := yaml.Unmarshal(front, &raw); err != nil {
		return nil
	}
	if err := rejectRemovedCodexFields(raw); err != nil {
		return err
	}
	if err := rejectRemovedVerifyFields(raw); err != nil {
		return err
	}
	if err := rejectRemovedPolicyFields(raw); err != nil {
		return err
	}
	return rejectRemovedMiscFields(raw)
}

// rejectRemovedMiscFields surfaces clear errors for removed keys that do not
// have their own dedicated helper: the nested `claude.profile` / `agent.fallback`
// keys removed earlier, and the top-level `pr:` / `safety:` blocks removed under
// #578. Collecting them here keeps rejectRemovedFields a flat list of helper
// calls (gocognit). Like the verify.*/policy.*/codex.* rejects, each fails loud
// instead of being silently dropped. The `pr:`/`safety:` blocks configured
// capabilities that no longer exist — the worker does not open/label PRs (PR
// handoff is agent-side per SPEC §1, #76), and `safety:` was a descriptive
// struct the worker never enforced — so their intent belongs in the WORKFLOW
// prompt (SPEC §3.2), not front matter.
func rejectRemovedMiscFields(raw map[string]any) error {
	if claude, ok := raw["claude"].(map[string]any); ok {
		if _, present := claude["profile"]; present {
			return fmt.Errorf("claude.profile is not supported; the codex-only `profile` field was removed in issue #541 and Claude never had runner profiles. Remove it")
		}
	}
	// pr:/safety: are top-level keys, so they must be checked regardless of
	// whether an `agent:` block is present (rejectRemovedAgentFields below
	// returns early when it is not).
	if _, present := raw["pr"]; present {
		return fmt.Errorf("pr.* is no longer supported (#578): the worker does not open, label, or assign reviewers to PRs — PR creation is the coding agent's responsibility (SPEC §1, #76). Express draft/labels/reviewers in the agent's WORKFLOW prompt or tool surface, and remove the `pr:` block")
	}
	if _, present := raw["safety"]; present {
		return fmt.Errorf("safety.* is no longer supported (#578): it was a descriptive struct the worker never enforced, which falsely implied enforcement. Express the safety envelope in the WORKFLOW prompt/README, and use `sandbox:` for worker-enforced write/network restrictions. Remove the `safety:` block")
	}
	return rejectRemovedAgentFields(raw)
}

// rejectRemovedAgentFields surfaces clear errors for removed agent.* keys so a
// stale workflow fails loud instead of being silently dropped: `agent.fallback`
// (#40, never read) and the `agent.max_retry_attempts` / `agent.max_timeout_retries`
// retry caps removed under #577 (the orchestrator no longer caps retries). Split
// out of rejectRemovedFields to keep that function under the gocognit budget.
func rejectRemovedAgentFields(raw map[string]any) error {
	agent, ok := raw["agent"].(map[string]any)
	if !ok {
		return nil
	}
	if _, present := agent["fallback"]; present {
		return fmt.Errorf("agent.fallback is no longer supported (issue #40); the worker never read this field. Remove it and set agent.default to a more reliable runner if you need a different default")
	}
	for _, key := range []string{"max_retry_attempts", "max_timeout_retries"} {
		if _, present := agent[key]; present {
			return fmt.Errorf("agent.%s is no longer supported (#577): the orchestrator no longer caps retries. SPEC §8.4 / §16.6 retry unboundedly with exponential backoff until the tracker takes the issue out of active work; to stop a persistently-failing issue, move it out of the active states. Remove agent.%s", key, key)
		}
	}
	return nil
}

// rejectRemovedVerifyFields surfaces clear errors for the verify.* keys that
// configured removed worker gates so a stale workflow fails loud instead of
// silently dropping a control the operator believes is active:
//
//   - timeout / allow_failure / env_passthrough configured the removed worker
//     verify gate (#557 / DEVIATIONS D33). Verification is the coding agent's
//     job now — verify.commands are surfaced to the agent's prompt and the
//     worker no longer runs them.
//   - secret_scan configured the removed worker secret-scan gate (#561). It
//     ran after the agent had already pushed, so it could only flag, never
//     prevent, a leak, and it raced reconcile-cancel the same way the verify
//     gate did (#557). PR CI / the host's secret scanning is the backstop now.
//
// verify.commands remains valid and is not rejected.
func rejectRemovedVerifyFields(raw map[string]any) error {
	verify, ok := raw["verify"].(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"timeout", "allow_failure", "env_passthrough"} {
		if _, present := verify[key]; present {
			return fmt.Errorf("verify.%s is no longer supported (#557): the worker verify gate was removed, so verification is the agent's responsibility — verify.commands are surfaced to the prompt and the worker no longer runs them. Remove verify.%s and express the check in the WORKFLOW prompt instead", key, key)
		}
	}
	if _, present := verify["secret_scan"]; present {
		return fmt.Errorf("verify.secret_scan is no longer supported (#561): the worker secret-scan gate was removed — it ran after the agent had already pushed, so it could only flag, never prevent, a leak, and it raced reconcile-cancel. Remove verify.secret_scan and rely on PR CI / your host's secret scanning, or run the scan from the WORKFLOW prompt before the agent pushes")
	}
	return nil
}

// rejectRemovedPolicyFields surfaces clear errors for the policy path/diffstat
// gate keys removed under #561, so a stale workflow fails loud instead of
// silently dropping a scope/path control the operator believes is enforced. The
// worker policy gate ran after the agent had already pushed (could only flag,
// never prevent) and raced reconcile-cancel; SPEC §3.2 homes scope/validation
// rules in the WORKFLOW.md prompt and upstream has no such config. policy.mode
// remains valid (it selects draft_pr vs analysis_only).
func rejectRemovedPolicyFields(raw map[string]any) error {
	if policy, ok := raw["policy"].(map[string]any); ok {
		for _, key := range []string{"allow_paths", "deny_paths", "max_changed_files", "max_changed_lines", "max_changed_loc"} {
			if _, present := policy[key]; present {
				return fmt.Errorf("policy.%s is no longer supported (#561): the worker path/diffstat gate was removed — it ran after the agent had already pushed, so it could only flag, never prevent. Express scope/path rules in the WORKFLOW prompt (SPEC §3.2) and use sandbox write restrictions for hard path prevention. Remove policy.%s; policy.mode is still honored", key, key)
			}
		}
	}
	if agent, ok := raw["agent"].(map[string]any); ok {
		if _, present := agent["policy_violation_budget"]; present {
			return fmt.Errorf("agent.policy_violation_budget is no longer supported (#561): the worker policy gate and its violation-retry loop were removed. Remove agent.policy_violation_budget")
		}
	}
	return nil
}

// rejectRemovedCodexFields surfaces clear errors for codex.* keys that
// configured the removed one-shot `codex exec` runner (issue #541): the
// `codex.profile` field and a `codex.command` whose argv runs `codex exec`.
// Both would otherwise be silently dropped or fall through to a `sh -c
// "codex exec"` launch that the app-server runner cannot speak, failing the
// first real run with an opaque protocol/timeout error.
func rejectRemovedCodexFields(raw map[string]any) error {
	codex, ok := raw["codex"].(map[string]any)
	if !ok {
		return nil
	}
	if _, present := codex["profile"]; present {
		return fmt.Errorf("codex.profile is no longer supported (issue #541); the one-shot `codex exec` runner it configured was removed. The SPEC §10 runner is `codex app-server` (agent.default: codex-app-server); set its sandbox with codex.thread_sandbox / codex.turn_sandbox_policy instead")
	}
	if command, ok := codex["command"].(string); ok && commandRunsCodexExec(command) {
		return fmt.Errorf("codex.command %q runs the removed one-shot `codex exec` runner (issue #541); the SPEC §10 runner is `codex app-server` — set codex.command to `codex app-server`", command)
	}
	return nil
}

// commandRunsCodexExec reports whether a codex.command launches the removed
// one-shot `codex exec` runner (issue #541). It is a deliberately conservative
// load-time guard: quote characters are stripped before tokenizing so quoted
// spellings (e.g. `"codex" "exec"`) are caught too. The runner's
// splitAppServerCommand is the authoritative launch-time parser — this only
// exists to turn the otherwise opaque app-server JSON-RPC handshake failure into
// a clear config error at load. Over-rejecting a pathological quoted command is
// the safe direction (fail fast) versus silently launching the removed runner.
func commandRunsCodexExec(command string) bool {
	unquoted := strings.NewReplacer(`"`, "", `'`, "").Replace(command)
	fields := strings.Fields(unquoted)
	return len(fields) >= 2 && fields[0] == "codex" && fields[1] == "exec"
}

var knownTopLevelWorkflowKeys = map[string]struct{}{
	"agent":     {},
	"claude":    {},
	"codex":     {},
	"hooks":     {},
	"policy":    {},
	"polling":   {},
	"repo":      {},
	"sandbox":   {},
	"server":    {},
	"tracker":   {},
	"verify":    {},
	"workspace": {},
}

func logUnknownTopLevelKeys(front []byte) {
	var raw map[string]any
	if err := yaml.Unmarshal(front, &raw); err != nil {
		return
	}
	unknown := make([]string, 0)
	for key := range raw {
		if _, ok := knownTopLevelWorkflowKeys[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	for _, key := range unknown {
		log.Printf("workflow: unknown top-level key %s ignored", key)
	}
}
