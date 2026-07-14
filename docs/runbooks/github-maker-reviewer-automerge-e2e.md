# GitHub maker/reviewer auto-merge E2E

This runbook validates the production-governance shape for GitHub: a maker
worker implements issues and opens PRs, a separate reviewer worker independently
reviews those PRs, GitHub branch protection gates the merge, and the reviewer
marks issues Done/closed only after GitHub reports the PR merged. For the short
operator setup checklist, read
[`github-maker-reviewer-governance.md`](github-maker-reviewer-governance.md)
first.

This is a release-validation and governance test, not a CI check. It spends real
Codex quota, needs real GitHub accounts, and should run in a disposable
repository.

## What This Validates

- The worker/orchestrator schedules, runs, polls, and observes; it does not write
  PRs, merge PRs, approve reviews, or close issues.
- The maker agent can implement, test, push, open or update a PR, comment the PR
  URL, and hand off with `aiops:human-review`.
- The maker cannot approve or merge its own PR.
- The reviewer agent runs in a different workspace root, different `GH_CONFIG_DIR`,
  and different agent context.
- The reviewer requests Rework on a failing head, and the maker responds with a
  new head before handing off again.
- The reviewer records one exact head/base `COMMENTED` checkpoint after local
  PASS; same-tuple retries reuse it and take one live external-state snapshot.
- The reviewer approves and enables GitHub native CI-gated auto-merge only after
  review passes.
- The reviewer confirms `state: MERGED` or non-empty `mergedAt` before adding
  `aiops:done` and closing the issue.
- A dependency issue is not activated until its prerequisite issue is Done/closed.

If any preflight below fails, report **BLOCKED**. Do not switch to a
single-agent merge or a worker-side merge.

## Helper Scripts

| Script | Purpose |
| --- | --- |
| `scripts/github-maker-reviewer-e2e-bootstrap.sh` | Create the run-root skeleton, render GitHub maker/reviewer workflows, seed issue bodies, and write `env.example` plus `NEXT-STEPS.md`. |
| `scripts/github-maker-reviewer-release-preflight.sh` | Resolve the latest release, download worker/tui/SHA/SBOM, verify checksum and attestation, extract binaries, record versions, and run doctor. |
| `scripts/github-maker-reviewer-capture.py` | Capture worker `/api/v1/state`, GitHub issue/PR/Actions/branch-protection JSON, and key screenshots. |
| `scripts/github-maker-reviewer-final-verify.py` | Fresh-clone `main`, run npm gates, and capture final desktop/mobile app screenshots. |
| `scripts/github-maker-reviewer-report.py` | Generate `reports/report.md` and `reports/merge-mechanism-retro.md` from captured evidence. |

## 0. Prepare Host Tools

Required:

- `gh` authenticated to GitHub for setup operations.
- Git, Node.js, npm, Python 3, and Playwright Chromium for screenshots.
- A working host `codex app-server` auth path.
- Permission to create or mutate a disposable GitHub repository.

Install screenshot tooling once outside the repo. Browser binaries are installed
into the run-root cache after `env.local` is sourced.

```bash
python3 -m venv "$HOME/.cache/aiops-ghmr-e2e/venv"
. "$HOME/.cache/aiops-ghmr-e2e/venv/bin/activate"
python -m pip install --upgrade pip
python -m pip install playwright
```

## 1. Prepare the Run Root

```bash
RUN_ROOT=/tmp/aiops-github-maker-reviewer-automerge-e2e-$(date +%Y%m%d-%H%M%S)
REPO_OWNER=your-github-org-or-user
REPO_NAME=aiops-e2e-gh-maker-reviewer-$(date +%Y%m%d-%H%M%S)

scripts/github-maker-reviewer-e2e-bootstrap.sh \
  --run-root "$RUN_ROOT" \
  --repo "$REPO_OWNER/$REPO_NAME" \
  --port-base 4300

install -m 600 "$RUN_ROOT/env.example" "$RUN_ROOT/env.local"
$EDITOR "$RUN_ROOT/env.local"
set -a
. "$RUN_ROOT/env.local"
set +a

PLAYWRIGHT_BROWSERS_PATH="$PLAYWRIGHT_BROWSERS_PATH" \
  python -m playwright install chromium
```

Do not commit `env.local`, `secrets/`, downloaded binaries, auth homes, npm
caches, or raw run evidence.

The bootstrap writes:

- `workflows/github-maker-WORKFLOW.md`
- `workflows/github-reviewer-automerge-WORKFLOW.md`
- `issues/01-*.md`, `issues/02-*.md`, `issues/03-*.md`, and an optional
  deterministic Rework control issue
- `tools/` copies of the reusable helper scripts
- evidence directories: `logs/`, `state/`, `screenshots/`, `forge-json/`,
  `final-verify/`, and `reports/`

## 2. Configure GitHub Identities

Use three credentials:

- setup/operator: repository creation, branch protection, and capture
- maker: code/branch/PR author
- reviewer: PR reviewer, auto-merge enabler, and issue closer

Each role must use a distinct file-backed `GH_CONFIG_DIR`:

```bash
mkdir -p \
  "$AIOPS_GHMR_SETUP_GH_CONFIG_DIR" \
  "$AIOPS_GHMR_MAKER_GH_CONFIG_DIR" \
  "$AIOPS_GHMR_REVIEWER_GH_CONFIG_DIR"

env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$AIOPS_GHMR_SETUP_GH_CONFIG_DIR" gh auth login
env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$AIOPS_GHMR_MAKER_GH_CONFIG_DIR" gh auth login
env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$AIOPS_GHMR_REVIEWER_GH_CONFIG_DIR" gh auth login

env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$AIOPS_GHMR_SETUP_GH_CONFIG_DIR" gh auth setup-git
env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$AIOPS_GHMR_MAKER_GH_CONFIG_DIR" gh auth setup-git
env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$AIOPS_GHMR_REVIEWER_GH_CONFIG_DIR" gh auth setup-git

env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$AIOPS_GHMR_MAKER_GH_CONFIG_DIR" gh api user --jq .login
env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$AIOPS_GHMR_REVIEWER_GH_CONFIG_DIR" gh api user --jq .login
```

Set `AIOPS_GHMR_MAKER_LOGIN` and `AIOPS_GHMR_REVIEWER_LOGIN` in `env.local` to
the observed logins. The workflow passes only `GH_CONFIG_DIR` and
`AIOPS_EXPECTED_GITHUB_LOGIN` to the agent. It does **not** pass `GITHUB_TOKEN`
to the agent.

## 3. Seed the Disposable Repo

Create or reset a disposable repository:

```bash
env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$AIOPS_GHMR_SETUP_GH_CONFIG_DIR" \
  gh repo create "$AIOPS_GHMR_REPO" --private
```

Seed a minimal Vite React TypeScript Web Todo app. It must have these commands:

```bash
npm ci
npm test
npm run build
npm run test:e2e
```

Add a GitHub Actions workflow with a required job named `build-test` that runs
the same gates:

```yaml
name: build-test

on:
  pull_request:
  push:
    branches: [main]

jobs:
  build-test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
        with:
          node-version: 22
          cache: npm
      - run: npm ci
      - run: npm test
      - run: npm run build
      - run: npx playwright install --with-deps chromium
      - run: npm run test:e2e
```

Push `main`, wait for the first `build-test` run to pass, and capture the seed
state:

```bash
env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$AIOPS_GHMR_SETUP_GH_CONFIG_DIR" \
  gh run list --repo "$AIOPS_GHMR_REPO" --limit 5 \
  --json databaseId,workflowName,status,conclusion,headSha,url \
  > "$AIOPS_GHMR_RUN_ROOT/forge-json/actions-initial.json"
```

## 4. Configure Labels and Branch Protection

Create labels:

```bash
for label in aiops:todo aiops:rework aiops:human-review aiops:blocked aiops:done aiops:canceled; do
  env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$AIOPS_GHMR_SETUP_GH_CONFIG_DIR" \
    gh label create "$label" --repo "$AIOPS_GHMR_REPO" --color 5319e7 --force
done
```

`aiops:blocked` is only for true operator-owned blockers. Codex, CI, approval,
auto-merge, and merge pending stay in `aiops:human-review`; current-head review
threads are FAIL evidence for `aiops:rework`. Rework needs a new head plus a
`Rework response:`; historical review counts are diagnostic only.

After local PASS on an unseen `(headRefOid, baseRefOid, baseRefName)`, the
reviewer writes one reviewer-owned `COMMENTED` checkpoint. A same-tuple retry
skips local gates/rubric, reuses any exact-tuple Codex trigger, and takes one
live snapshot. It never posts a second trigger or waits/polls for external
state. REST records and GraphQL threads are paginated inside the snapshot. A
changed head/base requires full review. Review commands use a detached captured
head, a pre-write tuple-only guard, and REST `commit_id` pinning. Branch
protection's stale approval dismissal prevents a later merge-base change from
using the prior approval. Blocked handoffs remove active labels while adding
`aiops:blocked`.

Enable auto-merge and squash-only merges:

```bash
env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$AIOPS_GHMR_SETUP_GH_CONFIG_DIR" \
  gh repo edit "$AIOPS_GHMR_REPO" \
    --enable-auto-merge \
    --enable-squash-merge \
    --delete-branch-on-merge \
    --enable-merge-commit=false \
    --enable-rebase-merge=false
```

Protect `main`:

```bash
env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$AIOPS_GHMR_SETUP_GH_CONFIG_DIR" \
  gh api --method PUT "repos/$AIOPS_GHMR_REPO/branches/main/protection" \
  --input - <<'JSON'
{
  "required_status_checks": {
    "strict": true,
    "contexts": ["build-test"]
  },
  "enforce_admins": true,
  "required_pull_request_reviews": {
    "dismiss_stale_reviews": true,
    "require_last_push_approval": true,
    "required_approving_review_count": 1
  },
  "restrictions": null,
  "required_linear_history": false,
  "allow_force_pushes": false,
  "allow_deletions": false,
  "block_creations": false,
  "required_conversation_resolution": false,
  "lock_branch": false,
  "allow_fork_syncing": true
}
JSON
```

Capture:

```bash
env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$AIOPS_GHMR_SETUP_GH_CONFIG_DIR" \
  gh api "repos/$AIOPS_GHMR_REPO/branches/main/protection" \
  > "$AIOPS_GHMR_RUN_ROOT/forge-json/branch-protection-initial.json"
```

## 5. Create Issues Without Activating Them

Create issues from the bootstrap-generated issue bodies. Do not add active labels
yet:

```bash
for file in "$AIOPS_GHMR_RUN_ROOT"/issues/*.md; do
  title="$(sed -n '1s/^# //p' "$file")"
  env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$AIOPS_GHMR_SETUP_GH_CONFIG_DIR" \
    gh issue create --repo "$AIOPS_GHMR_REPO" --title "$title" --body-file "$file"
done
```

Activate only the current ready issue:

```bash
env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$AIOPS_GHMR_SETUP_GH_CONFIG_DIR" \
  gh issue edit 1 --repo "$AIOPS_GHMR_REPO" --add-label aiops:todo
```

For dependency/sequencing proof, keep issue #3 without `aiops:todo` until the
prerequisite issues are `aiops:done` and closed.

## 6. Preflight Binary and Doctor

Run this after sourcing `env.local` and before activating more work:

```bash
"$AIOPS_GHMR_RUN_ROOT/tools/release-preflight.sh" \
  --run-root "$AIOPS_GHMR_RUN_ROOT"
```

The script runs `worker --doctor --deploy=binary --mode=real` for both
workflows with separate maker/reviewer `AIOPS_MIRROR_ROOT` values, records the
role identities, and runs a maker `git push --dry-run` against a disposable
branch ref when the repo variables are set. The preflight must record:

- `artifacts/release-view-summary.json`
- `artifacts/sha256.log`
- `artifacts/attestation.log`
- `artifacts/sbom-summary.json`
- `artifacts/worker-version.log`
- `artifacts/tui-version.log`
- `artifacts/codex-version.log`
- `artifacts/github-role-auth-preflight.log`
- `artifacts/maker-git-push-dry-run.log`
- `logs/maker-doctor.log`
- `logs/reviewer-doctor.log`

If `gh release view`, checksum, attestation, `codex --version`, GitHub auth,
git push dry-run, or either doctor fails, stop and report BLOCKED.

## 7. Start Workers

Start maker:

```bash
GH_CONFIG_DIR="$AIOPS_GHMR_MAKER_GH_CONFIG_DIR" \
AIOPS_MIRROR_ROOT="$AIOPS_GHMR_MAKER_MIRROR_ROOT" \
AIOPS_EXPECTED_GITHUB_LOGIN="$AIOPS_GHMR_MAKER_LOGIN" \
NPM_CONFIG_CACHE="$NPM_CONFIG_CACHE" \
PLAYWRIGHT_BROWSERS_PATH="$PLAYWRIGHT_BROWSERS_PATH" \
  "$AIOPS_GHMR_WORKER_BIN" \
  --port "$AIOPS_GHMR_MAKER_PORT" \
  "$AIOPS_GHMR_MAKER_WORKFLOW" \
  2>&1 | tee "$AIOPS_GHMR_RUN_ROOT/logs/maker-worker.log"
```

Start reviewer:

```bash
GH_CONFIG_DIR="$AIOPS_GHMR_REVIEWER_GH_CONFIG_DIR" \
AIOPS_MIRROR_ROOT="$AIOPS_GHMR_REVIEWER_MIRROR_ROOT" \
AIOPS_EXPECTED_GITHUB_LOGIN="$AIOPS_GHMR_REVIEWER_LOGIN" \
NPM_CONFIG_CACHE="$NPM_CONFIG_CACHE" \
PLAYWRIGHT_BROWSERS_PATH="$PLAYWRIGHT_BROWSERS_PATH" \
  "$AIOPS_GHMR_WORKER_BIN" \
  --port "$AIOPS_GHMR_REVIEWER_PORT" \
  "$AIOPS_GHMR_REVIEWER_WORKFLOW" \
  2>&1 | tee "$AIOPS_GHMR_RUN_ROOT/logs/reviewer-worker.log"
```

Both workers must use the downloaded `worker` binary and real
`codex app-server`. The maker workspace and mirror roots must differ from the
reviewer workspace and mirror roots.

## 8. Capture Key Evidence

Use the capture helper at every transition:

```bash
"$AIOPS_GHMR_RUN_ROOT/tools/capture.py" \
  --run-root "$AIOPS_GHMR_RUN_ROOT" \
  --repo "$AIOPS_GHMR_REPO" \
  --tag issue1-maker-handoff \
  --maker-url "$AIOPS_GHMR_MAKER_DASHBOARD_URL" \
  --reviewer-url "$AIOPS_GHMR_REVIEWER_DASHBOARD_URL" \
  --gh-config-dir "$AIOPS_GHMR_SETUP_GH_CONFIG_DIR"
```

The helper always captures worker dashboard screenshots when dashboard URLs are
provided. It captures GitHub issue/action pages only when explicitly requested:

```bash
"$AIOPS_GHMR_RUN_ROOT/tools/capture.py" \
  --run-root "$AIOPS_GHMR_RUN_ROOT" \
  --repo "$AIOPS_GHMR_REPO" \
  --tag issue1-maker-handoff \
  --maker-url "$AIOPS_GHMR_MAKER_DASHBOARD_URL" \
  --reviewer-url "$AIOPS_GHMR_REVIEWER_DASHBOARD_URL" \
  --gh-config-dir "$AIOPS_GHMR_SETUP_GH_CONFIG_DIR" \
  --include-github-pages \
  --browser-storage-state "$AIOPS_GHMR_RUN_ROOT/state/github-storage-state.json"
```

For private disposable repos, `GH_CONFIG_DIR` authenticates only `gh`, not the
Playwright browser. Create `github-storage-state.json` from a logged-in browser
profile before relying on private GitHub page screenshots, or capture those
pages manually in the normal browser. The JSON evidence remains authoritative
for issue/PR/Actions state.

Required screenshot anchors:

- preflight / doctor result
- GitHub repo / issues initial state
- maker worker dashboard running
- reviewer worker dashboard running
- maker PR handoff with `aiops:human-review`
- reviewer approval / auto-merge / merge evidence
- first Rework rejection
- Rework fix handoff and second pass
- final Done/closed issues
- final Actions summary
- final app desktop and mobile screenshots

Required machine evidence:

- worker doctor logs
- maker and reviewer worker logs
- `/api/v1/state` snapshots
- reviewer checkpoint and exact-tuple trigger evidence
- GitHub issue JSON
- GitHub PR JSON
- GitHub Actions/check JSON
- branch protection JSON
- driver/capture logs
- fresh clone verification logs
- release checksum, attestation, and SBOM evidence
- `worker --version`, `tui --version`, `codex --version`, and role auth evidence

## 9. Rework and Dependency Control

Prefer a natural Rework on issue #2. If it passes first review, activate the
control Rework issue from `issues/04-*.md` to force deterministic proof:

- reviewer requests changes on the first head when the named executable test is
  missing
- issue moves from `aiops:human-review` to `aiops:rework`
- maker pushes a new commit and includes `Rework response:`
- reviewer reviews the new head, approves, enables auto-merge, confirms merged,
  then marks Done/closed
- reviewer uses `gh pr merge <PR> --auto --squash --delete-branch --match-head-commit <sha>`
  and never `--admin`

Activate the dependency issue only after prerequisite issues are Done/closed:

```bash
env -u GH_TOKEN -u GITHUB_TOKEN GH_CONFIG_DIR="$AIOPS_GHMR_SETUP_GH_CONFIG_DIR" \
  gh issue edit 3 --repo "$AIOPS_GHMR_REPO" --add-label aiops:todo
```

Keep a screenshot and JSON snapshot immediately before this activation.

## 10. Final Verification and Report

Fresh-clone `main` and run the same gates:

```bash
"$AIOPS_GHMR_RUN_ROOT/tools/final-verify.py" \
  --run-root "$AIOPS_GHMR_RUN_ROOT" \
  --repo "$AIOPS_GHMR_REPO" \
  --gh-config-dir "$AIOPS_GHMR_SETUP_GH_CONFIG_DIR"
```

Capture the final GitHub state with the exact `final` tag consumed by the report
helper:

```bash
"$AIOPS_GHMR_RUN_ROOT/tools/capture.py" \
  --run-root "$AIOPS_GHMR_RUN_ROOT" \
  --repo "$AIOPS_GHMR_REPO" \
  --tag final \
  --skip-screenshots \
  --gh-config-dir "$AIOPS_GHMR_SETUP_GH_CONFIG_DIR"
```

Generate reports:

```bash
"$AIOPS_GHMR_RUN_ROOT/tools/report.py" \
  --run-root "$AIOPS_GHMR_RUN_ROOT" \
  --repo "$AIOPS_GHMR_REPO" \
  --reviewer-login "$AIOPS_GHMR_REVIEWER_LOGIN"
```

The final deliverables live under:

- `reports/report.md`
- `reports/merge-mechanism-retro.md`
- `screenshots/`
- `forge-json/`
- `state/`
- `logs/`
- `final-verify/logs/`

## Pass / Fail Criteria

PASS requires all of the following:

- Latest formal release binary is used, not a local source build.
- Checksum, attestation, SBOM capture, version capture, and doctor pass.
- At least three feature issues exist.
- Happy path reaches maker PR, reviewer approval, CI-gated merge, Done/close.
- Rework path has at least one reviewer `CHANGES_REQUESTED` or equivalent
  Rework finding, then a new maker head, then reviewer approval and merge.
- Dependency issue is not activated before prerequisite Done/closed evidence.
- Maker does not approve, auto-merge, merge, close, or add `aiops:done`.
- Reviewer does not edit, commit, or push code.
- Same-tuple reviewer retry reuses its checkpoint, samples external state once,
  and does not repeat verification or the semantic/security rubric.
- Worker/orchestrator does not create PRs, merge PRs, or directly set terminal
  tracker state.
- Issue closure occurs only after reviewer confirms GitHub merged state.
- Fresh clone verification passes all npm gates.
- Final report includes verdict, timeline or event sequence, issue/PR table,
  maker/reviewer boundary evidence, auto-merge evidence, Rework evidence,
  screenshots index, and anomalies.

FAIL when a governance invariant is violated. BLOCKED when a required preflight
or platform capability is unavailable. Do not downgrade the scenario into
single-agent merge, admin merge, or worker/orchestrator merge.
