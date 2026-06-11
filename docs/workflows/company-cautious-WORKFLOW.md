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
    - Todo
    - In Progress
    - Rework
  terminal_states:
    - Done
    - Canceled
  # SPEC §6.4 opt-in dispatch gate: an issue is only picked up once it carries
  # EVERY label below (matched case-insensitively). Removing the label from a
  # running issue stops the agent on the next poll. Omit or leave empty to
  # dispatch every active issue. A blank entry matches no issue.
  required_labels:
    - aiops-ready

polling:
  interval_ms: 30000

workspace:
  root: ~/aiops-workspaces/company

agent:
  # Start with the mock runner. It exercises the worker pipeline (clone,
  # branch, workspace prep, runner loop) without authoring
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
  # Path/scope rules (e.g. "do not touch infra/**, deploy/**, secrets/**,
  # .github/**; keep the diff small") belong in the prompt body below (SPEC
  # §3.2). For HARD path prevention on a company repo, restrict writes via the
  # `sandbox:` block. The worker path/diffstat gate was removed in #561 —
  # `deny_paths` / `max_changed_*` are no longer accepted here.

# The cautious safety envelope (allowed networks/paths/commands, forbidden
# actions) is expressed in the prompt body below (SPEC §3.2) — see the Process
# and Rules sections. The descriptive `safety:` front-matter block was removed in
# #578 because an inert struct that enforced nothing falsely implied it did;
# worker-enforced process hardening lives under `sandbox:`.

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

# PR handoff (draft state, labels, reviewers) is the agent's responsibility via
# its tool surface (SPEC §1, #76) — see Process step 5 below, which tells the
# agent to open a draft PR, apply the cautious-mode labels, and request the
# company reviewer. The `pr:` front-matter block was removed in #578.
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
   that respects the off-limits paths stated in the prompt and keeps the diff
   small (≤12 files / ≤300 LOC review guideline).
4. Run the verification commands and capture results.
5. Open the PR as a draft, label it `ai-generated`, `needs-review`, and
   `cautious-mode`, and request review from your-company-reviewer. Summarize what
   you changed, why, and how you verified it in the pull request description.
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
   worker's. Stay here until you have reviewed several runs and confirmed the
   prompt + `sandbox:` write restrictions keep changes out of the directories
   you expect.

2. **Claude with draft PRs** (`claude`).
   When you are ready to let a model author code, switch
   `agent.default` to `claude` while keeping `policy.mode: draft_pr` and a
   prompt that tells the agent to open draft PRs. State the off-limits paths and
   a tight size budget in the prompt, and keep the `sandbox:` write restrictions
   conservative.

3. **Codex with draft PRs** (`codex-app-server`).
   Once the Claude loop looks healthy, swap `agent.default` to `codex-app-server`
   under the same guardrails. Raise the size caps only after several
   PRs have been reviewed and merged cleanly.

Do not switch off `mock` if any of the following is still true:

- Branch protection is not enforced on the default branch.
- A low-privilege bot account is not in use for Gitea or GitHub.
- Reviewers are not consistently triaging `ai-generated` PRs.
- Recent runs have surfaced out-of-scope edits or scope creep in review.

When in doubt, stay on `mock`. Cautious mode is the default for
company repositories until the workflow has earned trust.
