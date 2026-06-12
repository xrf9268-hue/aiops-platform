---
repo:
  # clone_url is required by the schema validator when front matter is present.
  # The test rewrites this placeholder to the live Gitea container clone URL
  # before loading the service workflow (same pattern as mock-happy.md).
  clone_url: http://localhost:3000/aiops-bot/demo-scripted-agent.git
tracker:
  kind: gitea
  active_states:
    - Todo
  terminal_states:
    - Done
    - Canceled
agent:
  default: claude
  timeout: 5m
claude:
  # The test rewrites this placeholder to the absolute path of the generated
  # scripted-agent shell script (the script path is only known at test
  # runtime, so it cannot be checked in).
  command: __SCRIPTED_AGENT_COMMAND__
policy:
  mode: draft_pr
verify:
  commands: []
---
Scripted agent task {{ task.id }} for {{ repo.owner }}/{{ repo.name }}.
