# Security posture

This runbook documents the current safety model for `aiops-platform` and the
controls operators should apply before connecting it to real repositories or
trackers. It is intentionally conservative: OpenAI Symphony SPEC §15 treats
harness hardening as part of the core safety model, not as an optional add-on.

## Current sandbox model

`aiops-platform` currently relies on the selected coding agent's own sandbox and
approval behavior. For Codex runs, that means the Codex CLI sandbox selected by
`codex.profile` in `WORKFLOW.md`.

The platform does not currently add an external OS, container, VM, or network
sandbox around the agent process. In particular, it does not yet provide:

- bubblewrap, firejail, `sandbox-exec`, or VM isolation;
- Docker-per-run workspace isolation;
- a sandbox-layer network egress allowlist;
- a filesystem allowlist beyond the workspace and policy checks already wired in
  the worker;
- a credential vault that mints per-run credentials.

This is the tracked D5 deviation: the present posture is acceptable for personal
repositories and trusted internal codebases where the operator understands the
risk, but it is not a hardened untrusted-code execution environment.

## Trust boundary

Treat all of these inputs as potentially hostile unless you control them:

- issue titles, descriptions, comments, and labels;
- repository contents, build scripts, dependency install hooks, tests, and
  generated files;
- `WORKFLOW.md` prompt text and hooks;
- tool arguments passed to client-side tools such as tracker or PR APIs.

The worker creates or reuses a per-issue workspace and runs the coding agent with
that workspace as the current directory. Workspace path validation is a baseline
control and must keep workspaces under the configured workspace root, but SPEC
§15.1 is explicit that path validation is not a substitute for approval policy,
credential scoping, or external sandboxing.

## What is defended today

The current Go implementation provides these safety controls:

- per-issue workspaces under a configured workspace root;
- sanitized workspace identifiers;
- coding-agent execution from the per-issue workspace directory;
- workflow-configured policy limits such as `deny_paths`, `max_changed_files`,
  and `max_changed_loc`;
- draft-PR mode for human review before merge;
- `$VAR` indirection for secrets in workflow configuration;
- masking of secret values in configuration inspection output;
- optional pre-push secret scanning;
- branch protection and review gates when configured on the remote repository.

These controls reduce accidental damage and make changes reviewable. They do not
make arbitrary repositories, issue authors, dependencies, or commands safe.

## What is not defended today

Until an external sandbox layer exists, do not assume the platform prevents a
malicious or compromised agent run from:

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
need that deployment model, first add and validate an external sandbox such as a
container, VM, bubblewrap, or firejail profile plus network egress controls and
per-run credential scoping.

## Operator checklist

Before pointing `aiops-platform` at any repository, especially a company
repository:

1. Use a dedicated low-privilege bot account for Git hosting and tracker access.
   Grant only the repositories, projects, teams, and labels needed for the
   workflow.
2. Keep branch protection enabled on the default branch. Require human review and
   passing CI before merge.
3. Keep `policy.mode: draft_pr` and `pr.draft: true` until several runs have
   been reviewed cleanly.
4. Start with `agent.default: mock`. Only switch to a real coding agent after the
   mock loop has proven clone, branch, PR, label, and policy behavior.
5. Do not pass shared production secrets, broad cloud credentials, personal
   tokens, SSH agents, or customer data into the worker environment or workspace.
6. Keep `.env`, `.env.*`, private keys, and service-account files outside the
   repository and workspace unless a specific run absolutely requires them.
7. Configure `policy.deny_paths` for sensitive directories such as deployment
   manifests, infrastructure, migrations, billing, auth, and secrets.
8. Keep `policy.max_changed_files` and `policy.max_changed_loc` small enough for
   reliable review.
9. Restrict tracker eligibility to trusted projects, teams, labels, and workflow
   states. Do not let arbitrary tracker items automatically reach the agent.
10. Prefer project-scoped tracker tools. If `linear_graphql` is available, scope
    credentials and prompts so it only operates on the intended project.
11. Review every agent-authored PR before merge, including generated files,
    workflow changes, dependency updates, and scripts run by CI.
12. Enable the optional secret-scan hook where practical; see
    `docs/runbooks/secret-scanning.md`.
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
- `pr.draft: true`;
- conservative changed-file and changed-LOC limits;
- explicit denied paths for sensitive directories;
- low-privilege bot credentials;
- branch protection with required human review;
- no shared secrets in the worker environment or workspace.

If any item above is not available, do not run the coding agent on that company
repository yet. Use mock or analysis-only workflows instead.

## Phase 2 hardening roadmap

The next enforcement phase should add optional external isolation rather than
expanding the trust boundary of the current host process. Planned work includes:

- a Linux sandbox wrapper such as bubblewrap or firejail around the agent
  invocation;
- Docker-based workspace isolation as an alternative to bare-filesystem
  workspaces;
- network egress allowlists enforced by the sandbox layer;
- a documented way to pass only the minimum credentials needed by the run;
- validation that the agent cwd and workspace path remain under the configured
  workspace root after isolation is applied.

Until those controls are implemented and tested, document this platform as a
trusted-environment orchestrator with review and policy guardrails, not as a
strong sandbox for untrusted code.
