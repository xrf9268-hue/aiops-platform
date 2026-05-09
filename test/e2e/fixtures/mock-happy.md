---
repo:
  # clone_url is required by the schema validator when front matter is present.
  # In webhook-triggered tasks the worker uses the task's clone_url (from the
  # webhook payload), not this field. The value here is never dereferenced by
  # the worker during a webhook-driven run; it just satisfies the validator.
  clone_url: http://localhost:3000/aiops-bot/demo-happy.git
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
