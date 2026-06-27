# Agent Workflow Guidelines

> Routing index and invariants for AI-assisted development in this repository.

This file does not replace the repository runbooks or Claude skills. It gives
Trellis tasks the minimum workflow context needed to route work to the right
source of truth.

---

## Primary Sources

| Work type | Source of truth |
| --- | --- |
| Daily operator loop | [`docs/runbooks/personal-daily-workflow.md`](../../../docs/runbooks/personal-daily-workflow.md) |
| Issue to PR work in this repo | [`.claude/skills/handle-issue/SKILL.md`](../../../.claude/skills/handle-issue/SKILL.md) |
| Existing PR follow-through | [`.claude/skills/handle-pr/SKILL.md`](../../../.claude/skills/handle-pr/SKILL.md) and [`docs/runbooks/pr-review-merge-protocol.md`](../../../docs/runbooks/pr-review-merge-protocol.md) |
| Fixed issue batches | [`docs/runbooks/batch-issue-processing.md`](../../../docs/runbooks/batch-issue-processing.md) |
| Binary self-hosted development | [`docs/runbooks/dogfood-development.md`](../../../docs/runbooks/dogfood-development.md) |
| Other repositories | [`docs/runbooks/agentic-project-template.md`](../../../docs/runbooks/agentic-project-template.md) |
| Runtime status interpretation | [`docs/runbooks/runtime-status.md`](../../../docs/runbooks/runtime-status.md) |
| GitHub maker/reviewer auto-merge release validation | [`docs/runbooks/github-maker-reviewer-automerge-e2e.md`](../../../docs/runbooks/github-maker-reviewer-automerge-e2e.md) |

---

## Scenario: GitHub Maker/Reviewer Auto-Merge E2E Harness

### 1. Scope / Trigger

- Trigger: adding or changing the reusable GitHub split-role E2E harness.
- Source of truth: the runbook above; this section pins the executable command
  and env contracts so future edits can be tested without rereading old run
  logs.

### 2. Signatures

- `scripts/github-maker-reviewer-e2e-bootstrap.sh --run-root DIR --repo OWNER/NAME [--port-base PORT]`
- `scripts/github-maker-reviewer-release-preflight.sh --run-root DIR [--release-repo OWNER/NAME] [--tag latest|vX.Y.Z]`
- `scripts/github-maker-reviewer-capture.py --run-root DIR --repo OWNER/NAME --tag TAG [--maker-url URL] [--reviewer-url URL] [--gh-config-dir DIR] [--include-github-pages] [--browser-storage-state PATH]`
- `scripts/github-maker-reviewer-final-verify.py --run-root DIR --repo OWNER/NAME [--gh-config-dir DIR]`
- `scripts/github-maker-reviewer-report.py --run-root DIR --repo OWNER/NAME [--date YYYY-MM-DD]`

### 3. Contracts

- Bootstrap creates distinct maker/reviewer workspace roots and distinct
  maker/reviewer mirror roots plus distinct setup/maker/reviewer
  `GH_CONFIG_DIR` directories under the run root.
- GitHub workflows use labels `aiops:todo`, `aiops:rework`,
  `aiops:human-review`, `aiops:done`, and `aiops:canceled`.
- Maker active states are `aiops:todo` and `aiops:rework`; reviewer active
  state is `aiops:human-review`.
- `GITHUB_TOKEN` remains worker-held and must not appear in
  `codex.env_passthrough`; agents receive `GH_CONFIG_DIR`,
  `NPM_CONFIG_CACHE`, `PLAYWRIGHT_BROWSERS_PATH`, and
  `AIOPS_EXPECTED_GITHUB_LOGIN`.
- Release preflight records release metadata, checksum, attestation, SBOM
  summary, binary versions, Codex version, role identity evidence, maker
  `git push --dry-run`, and maker/reviewer doctor logs.
- Capture records GitHub machine JSON through `gh`. Browser screenshots of
  private GitHub pages are explicit opt-in and need a Playwright storage-state
  file; `GH_CONFIG_DIR` does not authenticate the browser.

### 4. Validation & Error Matrix

- Missing `--run-root` or malformed `--repo` -> helper exits 2.
- Missing or mismatched GitHub role identity -> preflight exits non-zero and
  the run is BLOCKED.
- Failed checksum, attestation, `codex --version`, maker push dry-run, or
  doctor -> run is BLOCKED; do not downgrade to single-agent merge.
- Missing Playwright with explicit screenshot requests -> capture/final verify
  exits non-zero; default capture may skip screenshots only when no screenshot
  target was requested.
- Missing `--browser-storage-state` path when provided -> capture exits
  non-zero before taking screenshots.

### 5. Good/Base/Bad Cases

- Good: maker opens PR with non-closing issue reference, reviewer approves,
  GitHub required check passes, reviewer confirms `mergedAt`, then adds Done and
  closes the issue.
- Base: CI is slow; reviewer leaves the issue in `aiops:human-review` and a
  later continuation confirms the merge.
- Bad: same `GH_CONFIG_DIR` or same `AIOPS_MIRROR_ROOT` for maker and reviewer,
  maker uses `gh pr merge`, maker references `Issue #N` in the PR body, or issue
  is closed before GitHub reports merged.

### 6. Tests Required

- `scripts/github_maker_reviewer_e2e_test.go` must cover runbook discovery,
  workflow loading, env passthrough boundaries, bootstrap output, report output,
  and helper `--help` entrypoints.
- Run `go test -run 'TestGitHubMakerReviewer' -count=1 ./scripts` after edits.
- Run `go test -count=1 ./scripts` before committing shared script/runbook
  changes.

### 7. Wrong vs Correct

#### Wrong

Use one GitHub identity for both workflows and let the worker or maker merge
after CI passes.

#### Correct

Use distinct GitHub identities, distinct workspace roots, GitHub required
checks, reviewer `--match-head-commit` auto-merge, and Done/close only after
GitHub reports the PR merged.

---

## Trellis Role

Use Trellis for planning memory, task context, and batch ledgers:

- Create a parent Trellis task for a multi-issue batch or dogfood rollout.
- Create one child Trellis task per tracker issue.
- Record issue dependencies, branch/PR state, and next action in the parent
  task.
- Keep implementation instructions in the tracker issue and PR body, not only in
  Trellis.

Trellis is not a scheduler lock and is not a replacement for tracker state.
The worker dispatches from the configured tracker and the runbook-defined ready
gate.

The selected `WORKFLOW.md` defines the worker's operating mode. Dogfood is one
mode, not a global rule that every future AI-assisted development session must
use.

---

## Pre-Implementation Gate

Before changing code or workflow behavior:

1. Open or update the relevant Trellis task.
2. Write a short plan with goal, scope, acceptance criteria, dependencies, and
   verification.
3. For issue work driven by a design document, runbook, SPEC boundary,
   redaction/retention rule, or non-goal list, extract the negative constraints
   before choosing an implementation: required behavior, unsupported inputs,
   redaction and retention limits, forbidden runtime side effects, and any "do
   not store / parse / mutate / persist / automate / gate" wording. If the
   simplest design would require understanding arbitrary human, agent, or
   protocol text, stop and prefer opaque omission unless the issue explicitly
   asks for structured parsing with fixtures.
4. Run a `grill-with-docs` review of the plan against `CONTEXT.md`, ADRs, and
   the relevant runbooks.
5. Capture settled terminology in `CONTEXT.md` only when it is domain language,
   and capture durable trade-off decisions in `docs/adr/` only when they meet
   the ADR bar.
6. Before pre-push review, follow
   [`docs/runbooks/pr-review-merge-protocol.md`](../../../docs/runbooks/pr-review-merge-protocol.md)
   as the single source of truth. Do not restate reviewer-routing mechanics in
   Trellis specs or task notes.

For small documentation-only edits, the Trellis task may be lightweight, but the
source-of-truth runbook still wins over task notes.

---

## Issue Dependency Invariants

Classify every batch issue before dispatch:

- `hard dependency`: downstream work needs an upstream merge, API, schema,
  migration, branch base, or atomic refactor. Serialize it.
- `soft overlap`: shared files, package surface, generated artifacts, or
  dependency manifests create review/merge risk. Serialize in unattended mode.
- `independent issue`: no branch, contract, path, or review dependency. May run
  in parallel within worker and review capacity.

Tracker-specific gates:

- Linear: use native blocked-by relationships and keep blocked issues in `Todo`
  until blockers are terminal.
- Gitea: express issue dependencies with `Depends on #N`, but keep dependent
  issues in `Todo` or out of active labels until blockers are terminal.
- GitHub: use `aiops:ready` as the unattended queue label. Do not use `open` as
  an active state for dogfood work. The GitHub maker/reviewer release-validation
  harness is a separate disposable-repo mode and uses the runbook-specific
  `aiops:todo` -> `aiops:human-review` -> `aiops:done` label path instead.

`/api/v1/state.blocked` is not a dependency backlog. It reports local blocked
claims such as input-required or continuation-budget stops.

---

## Non-Goals

- Do not duplicate the `handle-issue`, `handle-pr`, batch, or PR merge protocol
  inside Trellis specs.
- Do not use Trellis parent/child links as evidence that the worker will avoid
  dispatching a blocked issue.
- Do not treat priority labels as readiness. Priority is human triage metadata;
  it does not grant permission to run work.
