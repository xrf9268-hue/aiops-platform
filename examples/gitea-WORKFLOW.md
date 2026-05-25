---
repo:
  owner: your-gitea-user
  name: your-repo
  clone_url: git@gitea.local:your-gitea-user/your-repo.git
  default_branch: main

tracker:
  kind: gitea
  endpoint: http://gitea.local
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
  command: codex exec
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
