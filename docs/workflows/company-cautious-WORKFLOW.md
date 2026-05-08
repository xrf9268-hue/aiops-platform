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
  # Start in analysis-only. The Claude analysis runner produces a plan
  # artifact and never commits code. Switch to `codex` only after
  # auditing several analysis runs and confirming the policy guardrails.
  default: claude-analysis
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
  deny_paths:
    - infra/**
    - deploy/**
    - "**/secrets/**"
    - "**/auth/**"
    - billing/**
    - "**/migrations/**"
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
2. In analysis-only mode, produce a written plan in `.aiops/PLAN.md`
   covering scope, risks, files to touch, and verification.
3. If code changes are eventually allowed, make the smallest safe edit
   that fits inside the policy's `allow_paths` and respects all
   `deny_paths`.
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

# When to switch to Codex

This template ships in analysis-only (`agent.default: claude-analysis`)
on purpose. Promote a company repository through the following stages,
and only move forward after the previous stage has produced a clean
audit trail:

1. **Analysis-only with Claude** (`claude-analysis`).
   Use this to validate that the repository, tracker, and policy
   guardrails behave correctly. The runner writes a plan artifact and
   never commits code. Stay here until you have reviewed at least a
   handful of plans and confirmed `deny_paths` blocks the right
   directories.

2. **Mock runner end-to-end** (`mock`).
   Switch `agent.default` to `mock` to confirm the worker can open a
   draft PR, attach the expected labels, and receive review comments
   without any model-authored code.

3. **Codex with draft PRs** (`codex`).
   Once analysis and the mock loop look healthy, set
   `agent.default: codex` while keeping `policy.mode: draft_pr`,
   `pr.draft: true`, and the full `deny_paths` list. Keep
   `max_changed_files` and `max_changed_loc` conservative; raise them
   only after several Codex PRs have been reviewed and merged cleanly.

Do not switch to Codex if any of the following is still true:

- Branch protection is not enforced on the default branch.
- A low-privilege bot account is not in use for Gitea or GitHub.
- Reviewers are not consistently triaging `ai-generated` PRs.
- Recent analysis runs have surfaced policy violations or scope creep.

When in doubt, stay in analysis-only. Cautious mode is the default for
company repositories until the workflow has earned trust.
