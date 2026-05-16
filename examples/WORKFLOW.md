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
  # statuses names the Linear workflow states used for handoff updates
  # (for example claim -> in_progress, PR opened -> human_review, failure
  # -> rework). Per SPEC §1, ticket writes belong on the agent/tool side;
  # transitional worker-side writes remain only until app-server tool
  # transport is complete. Defaults match Linear's stock template;
  # uncomment and edit only if your board uses different labels.
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
  # profile selects how the runner invokes codex:
  #   safe   (default) - codex exec --full-auto --skip-git-repo-check ...
  #   bypass           - codex exec --dangerously-bypass-approvals-and-sandbox ...
  #                      (only when the worker host is already isolated, e.g. a
  #                      container; codex bypasses its own sandbox + approval gates)
  #   custom           - run the literal codex.command via sh -lc; PROMPT.md
  #                      is piped on stdin. Escape hatch for bespoke wrappers.
  profile: safe

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
  # draft: true tells the agent/tooling to open the PR as a draft. For Gitea,
  # this may be represented by a `WIP: ` title prefix (Gitea's canonical draft
  # signal) when the PR tool/CLI creates the handoff.
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
6. Commit your changes, push the work branch to the remote, and open a draft pull request from the work branch to the base branch.
7. If tracker tooling is available, move/comment on the ticket from the agent/tool surface rather than expecting the orchestrator to do it.
8. Stop and explain the blocker if the task is ambiguous or unsafe.

Rules:
- Do not touch secrets, credentials, production deployment files, or database migrations.
- Do not do broad refactors unless explicitly requested.
- Prefer small reviewable changes.
- Draft PRs require human review.
- The orchestrator will not push, open PRs, or write tracker state for you; those are agent responsibilities per SPEC §1.
