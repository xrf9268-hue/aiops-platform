---
repo:
  owner: your-gitea-user
  name: your-repo
  clone_url: git@gitea.local:your-gitea-user/your-repo.git
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
  # statuses configures the Linear workflow state names the worker drives
  # the issue through as a task progresses:
  #   - claim          -> in_progress
  #   - PR opened      -> human_review
  #   - failure        -> rework (or a comment if the move fails)
  # Defaults match Linear's stock template; uncomment and edit only if
  # your board uses different labels.
  # statuses:
  #   in_progress: "In Progress"
  #   human_review: "Human Review"
  #   rework: "Rework"

workspace:
  root: ~/aiops-workspaces/personal

agent:
  default: mock
  max_concurrent_agents: 1
  max_turns: 8

codex:
  command: codex exec

claude:
  command: claude

policy:
  mode: draft_pr
  deny_paths:
    - infra/**
    - deploy/**
    - db/migrations/**
    - secrets/**
  max_changed_files: 12
  max_changed_loc: 300

verify:
  commands:
    - go test ./...

pr:
  # draft: true → worker opens the PR as a Gitea draft. Implementation note:
  # Gitea has no `draft` API field, so the worker prepends `WIP: ` to the PR
  # title (Gitea's canonical draft signal). Expect titles like
  # `WIP: chore(ai): <issue title>`.
  draft: true
  labels:
    - ai-generated
    - needs-review
---
You are working on a personal AI coding task.

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
1. Inspect the repository before editing.
2. Make the smallest safe change.
3. Keep changes inside the allowed scope.
4. Run the verification commands.
5. Write a concise summary in `.aiops/RUN_SUMMARY.md`.
6. Stop and explain the blocker if the task is ambiguous or unsafe.

Rules:
- Do not touch secrets, credentials, production deployment files, or database migrations.
- Do not do broad refactors unless explicitly requested.
- Prefer small reviewable changes.
- Draft PRs require human review.
