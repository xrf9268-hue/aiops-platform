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
  # This dogfood queue processes only issues explicitly labeled aiops:ready.
  # Priority labels are triage metadata, not permission to run an issue.
  # Optional: raise this for very large repositories. A cap hit skips only the
  # overflowing state/label scan and logs the diagnostic.
  # pagination_max_pages: 25
  active_states:
    - aiops:ready
  terminal_states:
    - closed
  inactive_states: []

polling:
  interval_ms: 60000

workspace:
  root: ~/aiops-workspaces/github/xrf9268-hue-aiops-platform

agent:
  default: codex-app-server
  max_concurrent_agents: 2
  max_concurrent_agents_by_state:
    aiops:ready: 2
  max_turns: 12
  timeout: 2h
  # Failure retries are unbounded per SPEC §8.4 / §16.6 (bounded only by the
  # 5-minute backoff ceiling), and keep going until the tracker takes the issue
  # out of active work. The `agent.max_retry_attempts` / `agent.max_timeout_retries`
  # opt-in caps were removed in #577 (rejected at load); to stop a persistently
  # failing issue, move it out of the active states. See DEVIATIONS.md D29.

codex:
  # The SPEC §10 runner is the long-running `codex app-server` JSON-RPC session.
  command: codex app-server
  # thread_sandbox: danger-full-access is intended for a trusted local Mac or an
  # already-isolated Docker worker. Use workspace-write on shared hosts.
  thread_sandbox: danger-full-access

claude:
  command: claude -p --permission-mode bypassPermissions --output-format text --max-turns 20

policy:
  mode: draft_pr
  # Scope and path rules belong in the prompt body (SPEC §3.2); hard path
  # prevention belongs to `sandbox:` write restrictions. The worker path/diffstat
  # gate was removed in #561 — `deny_paths` / `max_changed_*` are not accepted.

# The agent's safety envelope (allowed networks/paths/commands, forbidden
# actions) is expressed in the prompt body below (SPEC §3.2). The descriptive
# `safety:` front-matter block was removed in #578 (it enforced nothing).

sandbox:
  enabled: false
  backend: none
  network: none

verify:
  commands:
    - test -z "$(gofmt -l $(git ls-files '*.go'))"
    - go mod tidy && git diff --exit-code -- go.mod go.sum
    - go test -run '^TestProductionGoFilesStayWithinSizeBudget$' -count=1 ./scripts
    - go test -race -covermode=atomic ./...
    - go build ./cmd/worker ./cmd/tui

# PR handoff (draft state, labels, reviewers) is the agent's responsibility via
# its tool surface (SPEC §1, #76) — see the prompt below, which tells the agent
# to open a draft PR. The `pr:` front-matter block was removed in #578.
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
