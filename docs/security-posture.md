# Security posture

This runbook documents the current safety model for `aiops-platform` and the
controls operators should apply before connecting it to real repositories or
trackers. It is intentionally conservative: OpenAI Symphony SPEC §15 treats
harness hardening as part of the core safety model, not as an optional add-on.

## Current sandbox model

`aiops-platform` always relies on the selected coding agent's own sandbox and
approval behavior. For Codex runs, that means the Codex app-server sandbox
selected by `codex.thread_sandbox` / `codex.turn_sandbox_policy` in `WORKFLOW.md`.

The platform now also supports an optional Linux process sandbox wrapper for
agent invocation. It is disabled by default; operators enable it explicitly with
the worker-enforced `sandbox:` workflow block when the host has a supported
backend installed:

```yaml
sandbox:
  enabled: true
  backend: firejail        # firejail or bubblewrap
  network: allowlist       # none or allowlist
  network_allowlist_cidrs:
    - 203.0.113.10/32
  env_allowlist:
    - PATH
    - AIOPS_RUN_TOKEN
  credential_files:
    - ~/.config/aiops/run-token
```

Supported enforcement today:

- `bubblewrap` or `firejail` wraps the agent process when configured;
- the agent working directory must remain under `workspace.root` before the
  sandbox wrapper is applied;
- the child process environment is reduced to `sandbox.env_allowlist`;
- the agent executable must live under a path the sandbox backend exposes
  (for example `/usr` or `/bin` with the current bubblewrap/firejail profile);
- explicitly listed credential files are checked for readability and bound into
  the sandbox read-only;
- `network: none` disables network access for supported backends;
- `network: allowlist` is supported through `firejail --netfilter` and CIDR
  allowlist rules.

Still not provided:

- Docker-per-run workspace isolation;
- VM isolation or macOS `sandbox-exec` support;
- a credential vault that mints per-run credentials;
- durable persistence for input-blocked state across worker restarts.

Docker-based workspace isolation should be a follow-up rather than part of this
phase: it changes workspace creation, cache ownership, Git remotes/credentials,
and artifact handoff semantics, while the current phase only wraps the already
selected agent invocation. Track Docker isolation separately so it can preserve
the workspace-root invariant and SPEC workspace lifecycle behavior deliberately.

## Docker Compose SSH key isolation

`deploy/docker-compose.yml` does **not** bind-mount the operator's full
`~/.ssh` directory into the worker container. Doing so was the prior
default and exposed the operator's entire SSH key set, `known_hosts`, and
`config` to the agent process — a single prompt-injection or malicious
dependency that read `~/.ssh/id_*` could exfiltrate every keypair on
the host.

Today the worker container receives only two file-level binds, under the
unprivileged `aiops` user's home directory:

| Host path (default)       | Container path                       |
| ------------------------- | ------------------------------------ |
| `deploy/ssh/id_ed25519`   | `/home/aiops/.ssh/id_ed25519:ro`     |
| `deploy/ssh/known_hosts`  | `/home/aiops/.ssh/known_hosts:ro`    |

Both paths are overridable through environment variables in the operator's
`.env`:

```dotenv
AIOPS_SSH_KEY_PATH=...
AIOPS_SSH_KNOWN_HOSTS_PATH=...
```

Operators generate the dedicated keypair under `deploy/ssh/` with
`ssh-keygen` and add the public key as a Gitea / GitHub deploy key on the
target repository. See `deploy/ssh/README.md` and
`docs/runbooks/local-dev.md` for the step-by-step setup.

**Threat reduced, not eliminated.** The worker container runs as the
unprivileged `aiops` user (`USER aiops` in `Dockerfile`), with
`no-new-privileges:true` and all Linux capabilities dropped in Compose. A
successful container breakout or a write-side compromise can still misuse
the mounted deploy key — but the key's blast radius is bounded to the
repos that deploy key authorises, not every repo on the operator's host,
and a compromised command no longer executes as root inside the container.

## Trust boundary

Treat all of these inputs as potentially hostile unless you control them:

- issue titles, descriptions, comments, and labels;
- repository contents, build scripts, dependency install hooks, tests, and
  generated files;
- `WORKFLOW.md` prompt text and hooks;
- tool arguments passed to client-side tools such as tracker or PR APIs.

The worker creates or reuses a per-issue workspace and runs the coding agent with
that workspace as the current directory. Subprocess-backed runners validate that
the launch cwd is the per-issue workspace path and that the workspace path stays
under the configured workspace root before starting the coding agent, independent
of whether the optional sandbox wrapper is enabled. SPEC §15.1 is explicit that
path validation is not a substitute for approval policy, credential scoping, or
external sandboxing.

## What is defended today

The current Go implementation provides these safety controls:

- per-issue workspaces under a configured workspace root;
- sanitized workspace identifiers;
- coding-agent execution from the per-issue workspace directory;
- runner-side `cwd` and workspace-root validation before agent subprocess
  launch, even when `sandbox.enabled` is false;
- draft-PR mode for human review before merge;
- `$VAR` indirection for secrets in workflow configuration;
- masking of secret values in configuration inspection output;
- operator-visible blocked state for Codex input-required and MCP elicitation
  requests, so non-interactive runs stop and remain claimed instead of burning
  retries silently;
- allow-listed environment for agent and hook subprocesses: by
  default these children run with only a small POSIX baseline env (`PATH`,
  `HOME`, `USER`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TZ`, `TERM`). Tracker/repo
  tokens (`LINEAR_API_KEY`, `GITHUB_TOKEN`, `GITEA_TOKEN`) and any other secret
  in the worker's `.env` are excluded — a malicious or buggy WORKFLOW.md cannot
  `env > /tmp/dump` and exfiltrate them. Operators opt non-tracker vars back in
  per workflow with `codex.env_passthrough`, `claude.env_passthrough`, or
  `hooks.env_passthrough`; agent passthrough
  rejects tracker/repo API token names so those credentials stay behind
  orchestrator-owned tools. See
  [`docs/design/hook-verify-env-allowlist.md`](design/hook-verify-env-allowlist.md);
- allow-listed redaction of Codex `turn/failed`, `turn/cancelled`, and failed
  `turn/completed` protocol payloads: returned error strings and
  `runtime_events` JSON payloads carry only the documented
  `status`/`reason`/`error`/`message`/`error_code` fields (plus the same keys
  under nested `turn`), with `"reason unavailable"` as fallback. Arbitrary
  protocol fields — including any tool-call output, elicitation echoes, or
  future Codex additions — are never embedded in worker error strings,
  `RecordEvent.Message`, or `/api/v1/state` payloads;
- branch protection and review gates when configured on the remote repository.

These controls reduce accidental damage and make changes reviewable. They do not
make arbitrary repositories, issue authors, dependencies, or commands safe.

## What is not defended today

Unless the optional sandbox wrapper is enabled and validated on the worker host,
do not assume the platform prevents a malicious or compromised agent run from:

- reading host files that are accessible to the worker OS user;
- reading credentials present in the process environment or filesystem;
- making arbitrary outbound network connections allowed by the host;
- running destructive commands as the worker OS user;
- executing malicious dependency lifecycle hooks or test scripts;
- exfiltrating repository, tracker, or environment data through logs, PR text,
  tracker comments, or network requests;
- mutating repositories or trackers beyond what the configured credentials can
  access.

A prompt-injection in a trusted-looking issue or repository can still try to
convince the agent to use its available tools unsafely. Keep the available tools,
credentials, filesystem paths, and network destinations to the minimum needed
for the workflow.

## Explicit untrusted-use warning

Do not use this platform against untrusted issue authors, untrusted repositories,
unreviewed third-party dependency trees, or shared production secrets. If you
need that deployment model, first enable and validate an external sandbox such
as bubblewrap or firejail, or add a stronger container/VM isolation layer, plus
network egress controls and per-run credential scoping.

## Operator checklist

Before pointing `aiops-platform` at any repository, especially a company
repository:

1. Use a dedicated low-privilege bot account for Git hosting and tracker access.
   Grant only the repositories, projects, teams, and labels needed for the
   workflow.
2. Keep branch protection enabled on the default branch. Require human review and
   passing CI before merge.
3. Keep `policy.mode: draft_pr` and instruct the agent to open draft PRs in the
   WORKFLOW prompt (PR handoff is agent-side per SPEC §1, #76) until several runs
   have been reviewed cleanly.
4. Start with `agent.default: mock`. Only switch to a real coding agent after the
   mock loop has proven clone, branch, PR, label, and policy behavior.
5. Do not pass shared production secrets, broad cloud credentials, personal
   tokens, SSH agents, or customer data into the worker environment or workspace.
6. Keep `.env`, `.env.*`, private keys, and service-account files outside the
   repository and workspace unless a specific run absolutely requires them.
7. Keep the agent away from sensitive directories (deployment manifests,
   infrastructure, migrations, billing, auth, secrets): state them as
   off-limits in the `WORKFLOW.md` prompt (SPEC §3.2) and, for hard
   prevention, restrict writes via the `sandbox:` block. (The worker
   `policy.deny_paths` / `max_changed_*` gate was removed in #561: it ran
   post-push, so it could only flag — never prevent — a forbidden change.)
8. Keep changes small enough for reliable review — instruct the agent to keep
   diffs tight in the prompt, and split oversized PRs at review time.
9. Restrict tracker eligibility to trusted projects, teams, labels, and workflow
   states. Do not let arbitrary tracker items automatically reach the agent.
10. Prefer project-scoped tracker tools. If `linear_graphql` is available, scope
    credentials and prompts so it only operates on the intended project.
11. Review every agent-authored PR before merge, including generated files,
    workflow changes, dependency updates, and scripts run by CI.
12. Scan for credential leaks before the branch is pushed — run a secret
    scanner (e.g. `gitleaks`) from the WORKFLOW prompt as part of the agent's
    pre-handoff checks, and rely on PR CI / your host's secret scanning as the
    backstop. The worker does not scan after the fact: push is the agent's
    responsibility (SPEC §1), so a post-turn worker scan could only flag a leak
    that was already pushed, never prevent it (#561).
13. Run workers under a dedicated OS user and keep the workspace root permissions
    restricted to that user.
14. Treat logs, run summaries, PR bodies, and tracker comments as data exfiltration
    surfaces. Do not include raw secrets or customer data.

## Company repository minimum posture

For company repositories, use `docs/workflows/company-cautious-WORKFLOW.md` as the
starting point and keep the following minimum posture until an external sandbox
lands:

- `agent.default: mock` for initial validation;
- `policy.mode: draft_pr`;
- a WORKFLOW prompt that tells the agent to open draft PRs (SPEC §1, #76);
- conservative changed-file and changed-LOC limits;
- explicit denied paths for sensitive directories;
- low-privilege bot credentials;
- branch protection with required human review;
- no shared secrets in the worker environment or workspace.

If any item above is not available, do not run the coding agent on that company
repository yet. Use mock or analysis-only workflows instead.

## Remaining hardening roadmap

The Linux wrapper is a first enforcement layer, not a complete untrusted-code
sandbox. Remaining work includes:

- Docker-based or VM workspace isolation as an alternative to bare-filesystem
  workspaces;
- stronger credential-vault integration that mints per-run credentials instead
  of binding existing files;
- backend-specific operational validation on each supported host distribution;
- broader lifecycle-hook integration once SPEC workspace hooks are implemented.

Until those controls are implemented and tested for your deployment, document
this platform as a trusted-environment orchestrator with optional process
sandboxing, review, and policy guardrails, not as a strong sandbox for arbitrary
untrusted code.
