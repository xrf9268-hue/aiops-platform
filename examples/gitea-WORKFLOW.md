---
repo:
  owner: your-gitea-user
  name: your-repo
  clone_url: git@gitea.local:your-gitea-user/your-repo.git
  default_branch: main

tracker:
  kind: gitea
  endpoint: http://gitea.local
  # Worker-held token for polling and the agent tool surface; expanded from
  # the worker's environment, never exposed to the agent. The worker does not
  # read GITEA_TOKEN directly — this whole-value $VAR reference is the only
  # way the token reaches it.
  api_key: $GITEA_TOKEN
  # Optional: raise this for large Gitea repositories. A cap hit skips only the
  # overflowing aiops/* state label and logs the diagnostic.
  # pagination_max_pages: 25
  active_states:
    - Todo
    - Rework
  terminal_states:
    - Done
    - Canceled
  inactive_states:
    - Human Review

polling:
  interval_ms: 30000

workspace:
  root: ~/aiops-workspaces/personal

agent:
  default: mock
  max_concurrent_agents: 1
  max_turns: 8

codex:
  command: codex app-server

claude:
  command: claude

policy:
  mode: draft_pr
  # Scope and path rules belong in the prompt body (SPEC §3.2); hard path
  # prevention belongs to `sandbox:` write restrictions. The worker path/diffstat
  # gate was removed in #561 — `deny_paths` / `max_changed_*` are not accepted.

verify:
  commands:
    - go test ./...

# PR handoff (draft state, labels, reviewers) is the agent's responsibility via
# its tool surface (SPEC §1, #76); express it in the prompt body below. The
# `pr:` front-matter block was removed in #578.
---
You are working on a personal AI coding task from Gitea.

Task:
- ID: {{ task.id }}
- Title: {{ task.title }}
- Actor: {{ task.actor }}

Repository:
- {{ repo.owner }}/{{ repo.name }}
- Base branch: {{ repo.branch }}

Description:
{{ task.description }}
