# Local Gitea Crowd Runner product lifecycle E2E

This runbook validates the latest aiops-platform release binary with real `codex app-server` by asking Codex to build a fresh production-style 3D
crowd-runner web game through Gitea issues and PRs. It is a supervised release
validation and product-quality capture flow, not a CI job: it spends real Codex
quota and needs local Gitea credentials.

The previous private `crowdrunner` repository is historical context only. The
exercise creates a new `crowd-runner-product` repository and requires a fresh
design inspired by the public crowd-runner genre: gate choices, crowd growth,
traps, enemy clashes, coins/upgrades, skins, deterministic daily challenges,
castle/King finale, mobile portrait play, and measurable performance.

## What This Validates

1. The downloaded release `worker` and `tui` binaries run outside the source
   tree after checksum and GitHub attestation verification.
2. A local Gitea maker worker consumes `Todo` / `Rework`, writes code with real
   Codex, opens PRs, and hands off at `Human Review`.
3. A separate reviewer worker reviews in a fresh Codex context, sends failures
   to `Rework`, or approves and enables CI-gated auto-merge before setting
   `Done`.
4. Control issues prove no-ready idle behavior, reconcile cancellation, and a
   low-turn continuation / blocked path.
5. A fresh clone of the final product passes npm lint, unit tests, Playwright,
   and build, with screenshots and performance evidence.

## Helper Scripts

| Script | Purpose |
| --- | --- |
| `scripts/e2e-crowdrunner-bootstrap.sh` | Create the run-root skeleton, render maker/reviewer/stress workflows, seed a minimal repo, and write product/control issue bodies. |
| `scripts/e2e-crowdrunner-capture.py` | Capture worker state JSON, TUI raw frames, Gitea/dashboard/product screenshots, and a capture index. |
| `scripts/e2e-crowdrunner-freeze.py` | Optionally freeze dispatch at an operator-selected product milestone by removing ready labels and writing evidence. |
| `scripts/e2e-crowdrunner-report.py` | Generate the final Markdown report and promotion notes from saved state, PR, screenshot, trace, and verification evidence. |

## 0. Prepare the Host

Required tools:

- Git, curl, Python 3, Node/npm, and Go for local verification.
- `gh` for release asset and attestation verification.
- A local Gitea instance with maker/reviewer bot credentials.
- A working local Codex configuration. Local binary mode intentionally reuses the operator's usual Codex setup unless `CODEX_HOME` is explicitly set.
- Playwright Chromium for required screenshots and final product smoke.

Prepare Playwright once:

```bash
export AIOPS_CROWDRUNNER_TOOLS_ROOT="${AIOPS_CROWDRUNNER_TOOLS_ROOT:-$HOME/.cache/aiops-crowdrunner-e2e-tools}"
mkdir -p "$AIOPS_CROWDRUNNER_TOOLS_ROOT"
python3 -m venv "$AIOPS_CROWDRUNNER_TOOLS_ROOT/venv"
. "$AIOPS_CROWDRUNNER_TOOLS_ROOT/venv/bin/activate"
python -m pip install --upgrade pip
python -m pip install playwright
python -m playwright install chromium
```

## 1. Prepare the Run Root

Choose ports that do not collide with other workers. The default example uses
Gitea on `3107`, maker on `4201`, reviewer on `4202`, and the optional stress
worker on `4203`.

```bash
RUN_ROOT=/tmp/aiops-real-crowdrunner-e2e-$(date +%Y%m%d-%H%M%S)
scripts/e2e-crowdrunner-bootstrap.sh \
  --run-root "$RUN_ROOT" \
  --gitea-url http://127.0.0.1:3107 \
  --repo-owner aiops-bot \
  --repo-name crowd-runner-product \
  --port-base 4200 \
  --release-tag v0.1.9
```

The script writes `env.example`, `NEXT-STEPS.md`, `seed-repo/`, `issues/*.md`,
`workflows/maker-WORKFLOW.md`, `workflows/reviewer-automerge-WORKFLOW.md`,
`workflows/maker-low-turn-WORKFLOW.md`, and evidence directories for logs,
state JSON, screenshots, videos, traces, and reports.

Copy and fill secrets:

```bash
cp "$RUN_ROOT/env.example" "$RUN_ROOT/env.local"
$EDITOR "$RUN_ROOT/env.local"
set -a
. "$RUN_ROOT/env.local"
set +a
```

Do not commit `env.local`; it contains credentials.

## 2. Download and Verify the Release Binary

Use the release asset matching this macOS ARM64 host:
`aiops-platform_v0.1.9_darwin_arm64.tar.gz`.

```bash
cd "$AIOPS_CROWDRUNNER_RUN_ROOT/downloads"
gh release download "$AIOPS_CROWDRUNNER_RELEASE_TAG" \
  --repo xrf9268-hue/aiops-platform \
  --pattern "aiops-platform_${AIOPS_CROWDRUNNER_RELEASE_TAG}_darwin_arm64.tar.gz" \
  --pattern "aiops-platform_${AIOPS_CROWDRUNNER_RELEASE_TAG}_SHA256SUMS"

shasum -a 256 -c "aiops-platform_${AIOPS_CROWDRUNNER_RELEASE_TAG}_SHA256SUMS" \
  2>&1 | tee "$AIOPS_CROWDRUNNER_RUN_ROOT/artifacts/sha256.log"
gh attestation verify \
  "aiops-platform_${AIOPS_CROWDRUNNER_RELEASE_TAG}_darwin_arm64.tar.gz" \
  --repo xrf9268-hue/aiops-platform \
  2>&1 | tee "$AIOPS_CROWDRUNNER_RUN_ROOT/artifacts/attestation.log"

tar -xzf "aiops-platform_${AIOPS_CROWDRUNNER_RELEASE_TAG}_darwin_arm64.tar.gz" \
  -C "$AIOPS_CROWDRUNNER_RUN_ROOT/bin" worker tui
"$AIOPS_CROWDRUNNER_WORKER_BIN" --version | tee "$AIOPS_CROWDRUNNER_RUN_ROOT/artifacts/worker-version.log"
"$AIOPS_CROWDRUNNER_TUI_BIN" --version | tee "$AIOPS_CROWDRUNNER_RUN_ROOT/artifacts/tui-version.log"
codex --version | tee "$AIOPS_CROWDRUNNER_RUN_ROOT/artifacts/codex-version.log"
codex app-server --help > "$AIOPS_CROWDRUNNER_RUN_ROOT/artifacts/codex-app-server-help.log"
```

## 3. Create Local Gitea State

Create a new repository from `$AIOPS_CROWDRUNNER_RUN_ROOT/seed-repo`.

Required labels:

- `aiops/todo`
- `aiops/in-progress`
- `aiops/human-review`
- `aiops/rework`
- `aiops/done`
- `aiops/canceled`
- `aiops/stress` (auxiliary routing label for issue 16; pair it with
  `aiops/todo` so Gitea derives the mapped `Todo` state)

Create issues from `issues/*.md` without `aiops/*` state labels. Keep product
issues inactive until the dashboard doctor passes; after that gate, activate
issues 1-12 by adding `aiops/todo`. Keep control issues staged:

- Issue 13: no `aiops/*` label.
- Issue 14: add `aiops/todo` only when testing cancellation.
- Issue 15: leave unlabeled or blocked.
- Issue 16: run only with the low-turn stress workflow; add both `aiops/todo`
  and `aiops/stress` when starting the continuation-budget control.

Save the created issue list:

```bash
curl -fsS -H "Authorization: token $GITEA_TOKEN" \
  "$AIOPS_CROWDRUNNER_GITEA_URL/api/v1/repos/$AIOPS_CROWDRUNNER_REPO_OWNER/$AIOPS_CROWDRUNNER_REPO_NAME/issues?state=all" \
  > "$AIOPS_CROWDRUNNER_RUN_ROOT/state/issues-created.json"
```

## 4. Preflight the Workers

Run the release binary, not `go run`:

```bash
AIOPS_WORKFLOW_PATH="$AIOPS_CROWDRUNNER_MAKER_WORKFLOW" \
AIOPS_MIRROR_ROOT="$AIOPS_CROWDRUNNER_MAKER_MIRROR_ROOT" \
  "$AIOPS_CROWDRUNNER_WORKER_BIN" --doctor --deploy=binary --mode=real \
  "$AIOPS_CROWDRUNNER_MAKER_WORKFLOW" \
  | tee "$AIOPS_CROWDRUNNER_RUN_ROOT/artifacts/maker-doctor.log"

AIOPS_WORKFLOW_PATH="$AIOPS_CROWDRUNNER_REVIEWER_WORKFLOW" \
AIOPS_MIRROR_ROOT="$AIOPS_CROWDRUNNER_REVIEWER_MIRROR_ROOT" \
  "$AIOPS_CROWDRUNNER_WORKER_BIN" --doctor --deploy=binary --mode=real \
  "$AIOPS_CROWDRUNNER_REVIEWER_WORKFLOW" \
  | tee "$AIOPS_CROWDRUNNER_RUN_ROOT/artifacts/reviewer-doctor.log"

"$AIOPS_CROWDRUNNER_WORKER_BIN" --print-config "$(dirname "$AIOPS_CROWDRUNNER_MAKER_WORKFLOW")" \
  > "$AIOPS_CROWDRUNNER_RUN_ROOT/artifacts/maker-print-config.log"
"$AIOPS_CROWDRUNNER_WORKER_BIN" --print-config "$(dirname "$AIOPS_CROWDRUNNER_REVIEWER_WORKFLOW")" \
  > "$AIOPS_CROWDRUNNER_RUN_ROOT/artifacts/reviewer-print-config.log"
```

## 5. Run Maker and Reviewer

Start each worker in its own shell or supervised pane:

```bash
AIOPS_WORKFLOW_PATH="$AIOPS_CROWDRUNNER_MAKER_WORKFLOW" \
AIOPS_MIRROR_ROOT="$AIOPS_CROWDRUNNER_MAKER_MIRROR_ROOT" \
AIOPS_SERVER_HOST=127.0.0.1 \
  "$AIOPS_CROWDRUNNER_WORKER_BIN" --port "$AIOPS_CROWDRUNNER_MAKER_PORT" \
  "$AIOPS_CROWDRUNNER_MAKER_WORKFLOW" \
  2>&1 | tee "$AIOPS_CROWDRUNNER_RUN_ROOT/logs/maker-worker.log"

AIOPS_WORKFLOW_PATH="$AIOPS_CROWDRUNNER_REVIEWER_WORKFLOW" \
AIOPS_MIRROR_ROOT="$AIOPS_CROWDRUNNER_REVIEWER_MIRROR_ROOT" \
AIOPS_SERVER_HOST=127.0.0.1 \
  "$AIOPS_CROWDRUNNER_WORKER_BIN" --port "$AIOPS_CROWDRUNNER_REVIEWER_PORT" \
  "$AIOPS_CROWDRUNNER_REVIEWER_WORKFLOW" \
  2>&1 | tee "$AIOPS_CROWDRUNNER_RUN_ROOT/logs/reviewer-worker.log"
```

After both workers are listening, validate the dashboard endpoints before
activating product issues:

```bash
AIOPS_WORKFLOW_PATH="$AIOPS_CROWDRUNNER_MAKER_WORKFLOW" \
AIOPS_MIRROR_ROOT="$AIOPS_CROWDRUNNER_MAKER_MIRROR_ROOT" \
  "$AIOPS_CROWDRUNNER_WORKER_BIN" --doctor --deploy=binary --mode=real \
  --dashboard-url "$AIOPS_CROWDRUNNER_MAKER_DASHBOARD_URL" \
  "$AIOPS_CROWDRUNNER_MAKER_WORKFLOW" \
  | tee "$AIOPS_CROWDRUNNER_RUN_ROOT/artifacts/maker-dashboard-doctor.log"

AIOPS_WORKFLOW_PATH="$AIOPS_CROWDRUNNER_REVIEWER_WORKFLOW" \
AIOPS_MIRROR_ROOT="$AIOPS_CROWDRUNNER_REVIEWER_MIRROR_ROOT" \
  "$AIOPS_CROWDRUNNER_WORKER_BIN" --doctor --deploy=binary --mode=real \
  --dashboard-url "$AIOPS_CROWDRUNNER_REVIEWER_DASHBOARD_URL" \
  "$AIOPS_CROWDRUNNER_REVIEWER_WORKFLOW" \
  | tee "$AIOPS_CROWDRUNNER_RUN_ROOT/artifacts/reviewer-dashboard-doctor.log"
```

After both dashboard doctors pass, add `aiops/todo` to issues 1-12. Use the
Gitea UI or API, then trigger a work poll:

```bash
curl -fsS -X POST -H 'X-AIOPS-Refresh: true' \
  "$AIOPS_CROWDRUNNER_MAKER_DASHBOARD_URL/api/v1/refresh"
curl -fsS -X POST -H 'X-AIOPS-Refresh: true' \
  "$AIOPS_CROWDRUNNER_REVIEWER_DASHBOARD_URL/api/v1/refresh"
```

## 6. Capture Evidence

Capture state and screenshots repeatedly:

```bash
scripts/e2e-crowdrunner-capture.py \
  --run-root "$AIOPS_CROWDRUNNER_RUN_ROOT" \
  --maker-url "$AIOPS_CROWDRUNNER_MAKER_DASHBOARD_URL" \
  --reviewer-url "$AIOPS_CROWDRUNNER_REVIEWER_DASHBOARD_URL" \
  --gitea-url "$AIOPS_CROWDRUNNER_GITEA_URL" \
  --repo-owner "$AIOPS_CROWDRUNNER_REPO_OWNER" \
  --repo-name "$AIOPS_CROWDRUNNER_REPO_NAME" \
  --tui-bin "$AIOPS_CROWDRUNNER_TUI_BIN"
```

Optional milestone freeze: run this in a separate terminal before the product
run reaches the selected count. It leaves workers and dashboards running, removes
`aiops/todo` from remaining product issues after N products reach `aiops/done`,
and writes the stop as operator milestone evidence instead of a worker failure:

```bash
scripts/e2e-crowdrunner-freeze.py \
  --run-root "$AIOPS_CROWDRUNNER_RUN_ROOT" \
  --gitea-url "$AIOPS_CROWDRUNNER_GITEA_URL" \
  --repo-owner "$AIOPS_CROWDRUNNER_REPO_OWNER" \
  --repo-name "$AIOPS_CROWDRUNNER_REPO_NAME" \
  --stop-after 10
```

To resume after the milestone report, re-add `aiops/todo` to the frozen product
issues and trigger a dashboard refresh.

For required evidence, pass explicit screenshots so missing Playwright fails
fast:

```bash
scripts/e2e-crowdrunner-capture.py \
  --run-root "$AIOPS_CROWDRUNNER_RUN_ROOT" \
  --screenshot maker-dashboard="$AIOPS_CROWDRUNNER_MAKER_DASHBOARD_URL" \
  --screenshot reviewer-dashboard="$AIOPS_CROWDRUNNER_REVIEWER_DASHBOARD_URL" \
  --screenshot gitea-issues="$AIOPS_CROWDRUNNER_GITEA_URL/$AIOPS_CROWDRUNNER_REPO_OWNER/$AIOPS_CROWDRUNNER_REPO_NAME/issues"
```

## 7. Exercise Control Scenarios

- **No-ready idle:** confirm issue 13 has no PR and never appears in maker
  running state.
- **CONTROL cancel running Codex issue:** label issue 14 `aiops/todo`, wait until maker state shows
  it running, then replace the label with `aiops/canceled`; capture before and
  after state.
- **Blocked held:** confirm issue 15 remains undispatched.
- **Continuation budget:** start the stress worker on port 4203 with
  `workflows/maker-low-turn-WORKFLOW.md`, label issue 16 with both `aiops/todo`
  and `aiops/stress`, then
  wait for local continuation-budget exhaustion. The bootstrap writes
  `state/continuation-control-expected.json`; the expected machine evidence is
  `state/stress-final.json` with `counts.blocked >= 1` and a blocked row for
  issue 16 whose `method` is `continuation_budget`. A PR, terminal label, or
  `Human Review` handoff for issue 16 means the control failed.

## 8. Final Verification

Clone final `main` into `final-verify/crowd-runner-product`, then run:

```bash
cd "$AIOPS_CROWDRUNNER_RUN_ROOT/final-verify/crowd-runner-product"
{
  printf '\n$ npm ci\n'
  npm ci
  printf '\n$ npm run lint\n'
  npm run lint
  printf '\n$ npm run test -- --run\n'
  npm run test -- --run
  printf '\n$ npm run test:e2e\n'
  npm run test:e2e
  printf '\n$ npm run build\n'
  npm run build
} | tee "$AIOPS_CROWDRUNNER_RUN_ROOT/final-verify/verification.log"
```

Capture final gameplay, mobile, performance, and boss/finale screenshots under
`final-verify/screenshots/`, with traces/videos under `final-verify/traces/`
and `final-verify/videos/`.

## 9. Generate the Report Pack

Save final state:

```bash
curl -fsS "$AIOPS_CROWDRUNNER_MAKER_DASHBOARD_URL/api/v1/state" \
  > "$AIOPS_CROWDRUNNER_RUN_ROOT/state/maker-final.json"
curl -fsS "$AIOPS_CROWDRUNNER_REVIEWER_DASHBOARD_URL/api/v1/state" \
  > "$AIOPS_CROWDRUNNER_RUN_ROOT/state/reviewer-final.json"
curl -fsS -H "Authorization: token $GITEA_TOKEN" \
  "$AIOPS_CROWDRUNNER_GITEA_URL/api/v1/repos/$AIOPS_CROWDRUNNER_REPO_OWNER/$AIOPS_CROWDRUNNER_REPO_NAME/issues?state=all" \
  > "$AIOPS_CROWDRUNNER_RUN_ROOT/state/issues-final.json"
curl -fsS -H "Authorization: token $GITEA_TOKEN" \
  "$AIOPS_CROWDRUNNER_GITEA_URL/api/v1/repos/$AIOPS_CROWDRUNNER_REPO_OWNER/$AIOPS_CROWDRUNNER_REPO_NAME/pulls?state=all" \
  > "$AIOPS_CROWDRUNNER_RUN_ROOT/state/prs-final.json"

scripts/e2e-crowdrunner-report.py --run-root "$AIOPS_CROWDRUNNER_RUN_ROOT"
```

Commit only curated, sanitized evidence: report, promotion notes, capture
index, screenshots, final verification logs, and TUI raw frames. Never commit
`env.local`, Codex auth files, downloaded binaries, cache directories, or
credential-bearing workflow files.

## Pass / Investigate Criteria

Pass when:

- all product issues reach `aiops/done`;
- product PRs merge through reviewer approval and CI;
- Rework, cancellation, no-ready, blocked, and continuation-budget controls
  behave as expected, including the report's continuation-control assertion;
- maker/reviewer dashboards are idle at closeout;
- fresh-clone npm verification and Playwright smoke pass;
- required screenshots and logs exist.

Investigate when:

- local Codex auth or sandboxing fails doctor;
- Gitea branch protection or action runner prevents auto-merge;
- reviewer leaves an issue in `Human Review` because CI has not merged yet;
- the product works visually but misses tests or performance evidence.
