---
# This is the service-level WORKFLOW.md selected when the worker process starts:
# pass it explicitly as `worker /path/to/WORKFLOW.md`, or run the worker from a
# directory containing `WORKFLOW.md`. Per Symphony SPEC §5.1, the worker loads
# this single file for the service and reuses it for every task; per-task
# repository checkouts do not override it.
repo:
  owner: your-gitea-user
  name: your-repo
  clone_url: git@gitea.local:your-gitea-user/your-repo.git
  default_branch: main

tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  # Required for Linear: maps to the Linear project slugId used by SPEC §11.2
  # project-scoped polling. Example: https://linear.app/acme/project/aiops-platform-abc123
  # uses project_slug: aiops-platform-abc123
  project_slug: your-linear-project-slug
  active_states:
    - AI Ready
    - In Progress
    - Rework
  terminal_states:
    - Done
    - Canceled
  inactive_states:
    - Backlog
    - Human Review
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

server:
  # Private-loopback SPEC §13.7 state endpoint: GET /api/v1/state
  # Set to -1 to disable when another deployment layer owns state serving.
  port: 4000

polling:
  interval_ms: 30000

# Optional multi-service routing (tracked as D25/#143 until the extension schema
# is fully documented). Omit `services` for the default single-service mode
# above. When present for Linear workflows, candidate selection matches
# project/team, labels, and custom fields, then dispatches the issue with that
# service's repo. Unmatched issues are skipped locally; ambiguous matches fail
# the poll tick. This is read-only routing: ticket writes remain agent/tool-side
# per SPEC §1.
# services:
#   - name: api
#     repo:
#       owner: your-gitea-user
#       name: api
#       clone_url: git@gitea.local:your-gitea-user/api.git
#       default_branch: main
#     tracker:
#       project_slug: api-platform
#       team_key: ENG
#       labels: [backend]
# `tracker.custom_fields:` is currently rejected at workflow load —
# Linear's GraphQL schema does not expose Issue custom fields. See #326.

workspace:
  root: ~/aiops-workspaces/personal

agent:
  default: mock
  max_concurrent_agents: 1
  max_turns: 8

codex:
  command: codex exec
  # profile selects how the runner invokes codex:
  #   safe   (default) - codex exec --sandbox workspace-write --skip-git-repo-check ...
  #   bypass           - codex exec --dangerously-bypass-approvals-and-sandbox ...
  #                      (only when the worker host is already isolated, e.g. a
  #                      container; codex bypasses its own sandbox + approval gates)
  #   custom           - run the literal codex.command via sh -c; PROMPT.md
  #                      is piped on stdin. Escape hatch for bespoke wrappers.
  profile: safe
  # linear_graphql narrows the agent-visible Linear GraphQL tool to the
  # operator-chosen surface (SPEC §15.5 harness hardening / #298). The
  # zero value (omit the block) keeps the safest posture: queries are
  # permitted, mutations are rejected before any HTTP request leaves
  # the orchestrator, and prompt-injected `issueDelete` /
  # `commentDelete` mutations cannot reach Linear. Flip
  # `allow_mutations: true` once you intend agents to drive Linear
  # state moves themselves; optionally narrow to a per-operation
  # allow-list.
  #
  # linear_graphql:
  #   allow_mutations: true
  #   allowed_mutations:
  #     - issueUpdate
  #     - commentCreate

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

# Safety policy for the agent and human reviewers. This block is descriptive:
# it documents the expected envelope but does not itself enforce network/path or
# command controls. Worker-enforced process hardening lives under `sandbox:`.
safety:
  allowed_networks:
    - git remote for this repository
    - configured issue tracker API
    - configured pull-request host
  allowed_paths:
    - repository workspace for this task
    - language/tool caches that do not contain shared credentials
  allowed_commands:
    - repository build, test, lint, and formatting commands
    - git commands needed to commit and push the work branch
    - tracker/PR tool calls needed for the workflow handoff
  forbidden:
    - reading host files outside the workspace unless explicitly required
    - using shared production secrets or personal credentials
    - contacting unrelated external services
    - changing deployment, infrastructure, migration, or secret paths

# Optional worker-enforced process hardening. Disabled by default so personal
# workflows continue to rely on the selected coding agent's own sandbox. Enable
# only after installing and validating the backend on the worker host. The
# selected agent binary must live under a path exposed by that backend profile.
sandbox:
  enabled: false
  backend: none      # none, bubblewrap, or firejail
  network: none      # none, or allowlist (allowlist requires firejail)
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
7. If tracker tooling is available, move/comment on the ticket from the agent/tool surface rather than expecting the orchestrator to do it. For Linear workflows, use the advertised `linear_graphql` dynamic tool for ticket comments and workflow-state mutations; the orchestrator proxies the request with its configured Linear auth so the API token never has to appear in the agent process or prompt.
8. Stop and explain the blocker if the task is ambiguous or unsafe.

Rules:
- Do not touch secrets, credentials, production deployment files, or database migrations.
- Do not do broad refactors unless explicitly requested.
- Prefer small reviewable changes.
- Draft PRs require human review.
- The orchestrator will not push, open PRs, or write tracker state for you; those are agent responsibilities per SPEC §1.
