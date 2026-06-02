---
repo:
  owner: your-gitea-user
  name: your-repo
  clone_url: git@gitea.local:your-gitea-user/your-repo.git
  default_branch: main

tracker:
  kind: gitea
  endpoint: http://gitea.local
  # Optional: raise this for large Gitea repositories. A cap hit skips only the
  # overflowing aiops/* state label and logs the diagnostic.
  # pagination_max_pages: 25
  active_states:
    - AI Ready
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

pr:
  draft: true
  labels:
    - ai-generated
    - needs-review
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
