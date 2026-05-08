---
repo:
  owner: your-company-org
  name: your-company-repo
  clone_url: git@gitea.company.local:your-company-org/your-company-repo.git
  default_branch: main

tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  active_states:
    - AI Ready
    - In Progress
    - Rework
  terminal_states:
    - Done
    - Canceled
  poll_interval_ms: 30000

workspace:
  root: ~/aiops-workspaces/company

agent:
  # Start with the mock runner. It executes the worker pipeline end to
  # end (clone, branch, draft PR, labels, comments) without authoring
  # any code, so you can validate policy guardrails before letting a
  # real model touch the repository. Only the runners registered in
  # `internal/runner/runner.go` (`mock`, `codex`, `claude`) can execute;
  # any other name fails the task with `unknown runner`. Switch to
  # `codex` (or `claude`) only after auditing several mock runs.
  default: mock
  fallback: mock
  max_concurrent_agents: 1
  max_turns: 6

codex:
  command: codex exec

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

verify:
  commands:
    - go test ./...

pr:
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
   use those runs to confirm the worker can clone, branch, open a draft
   PR, attach labels, and route review comments correctly.
3. After graduating to `codex` or `claude`, make the smallest safe edit
   that respects every entry in `policy.deny_paths` and stays inside
   `max_changed_files` / `max_changed_loc`.
4. Run the verification commands and capture results.
5. Write a concise summary in `.aiops/RUN_SUMMARY.md`.
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
(`mock`, `codex`, `claude`); any other name causes the worker to fail
the task with `unknown runner`. Promote a company repository through
the following stages, and only move forward after the previous stage
has produced a clean audit trail:

1. **Mock runner end-to-end** (`mock`).
   Use this to validate that the repository, tracker, and policy
   guardrails behave correctly. The mock runner writes a deterministic
   summary, opens the draft PR, attaches labels, and never authors
   code. Stay here until you have reviewed several runs and confirmed
   `deny_paths` blocks the directories you expect.

2. **Claude with draft PRs** (`claude`).
   When you are ready to let a model author code, switch
   `agent.default` to `claude` while keeping `policy.mode: draft_pr`,
   `pr.draft: true`, and the full `deny_paths` list. Keep
   `max_changed_files` and `max_changed_loc` conservative.

3. **Codex with draft PRs** (`codex`).
   Once the Claude loop looks healthy, swap `agent.default` to `codex`
   under the same guardrails. Raise the size caps only after several
   PRs have been reviewed and merged cleanly.

Do not switch off `mock` if any of the following is still true:

- Branch protection is not enforced on the default branch.
- A low-privilege bot account is not in use for Gitea or GitHub.
- Reviewers are not consistently triaging `ai-generated` PRs.
- Recent mock runs have surfaced policy violations or scope creep.

When in doubt, stay on `mock`. Cautious mode is the default for
company repositories until the workflow has earned trust.
