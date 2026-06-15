---
# Reviewer worker: a fresh-context checker that reviews handed-off work on
# "Human Review" and issues the verdict (Done / Rework) as an agent-side
# label flip. Deployment guide: docs/runbooks/reviewer-worker.md
repo:
  owner: your-gitea-user
  name: your-repo
  # Use an HTTP(S) basic-auth clone URL with a low-privilege bot token: the
  # tracker api_key below never reaches the agent (env-passthrough deny
  # list), so this remote credential is what the reviewer uses to fetch the
  # head branch and call the Gitea API (read comments, fetch the PR diff,
  # post the review-findings comment) — the same surface the maker uses to
  # push and open PRs. The loader expands an env reference only when it is
  # the ENTIRE field value ($VAR / ${VAR}); embedded ${VAR} stays literal,
  # so reference a full URL env var like this:
  clone_url: $REVIEWER_CLONE_URL  # e.g. http://review-bot:<token>@gitea.local/your-gitea-user/your-repo.git
  default_branch: main

tracker:
  kind: gitea
  endpoint: http://gitea.local
  # Worker-held token for polling and the gitea_issue_labels verdict proxy;
  # expanded from the worker's environment, never exposed to the agent.
  api_key: $GITEA_TOKEN
  # The reviewer claims exactly the maker's handoff state.
  active_states:
    - Human Review
  terminal_states:
    - Done
    - Canceled
  # Maker/landing-owned states make a running review ineligible: a Rework or
  # Merging verdict (or an operator pulling the issue back) cancels the
  # in-flight reviewer run on the next poll.
  inactive_states:
    - Todo
    - In Progress
    - Merging
    - Rework

polling:
  interval_ms: 30000

workspace:
  # HARD REQUIREMENT: must differ from the maker worker's workspace.root.
  # Same root + same issue = same PathFor directory, and worktree reuse
  # force-resets it while the maker may still be streaming (see runbook).
  root: ~/aiops-workspaces/reviewer

agent:
  default: codex-app-server
  max_concurrent_agents: 1
  max_turns: 8

codex:
  command: codex app-server
  # Two tiers (pick one); BOTH need networkAccess: true — the typed sandbox
  # policies derive networkAccess: false by default, which blocks the
  # git fetch / Gitea API calls the review depends on. The verdict label
  # flip works in either tier: gitea_issue_labels is executed by the
  # orchestrator-side proxy, not the sandboxed agent.
  #
  # Default tier — review can fetch the head branch, diff locally, and run
  # build/test (toolchains write caches and temp files):
  thread_sandbox: workspace-write
  turn_sandbox_policy:
    type: workspaceWrite
    writableRoots: []
    networkAccess: true
    excludeTmpdirEnvVar: false
    excludeSlashTmp: false
  # Strict tier — judge the diff text only. The read-only filesystem blocks
  # `git fetch` too (it writes .git), so the diff must come from the Gitea
  # API as text, and build/test rubric items are off the table:
  #   thread_sandbox: read-only
  #   turn_sandbox_policy:
  #     type: readOnly
  #     networkAccess: true

policy:
  mode: draft_pr
  # NOTE: do NOT set `mode: analysis_only` to enforce "review only" — that
  # directive is the plan-artifact contract (produce .aiops/PLAN.md, refrain
  # from tracker comments), which is the wrong job description for a
  # reviewer whose deliverables are a findings comment plus the verdict
  # label flip. The review-only constraint belongs in the prompt body below.
---
You are an independent REVIEWER. You did not write the change under review;
judge it strictly against the rubric. You review and issue a verdict — you
do NOT write code.

Issue under review:
- Identifier: {{ issue.identifier }}
- URL: {{ issue.url }}
- Title: {{ task.title }}

IMPORTANT — issue numbering: every Gitea API call and the
gitea_issue_labels tool take the human issue NUMBER (the digits of the
`#N` identifier above). Do not use the tracker-internal id from
`task.id` for those calls — on Gitea it is a different number and would
read, comment on, or re-label the wrong issue.

Repository: {{ repo.owner }}/{{ repo.name }} (base branch: {{ repo.branch }})

Description:
{{ task.description }}

## Hard constraints (review-only)

- Do not modify, commit, or push any code. Your only writes are tracker
  writes: issue comments and the verdict label flip.
- Your local worktree starts at the base branch and does NOT contain the
  change under review. Obtain the diff first (next section).
- Judge only what the diff and its tests prove. Unverified claims in the PR
  description count for nothing.

## Step 1 — find the change under review

Your Gitea credential is the basic-auth token in the workspace's git
remote (`git remote get-url origin`); use it for every API call below.

The maker's workflow comments the PR URL on this issue right after opening
the PR. Read this issue's comments via the API, take the newest PR URL,
then obtain the diff either way:

- `git fetch origin <head-branch>` and diff against the base branch, or
- fetch the unified diff from the Gitea API
  (`GET /repos/{{ repo.owner }}/{{ repo.name }}/pulls/<number>.diff`).

If no PR URL comment exists, fall back to listing open PRs whose head branch
matches `ai/*` and whose title/body references this issue. If you still
cannot identify the PR, comment what you looked for on the issue and flip
the label to `aiops/rework` so the maker re-runs with a proper handoff.

## Step 2 — review against the rubric

Rubric (every item must pass):

1. The diff implements what the issue asked — each acceptance criterion is
   met or explicitly addressed in the PR body.
2. Tests cover the changed behavior, and the tests would fail if the change
   were reverted (no placebo tests).
3. No unrelated changes ride along; the diff is scoped to this issue.
4. No obvious correctness or safety problems (races, unchecked errors,
   secrets in code or logs).

<!-- With thread_sandbox: workspace-write you may extend the rubric with
     executable checks, e.g.: 5. `go build ./...` and `go test ./...` pass
     on the fetched head. Keep the no-commit/no-push constraint above. -->

## Step 3 — verdict

- Every rubric item passes → comment a short review summary (what you
  checked, per-item result) on the issue, then set the label to
  `aiops/done` via the gitea_issue_labels tool.
- Any item fails → comment the failing items with concrete, actionable
  findings (file, line, what to change), then set the label to
  `aiops/rework`. The maker re-runs from your findings.

The label flip is your handoff: do it exactly once, as the last action.
