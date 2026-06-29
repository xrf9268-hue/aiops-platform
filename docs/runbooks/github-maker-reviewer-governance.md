# GitHub maker/reviewer governance

This guide is the short production setup checklist for running aiops-platform
against GitHub with separate maker and reviewer workers. Read it before the
long [GitHub maker/reviewer auto-merge E2E](github-maker-reviewer-automerge-e2e.md)
runbook; the E2E runbook remains the deep release-validation script and evidence
reference.

## Worker/orchestrator boundary

The worker/orchestrator schedules, prepares workspaces, runs agents, polls
GitHub, observes state, and reconciles active runs. It does not own PR
operations. In this topology, workers do not create PRs, approve PRs, merge PRs, close issues, or mark issues done on their own.

Those operations belong to the agents and GitHub:

- The maker agent implements the issue, verifies it, pushes a branch, opens or
  updates a PR, comments the PR URL on the issue, and hands off with
  `aiops:human-review`.
- The reviewer agent independently reviews the PR head, requests Rework or
  approves, enables GitHub native auto-merge, waits for GitHub to report the PR
  merged, then marks the issue `aiops:done` and closes it.
- GitHub branch protection and Actions decide whether the approved PR can land.

Do not add a worker/orchestrator phase, gate, config key, artifact, merge
shortcut, or issue-closing shortcut to make this topology work. The governance
surface is the workflow prompt, credentials, branch protection, and evidence
capture.

## Production topology

Use distinct GitHub identities for every role:

- setup/operator: creates or configures the repository, labels, Actions, branch
  protection, and evidence capture.
- maker: writes branches and PRs, but never reviews, approves, merges, closes,
  or adds `aiops:done`.
- reviewer: reviews PRs, enables auto-merge on passed heads, confirms merge,
  then closes issues, but never edits, commits, or pushes code.

Use distinct credential homes and runtime roots:

- distinct `GH_CONFIG_DIR` for setup/operator, maker, and reviewer;
- `env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR=<role-dir> gh ...` for
  role-specific `gh` commands, because ambient token variables take precedence
  over stored `GH_CONFIG_DIR` credentials;
- distinct maker and reviewer `workspace.root` values in the selected workflow
  files;
- distinct maker and reviewer `AIOPS_MIRROR_ROOT` values;
- `AIOPS_EXPECTED_GITHUB_LOGIN` set to the expected role login for each worker;
- no `GITHUB_TOKEN` or `GH_TOKEN` passed through to the agent environment.

The reusable source templates are:

- [`examples/github-maker-WORKFLOW.md`](../../examples/github-maker-WORKFLOW.md)
- [`examples/github-reviewer-automerge-WORKFLOW.md`](../../examples/github-reviewer-automerge-WORKFLOW.md)

Keep those files as the source templates. Copy them into each deployment or run
root, fill in repository values and role-specific roots, and validate the
rendered workflows with `worker --print-config` or `worker --doctor`.

## Label state machine

Create and use these GitHub labels:

| Label | Owner | Meaning |
| --- | --- | --- |
| `aiops:todo` | setup/operator | Ready for the maker to claim. |
| `aiops:rework` | reviewer | Returned to the maker after a failed review. |
| `aiops:human-review` | maker | Maker has handed off a PR URL for reviewer work. |
| `aiops:done` | reviewer | PR merge has been confirmed and the issue is closing. |
| `aiops:canceled` | setup/operator | Work should stop and active runs should reconcile away. |

The normal flow is:

```text
aiops:todo or aiops:rework
  -> maker implements, verifies, pushes, opens PR, comments PR URL
  -> aiops:human-review
  -> reviewer checks PR head
       -> aiops:rework on FAIL
       -> reviewer approval + GitHub native auto-merge on PASS
       -> GitHub reports merged
       -> aiops:done + issue close
```

For dependent issues, do not add `aiops:todo` until the prerequisite issues are
`aiops:done` and closed.

## Branch protection and auto-merge

Configure GitHub so repository policy, not the worker, is the merge gate:

- require at least one required status check, such as `build-test`;
- require one required approving review;
- enable stale-review dismissal or require approval of the latest push when the
  repository policy supports it;
- disallow force pushes and direct pushes to `main`;
- enable squash-only merging and GitHub native auto-merge;
- ensure the reviewer identity is different from the maker identity, because PR
  authors cannot satisfy their own approval requirement.

The reviewer should enable auto-merge with the reviewed head pinned, for
example:

```bash
gh pr merge <PR> --auto --squash --delete-branch --match-head-commit <sha>
```

Do not use `--admin` and do not mark the issue Done until GitHub reports
`state: MERGED` or a non-empty `mergedAt` for that PR.

## Evidence checklist

Capture enough evidence to prove the boundary and the merge path without
rerunning the long validation every time:

- rendered maker and reviewer workflow files;
- `worker --print-config` or `worker --doctor` output for both roles;
- setup, maker, and reviewer `gh api user --jq .login` output;
- distinct `GH_CONFIG_DIR`, `workspace.root`, and `AIOPS_MIRROR_ROOT` values;
- branch protection JSON showing required status checks and required review;
- GitHub Actions/check JSON for the required check on the reviewed head;
- issue JSON and timeline showing label transitions;
- PR JSON showing author, head SHA, review state, checks, `mergeStateStatus`,
  `state`, and `mergedAt`;
- maker handoff issue comment containing the PR URL;
- reviewer approval or Rework review tied to the reviewed head SHA;
- Done/close comment after merge confirmation.

For release validation or a disposable proof run, use the helper scripts from
the E2E runbook:

- [`scripts/github-maker-reviewer-release-preflight.sh`](../../scripts/github-maker-reviewer-release-preflight.sh)
- [`scripts/github-maker-reviewer-capture.py`](../../scripts/github-maker-reviewer-capture.py)
- [`scripts/github-maker-reviewer-final-verify.py`](../../scripts/github-maker-reviewer-final-verify.py)
- [`scripts/github-maker-reviewer-report.py`](../../scripts/github-maker-reviewer-report.py)

The full disposable-repo path is documented in
[`docs/runbooks/github-maker-reviewer-automerge-e2e.md`](github-maker-reviewer-automerge-e2e.md).

## Failure recovery

Treat governance failures as blockers, not reasons to collapse roles:

- Wrong GitHub identity: stop the worker or agent turn, fix `GH_CONFIG_DIR` or
  `AIOPS_EXPECTED_GITHUB_LOGIN`, then retry. Do not continue under the wrong
  role.
- Shared maker/reviewer workspace or mirror root: stop both workers and restart
  with distinct `workspace.root` and `AIOPS_MIRROR_ROOT` values.
- Missing branch protection or required check: pause activation of new issues,
  fix GitHub settings, then capture fresh branch-protection evidence.
- Maker handoff without a PR URL: reviewer returns the issue to `aiops:rework`
  with a concrete comment about what it checked.
- Failed review or missing behavior-level tests: reviewer requests changes and
  moves the issue to `aiops:rework`; maker must push a new head and include a
  `Rework response:`.
- CI still running after approval: leave the issue in `aiops:human-review` so a
  later reviewer continuation can re-check the same PR before marking Done.
- PR already merged but approval/check evidence is missing for the merged head:
  stop and collect the missing evidence or escalate to an operator. Do not jump
  straight to Done.

The safe fallback for any unclear failure is to leave the issue in an active,
reviewable state (`aiops:human-review` or `aiops:rework`) with a precise comment.
Never downgrade to a single-agent merge, admin merge, or worker-owned merge.
