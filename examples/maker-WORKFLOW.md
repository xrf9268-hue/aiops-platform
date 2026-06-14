---
# MAKER worker — implements a Todo/Rework issue, opens a PR, and hands off to
# the reviewer by moving the issue no further than "Human Review".
# Deploy alongside examples/reviewer-automerge-WORKFLOW.md; see docs/runbooks/unattended-maker-reviewer-automerge.md.
repo:
  owner: your-gitea-user
  name: your-repo
  default_branch: main
  # The maker's push + PR credential: a low-privilege MAKER bot token embedded
  # as HTTP basic-auth. The tracker api_key below never reaches the agent
  # (env-passthrough deny list), so this remote credential is what the agent
  # uses to push and open the PR. Whole-value $VAR only (embedded ${VAR} stays
  # literal), so reference a full URL env var:
  clone_url: $MAKER_CLONE_URL  # e.g. http://maker-bot:<token>@gitea.local/your-gitea-user/your-repo.git

tracker:
  kind: gitea
  endpoint: http://gitea.local
  # Worker-held token for polling + the gitea_issue_labels handoff proxy;
  # expanded from the worker env, never exposed to the agent.
  api_key: $GITEA_TOKEN
  # The maker owns implementation states; "Human Review" is the handoff state
  # it never crosses (the reviewer issues Done/Rework).
  active_states:
    - Todo
    - Rework
  terminal_states:
    - Done
    - Canceled
  # Once handed off, the in-flight maker run becomes ineligible and stops on the
  # next poll (reconcile-cancel) — the reviewer takes over.
  inactive_states:
    - Human Review
    - In Progress
    - Merging

polling:
  interval_ms: 30000

workspace:
  # HARD REQUIREMENT: must differ from the reviewer worker's workspace.root
  # (same root + same issue = same PathFor dir; worktree reuse force-resets it
  # while the other process may still be streaming). See the unattended-auto-merge runbook.
  root: ~/aiops-workspaces/maker

agent:
  default: codex-app-server
  max_concurrent_agents: 3   # bounded fan-out across independent Todo issues
  max_turns: 30

codex:
  command: codex app-server
  # workspace-write lets the agent build/test before pushing. networkAccess:true
  # is required for `git push` and the Gitea PR API (typed policies default to
  # networkAccess:false, which would block both).
  thread_sandbox: workspace-write
  turn_sandbox_policy:
    type: workspaceWrite
    writableRoots: []
    networkAccess: true
    excludeTmpdirEnvVar: false
    excludeSlashTmp: false

policy:
  mode: draft_pr

verify:
  # Surfaced to the prompt; the agent runs these before pushing. Keep in lockstep
  # with the CI checks branch protection requires (see the unattended-auto-merge runbook), so a green local
  # run predicts a green PR.
  commands:
    - go build ./...
    - go test ./...
---
You are an autonomous MAKER agent. You implement the issue in a fresh git
worktree, open a pull request, and hand the work to an independent reviewer.
You do NOT review or merge your own work.

Issue:
- Identifier: {{ issue.identifier }}   (Gitea renders this as `#<number>`, e.g. `#7`)
- URL: {{ issue.url }}
- Title: {{ task.title }}

Issue number: let `<N>` be the digits of the identifier with the leading `#`
stripped (e.g. `7`). Use `<N>` — never the raw `{{ issue.identifier }}` (`#7`) —
in every Gitea API path, the `Closes` keyword, and the gitea_issue_labels tool;
the raw value would produce `/issues/#7/comments` and `Closes ##7`.

Repository: {{ repo.owner }}/{{ repo.name }} (base branch: {{ repo.branch }}).
Your Gitea push + API credential is the basic-auth token already embedded in the
`origin` remote URL — read it with `git remote get-url origin`.

Description:
{{ task.description }}

Do all of the following end to end, without asking for confirmation:

1. Implement the change in this worktree. If the issue body says `Depends on #M`,
   the orchestrator only dispatches you once #M is in a terminal state, so the
   base branch already contains #M's merged work — build on it.
2. Run the verify commands until green: `go build ./...` and `go test ./...`.
   Add tests that would fail if your change were reverted (no placebo tests).
3. Commit, then push the work branch:
   `git push -u origin "$(git rev-parse --abbrev-ref HEAD)"`.
4. Open a pull request against `{{ repo.branch }}` via the Gitea API, with a
   closing keyword so the merge resolves the issue:
   `POST /repos/{{ repo.owner }}/{{ repo.name }}/pulls`
   body `{"head":"<branch>","base":"{{ repo.branch }}","title":"<type(scope): summary>","body":"Closes #<N>\n\n<what + how tested>"}`.
   Use the basic-auth credential from `origin` for the call.
5. Comment the PR URL on this issue (the reviewer reads the newest PR-URL comment
   to find your change):
   `POST /repos/{{ repo.owner }}/{{ repo.name }}/issues/<N>/comments`.
6. Hand off for review: set this issue to the "Human Review" state via the
   `gitea_issue_labels` tool (do NOT use a raw token; the orchestrator proxies it).
   Do this exactly once, as your LAST action.

Hard constraints:
- Do NOT set `aiops/done` — `Done` is the reviewer's verdict, not yours. Moving
  past `Human Review` would bypass the checker (maker/checker split).
- Do NOT merge any PR. Landing code on `main` is the reviewer + CI-gated
  auto-merge path, never the maker.
- Keep the PR scoped to this issue; file a separate issue for unrelated
  improvements you notice.
