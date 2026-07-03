# Local Gitea Web Todo lifecycle E2E

This runbook turns the ad-hoc "binary + local Gitea + Codex + maker/reviewer"
validation into a repeatable local operator SOP. It is intended for release
validation, demo capture, and promotion material generation. It is not a CI
required check: the run consumes real Codex quota, depends on local auth, and can
take long enough that a human should supervise it.

The shape is deliberately conservative:

- The worker binary is run as downloaded, not from the source tree.
- Local binary mode reuses the operator's Codex configuration by default. This
  matches upstream Symphony's `codex app-server` posture. Use a dedicated
  `CODEX_HOME` only when you want production-style reproducibility.
- The maker and reviewer workers are separate processes with separate
  workspace roots and mirror roots.
- Scripts prepare and capture evidence, but they do not spend Codex quota or
  mutate a real Gitea instance without the operator explicitly running those
  commands.

## What This Validates

Run this when you need evidence for the complete user-visible lifecycle:

1. A local Gitea repo is seeded with a Web Todo backlog.
2. The maker worker consumes `Todo` / `Rework`, writes code, opens PRs, and
   flips issues to `Human Review`.
3. The reviewer worker consumes `Human Review`, verifies in a fresh context,
   requests `Rework` when needed, and auto-merges clean PRs before flipping
   `Done`.
4. Control issues prove abnormal paths: no-ready idle, blocked/dependency held
   out of dispatch, and canceling a running Codex issue.
5. A fresh clone of the final app passes Go verification and browser smoke.
6. Dashboards, TUI raw frames, Gitea pages, Web UI screenshots, and report
   material are captured under one run root.

## Helper Scripts

The reusable helper entrypoints are intentionally small:

| Script | Purpose |
| --- | --- |
| `scripts/e2e-webtodo-bootstrap.sh` | Create the run-root directory skeleton, copy the maker/reviewer workflow templates, write an env template, and seed issue-body files. |
| `scripts/e2e-webtodo-capture.py` | Capture maker/reviewer state JSON, TUI raw frames, and optional Playwright screenshots into the run root. |
| `scripts/e2e-webtodo-report.py` | Generate a Markdown test report and promotion notes from the run root. |

These scripts are resumable. If the run stalls, keep the same run root and
continue from the failed phase.

## 0. Prepare the Host Environment

Do this once per machine, before creating the run root or activating issues.
Keep these tools outside the repository and outside the disposable run root.

Required host tools:

- Docker or an equivalent local Gitea runtime.
- Git, curl, Python 3, and the Go toolchain pinned by `go.mod`.
- Downloaded release `worker` and `tui` binaries for the version under test.
- A working local Codex configuration. Local binary mode normally reuses the
  operator's `~/.codex`; set a dedicated `CODEX_HOME` only for
  production-style reproducibility.

Prepare the screenshot tooling in a reusable Python virtual environment. The
capture helper uses Chromium, so it only needs that Playwright browser binary:

```bash
export AIOPS_WEBTODO_TOOLS_ROOT="${AIOPS_WEBTODO_TOOLS_ROOT:-$HOME/.cache/aiops-webtodo-e2e-tools}"
mkdir -p "$AIOPS_WEBTODO_TOOLS_ROOT"
python3 -m venv "$AIOPS_WEBTODO_TOOLS_ROOT/venv"
. "$AIOPS_WEBTODO_TOOLS_ROOT/venv/bin/activate"
python -m pip install --upgrade pip
python -m pip install playwright
python -m playwright install chromium
```

Smoke the screenshot stack before spending Codex quota:

```bash
python - <<'PY'
from playwright.sync_api import sync_playwright

with sync_playwright() as pw:
    browser = pw.chromium.launch()
    browser.close()
print("playwright chromium OK")
PY
```

On Linux hosts, if Chromium reports missing system packages, run
`python -m playwright install-deps chromium` or install the equivalent host
packages before continuing. Leave the virtual environment activated whenever
you run `scripts/e2e-webtodo-capture.py`; without Playwright, state JSON and TUI
capture still work, but screenshot evidence is skipped unless explicitly
requested.

## 1. Prepare the Run Root

Choose ports that do not collide with existing workers. The example keeps Gitea
on `3107`, maker on `4101`, and reviewer on `4102`.

```bash
RUN_ROOT=/tmp/aiops-webtodo-e2e-$(date +%Y%m%d-%H%M%S)
scripts/e2e-webtodo-bootstrap.sh \
  --run-root "$RUN_ROOT" \
  --gitea-url http://127.0.0.1:3107 \
  --repo-owner aiops-bot \
  --repo-name web-todo \
  --port-base 4100
```

The script writes:

- `env.example` with the required exports and port plan.
- `workflows/maker-WORKFLOW.md` and
  `workflows/reviewer-automerge-WORKFLOW.md`.
- `issues/*.md` with the Web Todo backlog and control scenarios.
- directories for `state/`, `artifacts/`, `logs/`, `promo/`, `reports/`,
  `mirrors/`, `workspaces/`, and `final-verify/` evidence.
- `NEXT-STEPS.md` with the command order for this run root.

Copy `env.example` to `env.local`, fill the token and clone URL values, then
source it in every terminal used for this run:

```bash
set -euo pipefail
cp "$RUN_ROOT/env.example" "$RUN_ROOT/env.local"
$EDITOR "$RUN_ROOT/env.local"
set -a
. "$RUN_ROOT/env.local"
set +a
```

Do not commit `env.local`; it contains credentials.

## 2. Create Local Gitea State

Use a disposable local Gitea instance and a low-privilege bot account. The exact
Gitea boot method can vary by host, but the target state should be stable:

- repo: `${AIOPS_WEBTODO_REPO_OWNER}/${AIOPS_WEBTODO_REPO_NAME}`
- default branch: `main`
- labels: `aiops/todo`, `aiops/in-progress`, `aiops/human-review`,
  `aiops/rework`, `aiops/done`, `aiops/canceled`
- issues: every file in `$AIOPS_WEBTODO_RUN_ROOT/issues/`, initially without
  `aiops/*` state labels

Keep every issue inactive until the dashboard doctor passes. After that gate,
activate the first ten primary issues by adding `aiops/todo`. Keep control
issues unlabeled until their phase:

- `CONTROL no-ready issue must stay idle`: no `aiops/*` label.
- `CONTROL cancel running codex issue`: add `aiops/todo` only when testing
  cancellation.
- `CONTROL blocked issue held out of ready gate`: leave unlabeled or express a
  blocker with `Depends on #N`.

Record the created issue list:

```bash
curl -fsS -H "Authorization: token $GITEA_TOKEN" \
  "$AIOPS_WEBTODO_GITEA_URL/api/v1/repos/$AIOPS_WEBTODO_REPO_OWNER/$AIOPS_WEBTODO_REPO_NAME/issues?state=all" \
  > "$AIOPS_WEBTODO_RUN_ROOT/state/issues-created.json"
```

## 3. Preflight

Run the downloaded binary, not a source build. `worker --doctor` must pass
before any issue is activated.

```bash
"$AIOPS_WEBTODO_WORKER_BIN" --version
"$AIOPS_WEBTODO_TUI_BIN" --version

AIOPS_WORKFLOW_PATH="$AIOPS_WEBTODO_MAKER_WORKFLOW" \
AIOPS_MIRROR_ROOT="$AIOPS_WEBTODO_MAKER_MIRROR_ROOT" \
  "$AIOPS_WEBTODO_WORKER_BIN" --doctor --deploy=binary --mode=real \
  "$AIOPS_WEBTODO_MAKER_WORKFLOW" \
  | tee "$AIOPS_WEBTODO_RUN_ROOT/artifacts/maker-doctor.log"

AIOPS_WORKFLOW_PATH="$AIOPS_WEBTODO_REVIEWER_WORKFLOW" \
AIOPS_MIRROR_ROOT="$AIOPS_WEBTODO_REVIEWER_MIRROR_ROOT" \
  "$AIOPS_WEBTODO_WORKER_BIN" --doctor --deploy=binary --mode=real \
  "$AIOPS_WEBTODO_REVIEWER_WORKFLOW" \
  | tee "$AIOPS_WEBTODO_RUN_ROOT/artifacts/reviewer-doctor.log"
```

For a local desktop binary run, `CODEX_HOME` normally stays unset so Codex uses
the operator's usual `~/.codex`. For a reproducible production-style run, set a
dedicated `CODEX_HOME`; the worker passes it through to `codex app-server` by
default.

## 4. Run Maker and Reviewer

Run each worker in a separate terminal, launchd unit, or tmux pane. Their roots
must differ.

```bash
# maker
AIOPS_WORKFLOW_PATH="$AIOPS_WEBTODO_MAKER_WORKFLOW" \
AIOPS_MIRROR_ROOT="$AIOPS_WEBTODO_MAKER_MIRROR_ROOT" \
AIOPS_SERVER_HOST=127.0.0.1 \
  "$AIOPS_WEBTODO_WORKER_BIN" \
  --port "$AIOPS_WEBTODO_MAKER_PORT" \
  "$AIOPS_WEBTODO_MAKER_WORKFLOW" \
  2>&1 | tee "$AIOPS_WEBTODO_RUN_ROOT/logs/maker-worker.log"

# reviewer
AIOPS_WORKFLOW_PATH="$AIOPS_WEBTODO_REVIEWER_WORKFLOW" \
AIOPS_MIRROR_ROOT="$AIOPS_WEBTODO_REVIEWER_MIRROR_ROOT" \
AIOPS_SERVER_HOST=127.0.0.1 \
  "$AIOPS_WEBTODO_WORKER_BIN" \
  --port "$AIOPS_WEBTODO_REVIEWER_PORT" \
  "$AIOPS_WEBTODO_REVIEWER_WORKFLOW" \
  2>&1 | tee "$AIOPS_WEBTODO_RUN_ROOT/logs/reviewer-worker.log"
```

After both workers are listening, validate the dashboard endpoints before you
activate issues:

```bash
AIOPS_WORKFLOW_PATH="$AIOPS_WEBTODO_MAKER_WORKFLOW" \
AIOPS_MIRROR_ROOT="$AIOPS_WEBTODO_MAKER_MIRROR_ROOT" \
  "$AIOPS_WEBTODO_WORKER_BIN" --doctor --deploy=binary --mode=real \
  --dashboard-url "$AIOPS_WEBTODO_MAKER_DASHBOARD_URL" \
  "$AIOPS_WEBTODO_MAKER_WORKFLOW" \
  | tee "$AIOPS_WEBTODO_RUN_ROOT/artifacts/maker-dashboard-doctor.log"

AIOPS_WORKFLOW_PATH="$AIOPS_WEBTODO_REVIEWER_WORKFLOW" \
AIOPS_MIRROR_ROOT="$AIOPS_WEBTODO_REVIEWER_MIRROR_ROOT" \
  "$AIOPS_WEBTODO_WORKER_BIN" --doctor --deploy=binary --mode=real \
  --dashboard-url "$AIOPS_WEBTODO_REVIEWER_DASHBOARD_URL" \
  "$AIOPS_WEBTODO_REVIEWER_WORKFLOW" \
  | tee "$AIOPS_WEBTODO_RUN_ROOT/artifacts/reviewer-dashboard-doctor.log"
```

After both dashboard doctors pass, add `aiops/todo` to issues 1-10. Use the
Gitea UI or API, then trigger a work poll:

```bash
curl -fsS -X POST -H 'X-AIOPS-Refresh: true' \
  "$AIOPS_WEBTODO_MAKER_DASHBOARD_URL/api/v1/refresh"
curl -fsS -X POST -H 'X-AIOPS-Refresh: true' \
  "$AIOPS_WEBTODO_REVIEWER_DASHBOARD_URL/api/v1/refresh"
```

## 5. Capture Evidence During the Run

Use the capture helper repeatedly at milestone moments:

```bash
scripts/e2e-webtodo-capture.py \
  --run-root "$AIOPS_WEBTODO_RUN_ROOT" \
  --maker-url "$AIOPS_WEBTODO_MAKER_DASHBOARD_URL" \
  --reviewer-url "$AIOPS_WEBTODO_REVIEWER_DASHBOARD_URL" \
  --gitea-url "$AIOPS_WEBTODO_GITEA_URL" \
  --repo-owner "$AIOPS_WEBTODO_REPO_OWNER" \
  --repo-name "$AIOPS_WEBTODO_REPO_NAME" \
  --tui-bin "$AIOPS_WEBTODO_TUI_BIN"
```

Add named screenshots for important pages:

```bash
scripts/e2e-webtodo-capture.py \
  --run-root "$AIOPS_WEBTODO_RUN_ROOT" \
  --screenshot issue-04-rework="$AIOPS_WEBTODO_GITEA_URL/$AIOPS_WEBTODO_REPO_OWNER/$AIOPS_WEBTODO_REPO_NAME/issues/4" \
  --screenshot maker-dashboard="$AIOPS_WEBTODO_MAKER_DASHBOARD_URL" \
  --screenshot reviewer-dashboard="$AIOPS_WEBTODO_REVIEWER_DASHBOARD_URL"
```

The script writes state snapshots under `state/`, TUI frames under
`promo/pages/`, screenshots under `promo/screenshots/`, and
`promo/capture-index.md`. The default screenshots are best-effort: if
Playwright is not installed, the helper still captures state JSON and TUI raw
frames and reports that default screenshots were skipped. Explicit
`--screenshot NAME=URL` captures remain required evidence and fail fast without
Playwright.

## 6. Exercise Control Scenarios

Run these after the happy path has proven the normal maker/reviewer loop:

- **No-ready idle:** confirm the no-ready issue has no PR and never appears in
  maker state.
- **Blocked/held:** confirm the blocked issue has no PR and never dispatches
  before its blocker is terminal.
- **Cancel running:** add `aiops/todo` to the cancel-control issue, wait until
  maker state shows it running, then replace the label with `aiops/canceled`.
  The next maker poll should classify an operator terminal stop and no PR should
  exist for the issue.

Capture before/after state JSON and the Gitea issue page for each control
scenario.

## 7. Final Verification

Clone the final Gitea repo into `final-verify/web-todo`, then run:

```bash
cd "$AIOPS_WEBTODO_RUN_ROOT/final-verify/web-todo"
gofmt -l .
go vet ./...
go test ./...
make test
make build
```

Record the output:

```bash
{
  printf '\n$ gofmt -l .\n'
  gofmt -l .
  printf '\n$ go vet ./...\n'
  go vet ./...
  printf '\n$ go test ./...\n'
  go test ./...
  printf '\n$ make test\n'
  make test
  printf '\n$ make build\n'
  make build
} | tee "$AIOPS_WEBTODO_RUN_ROOT/final-verify/verification.log"
```

Use Playwright or the browser tool of your choice to smoke the final app:
create todos, set an overdue due date, toggle completion, check filters, edit a
title inline, delete all todos, and save screenshots/video under
`final-verify/`.

## 8. Generate the Report Pack

At the end of the run, save final state:

```bash
curl -fsS "$AIOPS_WEBTODO_MAKER_DASHBOARD_URL/api/v1/state" \
  > "$AIOPS_WEBTODO_RUN_ROOT/state/maker-final.json"
curl -fsS "$AIOPS_WEBTODO_REVIEWER_DASHBOARD_URL/api/v1/state" \
  > "$AIOPS_WEBTODO_RUN_ROOT/state/reviewer-final.json"
curl -fsS -H "Authorization: token $GITEA_TOKEN" \
  "$AIOPS_WEBTODO_GITEA_URL/api/v1/repos/$AIOPS_WEBTODO_REPO_OWNER/$AIOPS_WEBTODO_REPO_NAME/issues?state=all" \
  > "$AIOPS_WEBTODO_RUN_ROOT/state/issues-final.json"
curl -fsS -H "Authorization: token $GITEA_TOKEN" \
  "$AIOPS_WEBTODO_GITEA_URL/api/v1/repos/$AIOPS_WEBTODO_REPO_OWNER/$AIOPS_WEBTODO_REPO_NAME/pulls?state=all" \
  > "$AIOPS_WEBTODO_RUN_ROOT/state/prs-final.json"
```

Then generate report files:

```bash
scripts/e2e-webtodo-report.py --run-root "$AIOPS_WEBTODO_RUN_ROOT"
```

Review the generated files under `reports/` and `promo/notes/`. The report
summarizes machine-readable state, then leaves the final pass/fail call to an
operator checklist because the helper cannot prove every required screenshot,
browser-smoke, and control-scenario assertion from JSON alone. Commit only a
curated evidence pack:

- Markdown report(s).
- Promotion notes and capture index.
- Useful screenshots and the final smoke video.
- TUI raw text and final verification logs.

Do not commit `env.local`, Codex auth files, downloaded binaries, cache
directories, or credential-bearing worker configs.

## Pass / Investigate Criteria

Treat the run as a pass when:

- All primary issues reach `aiops/done`.
- Every primary PR is merged.
- Control issues behave as expected.
- Maker and reviewer dashboards are idle at closeout.
- Fresh-clone Go verification passes.
- Final browser smoke passes with an empty console log.

Investigate, but do not automatically call it an aiops-platform bug, when:

- Local Codex configuration changes token use, reasoning style, or available
  skills/plugins. That is expected in local binary mode.
- A PR branch probe fails but the final fresh-main smoke passes.
- A worker shows the same issue in a short handoff overlap and then reconciles
  cleanly on the next poll.
