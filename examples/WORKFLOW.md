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
    - Todo
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

workspace:
  root: ~/aiops-workspaces/personal

agent:
  default: mock
  max_concurrent_agents: 1
  max_turns: 8

codex:
  # The SPEC §10 runner is `codex app-server` — a long-running JSON-RPC 2.0
  # session over stdio. Set agent.default: codex-app-server to use it.
  command: codex app-server
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
  # Scope and path rules (which files to keep changes within, which paths to
  # avoid) belong in the prompt body below (SPEC §3.2); hard path prevention
  # belongs to the `sandbox:` write restrictions. The worker path/diffstat gate
  # was removed in #561 — `deny_paths` / `max_changed_*` are no longer accepted.

# The agent's safety envelope (allowed networks/paths/commands, what is
# forbidden) is expressed in the prompt body below (SPEC §3.2) — see the Process
# and Rules sections. The descriptive `safety:` front-matter block was removed in
# #578 because an inert struct that enforced nothing falsely implied it did;
# worker-enforced process hardening lives under `sandbox:`.

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

# PR handoff (draft state, labels, reviewers) is the agent's responsibility via
# its tool surface (SPEC §1, #76) — see Process step 5 below, which tells the
# agent to open a draft PR. The `pr:` front-matter block configured a worker
# capability that no longer exists and was removed in #578.
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
5. Commit your changes, push the work branch to the remote, and open a draft pull request from the work branch to the base branch. Summarize what you changed, why, and how you verified it in the pull request description.
6. If tracker tooling is available, move/comment on the ticket from the agent/tool surface rather than expecting the orchestrator to do it. For Linear workflows, use the advertised `linear_graphql` dynamic tool for ticket comments and workflow-state mutations; the orchestrator proxies the request with its configured Linear auth so the API token never has to appear in the agent process or prompt.
7. Stop and explain the blocker if the task is ambiguous or unsafe.

Rules:
- Do not touch secrets, credentials, production deployment files, or database migrations.
- Do not do broad refactors unless explicitly requested.
- Prefer small reviewable changes.
- Draft PRs require human review.
- The orchestrator will not push, open PRs, or write tracker state for you; those are agent responsibilities per SPEC §1.
