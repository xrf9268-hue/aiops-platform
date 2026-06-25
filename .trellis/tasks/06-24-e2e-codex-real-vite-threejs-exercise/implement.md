# Implementation Plan

## Phase 1: Repository Tooling

1. Add a crowdrunner-specific bootstrap script that:
   - creates `/tmp/aiops-real-crowdrunner-e2e-<timestamp>` run roots;
   - renders maker/reviewer workflows from existing examples;
   - rewrites verify commands from Go to npm/TypeScript commands;
   - writes a minimal seed repository and product brief;
   - writes at least 16 issue files covering product and abnormal scenarios.
2. Add a capture helper that:
   - fetches maker/reviewer `/api/v1/state`;
   - captures raw TUI frames;
   - captures dashboard, Gitea, and product screenshots with Playwright when
     requested.
3. Add a report helper that:
   - loads final Gitea issue/PR JSON and worker state;
   - summarizes issue, PR, abnormal scenario, screenshot, and verification
     evidence;
   - distinguishes lifecycle, Codex delivery, and product quality verdicts.
4. Add a runbook and README link for the new SOP.
5. Add focused tests for bootstrap, report generation, capture behavior, and
   runbook discoverability.

## Phase 2: Run Root and Release Preflight

1. Create a timestamped run root with the new bootstrap script.
2. Download `aiops-platform_v0.1.9_darwin_arm64.tar.gz` and SHA256SUMS into the
   run root.
3. Verify SHA256 and GitHub attestation.
4. Extract `worker` and `tui` to the run root.
5. Capture `worker --version`, `tui --version`, `codex --version`, and
   `codex app-server --help` evidence.

## Phase 3: Live Gitea Execution

1. Source `env.local` after credentials are filled.
2. Create/seed the local Gitea repository from the run-root seed repo.
3. Create required `aiops/*` labels and issues from `issues/*.md`.
4. Run maker and reviewer `worker --doctor --deploy=binary --mode=real`.
5. Start maker and reviewer workers with distinct mirror/workspace roots.
6. Trigger refresh and capture start, mid-run, abnormal, and final evidence.
7. Run the low-turn stress worker for the continuation-budget scenario.

## Phase 4: Final Verification

1. Fresh clone final `main` into `final-verify/crowd-runner-product`.
2. Run:
   - `npm ci`
   - `npm run lint`
   - `npm run test -- --run`
   - `npm run test:e2e`
   - `npm run build`
3. Capture Playwright desktop and mobile screenshots, canvas nonblank checks,
   gameplay smoke, boss/finale state, and performance summary.
4. Generate the report pack.
5. Stop workers and leave logs, state, screenshots, and reports in the run root.
