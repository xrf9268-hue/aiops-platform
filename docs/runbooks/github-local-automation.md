# GitHub local automation

This runbook wires the local macOS operator flow for resolving
`xrf9268-hue/aiops-platform` GitHub issues with the worker-owned polling path.

## Model

- `cmd/worker` is the long-running issue execution engine.
- `examples/github-local-WORKFLOW.md` selects open GitHub issues by priority
  labels (`priority:p1`, `priority:p2`, `priority:p3`) first, then remaining
  open issues that have not been priority-triaged yet.
- Each issue runs in a deterministic per-issue workspace under
  `~/aiops-workspaces/github/xrf9268-hue-aiops-platform`.
- Codex is the primary implementer and is configured for full access through
  `codex.profile: bypass`.
- Codex review and Claude Code are both mandatory independent local diff
  reviewers before PR handoff.
- `scripts/local-pr-follow-through.sh` serializes PR gates and auto-merge:
  local Go gates, independent Codex and Claude Code diff reviews, all GitHub checks,
  unresolved review-thread check, then `gh pr merge --squash --auto`.
- The local review commands intentionally use structured-output modes:
  Codex runs `codex exec --output-schema <schema-file> -`, and Claude Code runs
  `claude -p --tools "" --json-schema '<schema-json>'`. `codex exec review
  --base` is not used for this gate because the Codex CLI treats `--base` and
  custom review prompt arguments as mutually exclusive. Claude Code receives the
  complete diff on stdin and intentionally has no repository tools in this gate,
  so the unattended run stays bounded to a single structured-output review
  task. Review sessions use Codex `--ephemeral` and Claude Code
  `--no-session-persistence` to avoid retaining short-lived reviewer history.

## Prerequisites

- `gh auth status -h github.com` must show an account with `repo` and workflow
  access to `xrf9268-hue/aiops-platform`.
- `codex`, `claude`, `go`, and `gh` must be on PATH.
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

## Install unattended macOS LaunchAgents

```bash
scripts/install-local-launchagents.sh
```

This installs:

- `com.aiops-platform.github-worker`: long-running worker, restarted by
  launchd if it exits.
- `com.aiops-platform.github-pr-follow-through`: PR gate/auto-merge sweep every
  10 minutes. It installs with `AIOPS_AUTO_MERGE=0` by default, so it runs the
  full gate without merging until the operator explicitly installs with
  `AIOPS_AUTO_MERGE=1`.

Logs are written under `~/Library/Logs/aiops-platform/`.

To stop and uninstall the local services:

```bash
scripts/uninstall-local-launchagents.sh
```

## Operational guardrails

- Do not run more than one worker service against the same workflow/workspace
  root.
- Keep branch protection and required CI enabled on `main`.
- Treat a non-JSON or non-empty Claude local review as blocking.
- Treat a non-JSON or non-empty Codex local review as blocking.
- Treat a local reviewer timeout as blocking. Increase `AIOPS_REVIEW_TIMEOUT`
  only after checking the reviewer logs and confirming it is slow rather than
  stuck.
- Claude Code review defaults to `AIOPS_CLAUDE_REVIEW_MAX_TURNS=2` and disables
  tools with `--tools ""`. It reviews only the supplied diff; re-enabling
  repository tools can make unattended runs hit turn limits before returning
  JSON.
- Treat unresolved non-outdated GitHub review threads as blocking.
- Use `AIOPS_AUTO_MERGE=0` during new workflow changes or after changing local
  credentials. This is the LaunchAgent install default; set
  `AIOPS_AUTO_MERGE=1` only after the no-merge sweep logs are clean.
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
