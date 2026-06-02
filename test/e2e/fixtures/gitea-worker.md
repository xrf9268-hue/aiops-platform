---
repo:
  clone_url: http://localhost:3000/aiops-bot/demo-gitea-tracker.git
agent:
  default: mock
  timeout: 5m
policy:
  mode: draft_pr
verify:
  commands: []
tracker:
  kind: gitea
  active_states:
    - AI Ready
    - Rework
  terminal_states:
    - Done
    - Canceled
polling:
  interval_ms: 5000
---
Run mock task {{ task.id }} for {{ repo.owner }}/{{ repo.name }}.
