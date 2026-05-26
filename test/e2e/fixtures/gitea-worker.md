---
repo:
  clone_url: http://localhost:3000/aiops-bot/demo-gitea-tracker.git
agent:
  default: mock
  timeout: 5m
policy:
  mode: draft_pr
  max_changed_files: 12
  max_changed_loc: 300
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
pr:
  draft: true
  labels:
    - ai-generated
    - needs-review
---
Run mock task {{ task.id }} for {{ repo.owner }}/{{ repo.name }}.
