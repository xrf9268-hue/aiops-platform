---
agent:
  default: mock
  timeout: 5m
policy:
  mode: draft_pr
  max_changed_files: 12
  max_changed_loc: 300
verify:
  commands: []
pr:
  draft: true
  labels:
    - ai-generated
    - needs-review
---
Run mock task {{ task.id }} for {{ repo.owner }}/{{ repo.name }}.
