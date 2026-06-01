---
repo:
  owner: your-company-org
  name: your-company-repo
  clone_url: git@gitea.company.local:your-company-org/your-company-repo.git
  default_branch: main

tracker:
  kind: linear
  # Required for Linear: maps to SPEC §11.2 project.slugId filtering.
  project_slug: your-linear-project-slug
  api_key: $LINEAR_API_KEY
  active_states:
    - AI Ready
    - In Progress
    - Rework
  terminal_states:
    - Done
    - Canceled

polling:
  interval_ms: 30000

workspace:
  root: ~/aiops-workspaces/company

agent:
  # Start with the mock runner. It exercises the worker pipeline (clone,
  # branch, workspace prep, policy checks, runner loop) without authoring
  # any code, so you can validate policy guardrails before letting a
  # real model touch the repository. PR creation, labels, and comments are
  # the agent's responsibility per SPEC §1, not the worker's. Only the runners registered in
  # `internal/runner/runner.go` (`mock`, `codex-app-server`, `claude`) can
  # execute; any other name fails the task with `unknown runner`. Switch to
  # `codex-app-server` (or `claude`) only after auditing several mock runs.
  default: mock
  max_concurrent_agents: 1
  max_turns: 6

codex:
  command: codex app-server

claude:
  command: claude

policy:
  # draft_pr keeps every change behind human review even after you
  # graduate from analysis-only. Do not set this to a non-draft mode
  # for company repositories.
  mode: draft_pr
  # The current `internal/workspace.matchesPath` only enforces patterns
  # ending in `/**` (prefix match) or `/*` (also prefix). Globs that
  # start with `**/` (e.g. `**/secrets/**`) become a literal prefix that
  # no real file path matches, so they silently fail open. List each
  # top-level directory you actually want to block. Add more entries if
  # your repository keeps these concerns under a different prefix
  # (e.g. `services/billing/**`, `apps/web/migrations/**`).
  deny_paths:
    - infra/**
    - deploy/**
    - secrets/**
    - auth/**
    - billing/**
    - migrations/**
    - db/migrations/**
    - .github/**
  max_changed_files: 8
  max_changed_loc: 200

# Safety policy for cautious company runs. These entries are descriptive: they
# make the expected network/path/command envelope explicit for the agent and
# reviewers. Worker-enforced process hardening lives under `sandbox:`.
safety:
  allowed_networks:
    - company Git host for this repository
    - configured Linear/Gitea tracker API
    - configured pull-request host
    - package registries required by the repository lockfiles
  allowed_paths:
    - repository workspace for this task
    - tool caches without shared credentials
  allowed_commands:
    - repository build, test, lint, and formatting commands
    - git commands for the work branch
    - tracker/PR tool calls needed for draft handoff
  forbidden:
    - reading files outside the workspace unless explicitly required
    - using production, customer, or personal credentials
    - contacting unrelated external services
    - modifying denied policy paths

# Optional worker-enforced process hardening. Keep disabled while validating the
# mock runner; enable only on worker hosts where the selected backend is
# installed and tested. `network: allowlist` requires the firejail backend.
sandbox:
  enabled: false
  backend: none
  network: none
  # network_allowlist_cidrs:
  #   - 203.0.113.10/32
  # env_allowlist:
  #   - PATH
  # credential_files: []

verify:
  commands:
    - go test ./...

pr:
  # draft: true tells the agent/tooling to open the PR as a draft. For Gitea,
  # this may be represented by a `WIP: ` title prefix (Gitea's canonical draft
  # signal) when the PR tool/CLI creates the handoff.
  draft: true
  labels:
    - ai-generated
    - needs-review
    - cautious-mode
  reviewers:
    - your-company-reviewer
---
You are working on a company AI coding task under cautious mode.

Task:
- ID: {{ task.id }}
- Title: {{ task.title }}
- Actor: {{ task.actor }}

Repository:
- {{ repo.owner }}/{{ repo.name }}
- Base branch: {{ repo.branch }}

Description:
{{ task.description }}

Process:
1. Read the relevant files before proposing any change.
2. While the workflow is on the `mock` runner, no code is authored;
   use those runs to confirm the worker can clone, branch, prepare the
   workspace, and run the agent loop correctly. PR creation, labels, and
   review handoff are the agent's responsibility per SPEC §1, not the
   worker's.
3. After graduating to `codex-app-server` or `claude`, make the smallest safe edit
   that respects every entry in `policy.deny_paths` and stays inside
   `max_changed_files` / `max_changed_loc`.
4. Run the verification commands and capture results.
5. Summarize what you changed, why, and how you verified it in the pull request description.
6. Stop and explain the blocker if the task is ambiguous, exceeds
   policy limits, or touches a denied path.

Rules:
- Never modify infrastructure, deploy manifests, secrets, auth code,
  billing logic, or database migrations.
- Never broaden the scope of a task; raise a follow-up issue instead.
- Draft PRs require human review before merge. Do not request merge.
- Do not exfiltrate secrets, tokens, or customer data into logs,
  prompts, or PR descriptions.

# When to switch runners

This template ships with `agent.default: mock` on purpose. Only the
runners registered in `internal/runner/runner.go` are accepted today
(`mock`, `codex-app-server`, `claude`); any other name causes the worker to fail
the task with `unknown runner`. Promote a company repository through
the following stages, and only move forward after the previous stage
has produced a clean audit trail:

1. **Mock runner end-to-end** (`mock`).
   Use this to validate that the repository, tracker, and policy
   guardrails behave correctly. The mock runner produces deterministic
   workspace artifacts and never authors code; PR creation, labels, and
   review handoff are the agent's responsibility per SPEC §1, not the
   worker's. Stay here until you have reviewed several runs and confirmed
   `deny_paths` blocks the directories you expect.

2. **Claude with draft PRs** (`claude`).
   When you are ready to let a model author code, switch
   `agent.default` to `claude` while keeping `policy.mode: draft_pr`,
   `pr.draft: true`, and the full `deny_paths` list. Keep
   `max_changed_files` and `max_changed_loc` conservative.

3. **Codex with draft PRs** (`codex-app-server`).
   Once the Claude loop looks healthy, swap `agent.default` to `codex-app-server`
   under the same guardrails. Raise the size caps only after several
   PRs have been reviewed and merged cleanly.

Do not switch off `mock` if any of the following is still true:

- Branch protection is not enforced on the default branch.
- A low-privilege bot account is not in use for Gitea or GitHub.
- Reviewers are not consistently triaging `ai-generated` PRs.
- Recent mock runs have surfaced policy violations or scope creep.

When in doubt, stay on `mock`. Cautious mode is the default for
company repositories until the workflow has earned trust.
