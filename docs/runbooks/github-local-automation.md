# GitHub local automation

This runbook wires the local macOS operator flow for resolving
`xrf9268-hue/aiops-platform` GitHub issues with the worker-owned polling path.

## Model

- `cmd/worker` is the long-running issue execution engine.
- The worker uses the orchestrator actor's in-process claim set to keep a
  tracker issue from being dispatched twice before a PR exists; the shell
  wrapper adds a workflow/workspace singleton lock so stale local processes do
  not create a second dispatcher.
- `examples/github-local-WORKFLOW.md` selects open GitHub issues by priority
  labels (`priority:p1`, `priority:p2`, `priority:p3`) first, then remaining
  open issues that have not been priority-triaged yet.
- Each issue runs in a deterministic per-issue workspace under
  `~/aiops-workspaces/github/xrf9268-hue-aiops-platform`.
- The GitHub tracker skips open issues that are already named by an open PR's
  explicit issue claim (`Closes #...`, `Fixes #...`, `Resolves #...`, or
  `Issue #...`). This keeps a worker restart or retry from dispatching a second
  agent for an issue whose PR is already in review, without suppressing casual
  `see also #...` mentions.
- Clean worker exits are bounded by `agent.max_turns`. If an issue remains in an
  active tracker state after that many clean turns, the orchestrator records a
  non-retryable failure for the unchanged issue instead of dispatching another
  full agent run.
- Failure-driven worker retries are bounded by `agent.max_retry_attempts`
  (default in the local workflow: `1`, meaning first run plus one retry).
  Runner-timeout retries remain separately bounded by
  `agent.max_timeout_retries`.
- Codex is the primary implementer and is configured for full access through
  `codex.profile: bypass`.
- Codex review and Claude Code are both mandatory independent local diff
  reviewers before PR handoff.
- `scripts/local-pr-follow-through.sh` serializes PR gates and auto-merge:
  local Go gates, independent Codex and Claude Code diff reviews, all GitHub checks,
  unresolved review-thread check, then `gh pr merge --squash --auto`.
- The local review commands intentionally use structured-output modes:
  Codex runs `codex exec --output-schema <schema-file> -`, and Claude Code runs
  `claude -p --tools "" --output-format json --json-schema '<schema-json>'`
  and reads `.structured_output`. `codex exec review --base` is not used for
  this gate because the Codex CLI treats `--base` and custom review prompt
  arguments as mutually exclusive. Claude Code receives the complete diff on
  stdin and intentionally has no repository tools in this gate, so the
  unattended run stays bounded to a structured-output review task. Review
  sessions use Codex `--ephemeral` and Claude Code
  `--no-session-persistence` to avoid retaining short-lived reviewer history.

## Prerequisites

- `gh auth status -h github.com` must show an account with `repo` and workflow
  access to `xrf9268-hue/aiops-platform`.
- `codex`, `claude`, `go`, and `gh` must be on PATH.
- GNU `timeout` must be on PATH as `timeout` or `gtimeout` (for example from
  Homebrew `coreutils` on macOS), or `AIOPS_TIMEOUT_BIN` must point to an
  equivalent binary. The follow-through script fails closed without it so
  reviewer and GitHub API calls cannot run unbounded.
- `GITHUB_TOKEN` may be exported explicitly; otherwise the scripts read it via
  `gh auth token -h github.com`.

## One-shot local worker

```bash
scripts/local-github-worker.sh
```

This builds `cmd/worker` into
`~/Library/Application Support/aiops-platform/bin/worker` and runs it with
`examples/github-local-WORKFLOW.md`.

## One-shot PR follow-through

```bash
scripts/local-pr-follow-through.sh
```

By default this processes open PRs labeled `ai-generated`. Pass PR numbers to
scope the run:

```bash
scripts/local-pr-follow-through.sh 173
```

Set `AIOPS_AUTO_MERGE=0` to run all gates without merging.

The script uses a dedicated checkout at
`~/aiops-workspaces/github/xrf9268-hue-aiops-platform-pr-follow-through` so PR
checkout/reset/clean operations do not touch the automation source worktree.
On macOS, the Go test/build gate defaults to Docker with cached Go module/build
volumes so it matches the Linux CI environment. Set `AIOPS_GATE_MODE=local` to
force host-native gates, or `AIOPS_GATE_MODE=docker` to force Docker everywhere.
After the local Go gate plus Claude Code and Codex diff reviews pass, the script
marks draft PRs ready, ensures a GitHub Codex review trigger exists for the
current head commit and base branch commit, waits for that trigger to finish, and re-checks review
threads before watching CI and merging. Local independent review results are
cached by PR number, head SHA, base SHA, and base branch name, so a 10-minute sweep does not
spend reviewer tokens again for an unchanged effective diff.
Failed local review results are cached too; the follow-through script blocks
the same head until the PR branch changes or an operator deletes the matching
state file after inspection. A failed local review cache hit is checked before
local Go gates, so an unchanged rejected PR does not rerun expensive gates or
block the rest of the sweep. Before using a cached review, remote review
trigger, GitHub check result, or merge decision, the script re-reads the PR
head and aborts the sweep if the branch changed under it.
If a previous follow-through sweep already posted an `@codex review` trigger
whose body includes the current head SHA, follow-through records and reuses
that existing comment only when it also includes the current base SHA and base
branch name instead of posting another one. Plain ad hoc
`@codex review` comments are not reused because they are not tied to a reviewed
head. A sweep also fails before running expensive gates when two open PRs claim
the same issue, even if one of the PRs does not carry the automation label;
close or retarget the superseded PR first.
The open-PR duplicate scan uses `AIOPS_PR_SCAN_LIMIT` (default: 1000) and fails
closed if the result count reaches that cap, because a truncated PR list is not
safe enough for unattended review or merge decisions.
Every PR selected for follow-through must include an explicit issue claim such
as `Closes #...` or `Issue #...` before the script will spend gate or review
tokens on it.

## Audit and state files

Unattended follow-through logs are written as timestamped key/value audit lines
in `~/Library/Logs/aiops-platform/github-pr-follow-through.out.log`, for
example:

```text
2026-05-21T09:00:00Z component=github-pr-follow-through event=local_reviews_cache_hit pr=182 head=<sha> base=<sha> base_ref=main
```

Important follow-through events include `lock_acquired`, `lock_busy`,
`pr_started`, `head_changed`, `local_gates_started`, `local_gates_passed`,
`local_reviews_started`, `local_reviews_cached`,
`local_reviews_failed_cached`, `local_reviews_failed_cache_hit`,
`github_codex_review_triggered`, `github_codex_review_existing_trigger_found`,
`github_codex_review_reused`, `duplicate_prs_detected`,
`missing_pr_issue_claim`, `pr_scan_limit_reached`, `checkout_head_mismatch`,
`pr_refs_changed`, `base_fetch_mismatch`, `github_checks_started`,
`merge_requested`, `merge_skipped`, and `pr_completed`.

The worker launch script also uses a workflow/workspace keyed singleton lock at
`~/Library/Caches/aiops-platform/github-worker-<hash>.lock`, so a stale manual
worker or old LaunchAgent cannot run a second dispatcher against the same
tracker/workspace pair. If the lock directory exists before its pid file is
written, a second worker treats the lock as initializing and exits busy instead
of deleting it; stale pid-less worker locks age out after
`AIOPS_WORKER_LOCK_STALE_SECONDS` (default: 3600).

The local state roots are:

- `~/Library/Caches/aiops-platform/github-worker-<hash>.lock` — singleton lock
  for the worker process keyed by workflow path plus workspace root. Recent
  pid-less locks are treated as initializing; stale pid-less locks age out after
  `AIOPS_WORKER_LOCK_STALE_SECONDS`.
- `~/Library/Caches/aiops-platform/pr-follow-through.lock` — atomic lock that
  prevents overlapping LaunchAgent sweeps. If a lock exists without a pid while
  a process is initializing, a second sweep exits as busy instead of deleting
  the lock; genuinely stale pid-less locks age out after
  `AIOPS_FOLLOW_THROUGH_LOCK_STALE_SECONDS` (default: 3600).
- `~/Library/Caches/aiops-platform/reviews` — local Claude Code plus Codex
  review terminal records keyed by repo, PR number, head SHA, base SHA, and base
  branch name. A `failed` record is intentional token protection; delete it only after
  auditing the saved reviewer JSON, raw output, stdout, and prompts under the
  sibling `.artifacts` directory and deciding the exact same diff should be
  reviewed again.
- `~/Library/Caches/aiops-platform/github-codex-review` — GitHub `@codex
  review` trigger comment records keyed by repo, PR number, head SHA, base SHA,
  and base branch name. A cached trigger is reused only if the referenced
  comment still contains all three current refs.
- `~/aiops-workspaces/github/xrf9268-hue-aiops-platform/.aiops-policy-feedback`
  — worker policy feedback records. A first policy rejection is fed into the
  next prompt; the second repeated policy rejection for the same issue becomes
  non-retryable to prevent broad diffs from burning another full agent run.
  Read or write errors for these files are also non-retryable, because failing
  open would disable the stop-after counter.

GitHub tracker pagination is fail-closed: if issue or open-PR pagination
exceeds the configured scan cap, the poll returns an error instead of acting on
a truncated issue set.

Per-tick state refresh (SPEC §8.5 Part B) for already-running GitHub issues
issues one `GET /repos/{owner}/{repo}/issues/{number}` per running issue,
sequentially, in poll order. The repo issue number is taken from the cache
populated by the active-issue list. Per-ID `404`/`410` responses are treated as
"issue removed" and silently skipped so a single deleted issue does not abort
reconciliation for the rest of the running set; other HTTP errors abort the
refresh so a transient outage cannot silently degrade reconciliation. At the
default GitHub primary REST rate limit (5,000 req/hour for a personal access
token) this is comfortably within budget for tens of concurrent running issues
on the default poll cadence; cut the worker count or extend the poll interval
if the workload approaches that ceiling.

## Install unattended macOS LaunchAgents

```bash
scripts/install-local-launchagents.sh
```

This installs:

- `com.aiops-platform.github-worker`: long-running worker, restarted by
  launchd if it exits.
- `com.aiops-platform.github-pr-follow-through`: PR gate/auto-merge sweep every
  10 minutes. It installs with `AIOPS_AUTO_MERGE=1` by default, so it merges
  only after the full local Go gate, Claude Code review, Codex review, GitHub
  checks, and review-thread gates pass. Install with `AIOPS_AUTO_MERGE=0` for a
  no-merge dry run.

Logs are written under `~/Library/Logs/aiops-platform/`.

To stop and uninstall the local services:

```bash
scripts/uninstall-local-launchagents.sh
```

## Operational guardrails

- Do not run more than one worker service against the same workflow/workspace
  root.
- If a duplicate PR is ever created, pause both LaunchAgents, close the
  superseded PR, and inspect the open PR bodies plus linked issue state before
  restarting. The expected steady state is one open PR per active issue.
- Keep branch protection and required CI enabled on `main`.
- Treat a non-JSON or non-empty Claude local review as blocking.
- Treat a non-JSON or non-empty Codex local review as blocking.
- Treat a local reviewer timeout as blocking. Increase `AIOPS_REVIEW_TIMEOUT`
  only after checking the reviewer logs and confirming it is slow rather than
  stuck.
- `AIOPS_GH_TIMEOUT` is for short GitHub API calls only. The long CI watch uses
  `AIOPS_CHECKS_TIMEOUT` (default: 30m), so normal CI duration does not cause
  repeated follow-through sweeps.
- `AIOPS_PR_SCAN_LIMIT` bounds open-PR scans for duplicate issue claims. If the
  number of returned PRs reaches the limit, the sweep stops before local gates
  or reviews because the duplicate-PR guard may be incomplete.
- Claude Code review defaults to `AIOPS_CLAUDE_REVIEW_MAX_TURNS=6`, disables
  tools with `--tools ""`, and uses `--output-format json` with
  `--json-schema`; the gate extracts `.structured_output` from Claude's JSON
  wrapper as the review JSON. It reviews only the supplied diff; re-enabling
  repository tools can make unattended runs hit turn limits before returning
  JSON.
- Treat unresolved non-outdated GitHub review threads as blocking. Follow-through
  paginates review threads until `hasNextPage=false`; checking only the first
  100 threads is not sufficient for merge.
- Ensure a GitHub `@codex review` trigger exists after any PR branch update and
  wait for that trigger to finish on the current head and base before merging.
  Reuse an existing trigger only when the comment body contains the current head
  SHA, base SHA, and base branch name.
  The completion gate requires a `+1` reaction from
  `chatgpt-codex-connector` on that head/base-bound trigger and no active `eyes`
  reaction; unrelated/manual reactions do not satisfy the gate.
- Merge uses `gh pr merge --match-head-commit <reviewed-head>` so GitHub rejects
  auto-merge setup if the PR branch changes between the last local check and the
  merge request.
- Use `AIOPS_AUTO_MERGE=0` during new workflow changes or after changing local
  credentials. Restore `AIOPS_AUTO_MERGE=1` after the no-merge sweep logs are
  clean so unattended follow-through can merge PRs that pass all gates.
- Stale per-issue worktrees live under
  `~/aiops-workspaces/github/xrf9268-hue-aiops-platform`; remove issue
  directories only after confirming the worker is stopped or the issue/PR is no
  longer active.
- The PR follow-through checkout lives under
  `~/aiops-workspaces/github/xrf9268-hue-aiops-platform-pr-follow-through` and
  is reset/cleaned on every PR gate run.
- The bare mirror cache under
  `~/Library/Caches/aiops-platform/mirrors/github.com/xrf9268-hue/aiops-platform.git`
  is intentionally retained for speed. Delete it only for a full reset.
- Docker gate caches live under `~/Library/Caches/aiops-platform/go-build` and
  `~/Library/Caches/aiops-platform/go-mod`; keep them for speed, or delete them
  for a full cache reset.
- When the optional Docker gate is enabled, it writes the reusable tag
  `aiops-platform:local-gate` by default instead of leaving dangling images.
