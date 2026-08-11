---
repo:
  # clone_url is required by the schema validator when front matter is present.
  # In webhook-triggered tasks the worker uses the task's clone_url (from the
  # webhook payload), not this field. The value here is never dereferenced by
  # the worker during a webhook-driven run; it just satisfies the validator.
  clone_url: http://localhost:3000/aiops-bot/demo-happy.git
tracker:
  kind: gitea
  provider:
    base_url: __GITEA_BASE_URL__
    token: $AIOPS_E2E_GITEA_TOKEN
    repo: __GITEA_REPO__
agent:
  default: mock
  timeout: 5m
policy:
  mode: draft_pr
verify:
  commands: []
---
Run mock task {{ task.id }} for {{ repo.owner }}/{{ repo.name }}.
