---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
  default_branch: main

tracker:
  kind: github
  api_key: $GITHUB_TOKEN
  # GitHub tracker states map to issue labels unless the state is open/closed/all.
  # This queue processes priority-labeled open issues first, then any remaining
  # open issues that have not been triaged with a priority label yet.
  active_states:
    - priority:p1
    - priority:p2
    - priority:p3
    - open
  terminal_states:
    - closed
  inactive_states: []

polling:
  interval_ms: 60000

workspace:
  root: ~/aiops-workspaces/github/xrf9268-hue-aiops-platform

agent:
  default: codex
  max_concurrent_agents: 2
  max_concurrent_agents_by_state:
    priority:p1: 1
    priority:p2: 1
    priority:p3: 1
    open: 1
  max_turns: 12
  timeout: 2h
  max_retry_attempts: 1
  max_timeout_retries: 1

codex:
  command: codex exec
  # Full access is intended for a trusted local Mac or an already-isolated Docker
  # worker. Use safe on shared hosts.
  profile: bypass

claude:
  command: claude -p --permission-mode bypassPermissions --output-format text --max-turns 20

policy:
  mode: draft_pr
  deny_paths:
    - .env
    - .env.*
    - secrets/**
  max_changed_files: 20
  max_changed_loc: 600

safety:
  allowed_networks:
    - github.com
    - api.github.com
    - configured language package registries needed by tests
  allowed_paths:
    - repository workspace for this task
    - local tool caches that do not contain shared credentials
  allowed_commands:
    - repository build, test, lint, and formatting commands
    - git commands needed to commit and push the work branch
    - gh commands needed to open and update PRs
    - codex and claude local review commands
  forbidden:
    - exposing tokens, API keys, or local credential files in logs, PR bodies, or comments
    - changing secrets or personal credential files

sandbox:
  enabled: false
  backend: none
  network: none

verify:
  timeout: 30m
  commands:
    - test -z "$(gofmt -l $(git ls-files '*.go'))"
    - go mod tidy && git diff --exit-code -- go.mod go.sum
    - go test -race -covermode=atomic ./...
    - go build ./cmd/worker ./cmd/linear-poller ./cmd/gitea-poller

pr:
  draft: true
  labels:
    - ai-generated
    - needs-review
---
You are autonomously resolving one GitHub issue in github.com/xrf9268-hue/aiops-platform.

Hard requirements:
- Work only on the assigned issue. Do not opportunistically refactor unrelated areas.
- Read AGENTS.md, README.md, the issue text, and the relevant SPEC/reference paths before design-sensitive changes.
- Use a focused failing test before production code changes when adding behavior or fixing bugs.
- Keep changes small enough for review. Respect the configured policy limits unless the issue explicitly requires a larger change.
- Run the configured verify commands before pushing.
- Before opening or updating a PR, run two independent local reviews of the
  final diff: Codex review and Claude Code review. Both are mandatory gates.
- Use machine-validated JSON for both local reviews. The expected shape is:
  `{"blocking_findings":[{"severity":"high|medium|low","file":"path","line":1,"issue":"text"}]}`.
- For Codex, use `codex exec --ephemeral --output-schema <schema-file> -` with
  the prompt and diff on stdin. Do not combine `codex exec review --base
  <branch>` with a custom prompt; that CLI mode treats `--base` and `PROMPT` as
  mutually exclusive.
- For Claude Code, use `claude -p --permission-mode bypassPermissions
  --no-session-persistence --tools "" --output-format json
  --json-schema '<schema-json>'
  --max-turns 6` and feed the complete review prompt plus diff on stdin. Claude
  must review only the supplied diff for this gate. The higher turn budget is
  still bounded by the outer review timeout and prevents structured-output
  retries from failing the gate before Claude can emit schema-valid JSON. Read
  `.structured_output` from Claude's JSON wrapper as the review JSON.
- Treat non-JSON output, command failure, or any non-empty
  `blocking_findings` list from either local reviewer as blocking. Fix findings
  with tests before push.
- Before opening a PR, check for an existing open PR that closes the assigned
  issue (`gh pr list --state open --search "#<issue-number>"` plus a direct
  PR-body/linked-issue check). If one exists, update and reuse that PR and
  branch; do not open a duplicate PR for the same issue.
- Open or update a draft PR with a body that names the issue, summarizes tests,
  and records both local review results.
- The PR body must include an explicit issue claim line for the assigned GitHub
  issue, preferably `Closes #<issue-number>`; `Issue #<issue-number>` is also
  recognized by the local tracker as an active PR claim. Do not use only casual
  references such as `See also #...` for the assigned issue.
- Do not post ad hoc `@codex review` comments yourself. Leave the PR for the
  follow-through automation, which posts or reuses a current-head-bound GitHub
  Codex review trigger and merges only after CI and review gates are clean.
