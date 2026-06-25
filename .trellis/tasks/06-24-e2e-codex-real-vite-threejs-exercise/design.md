# Design

## Topology

Use the existing local Gitea maker/reviewer pattern, adapted from
`examples/maker-WORKFLOW.md`,
`examples/reviewer-automerge-WORKFLOW.md`, and
`docs/runbooks/reviewer-worker.md`.

- Maker worker:
  - active states: `Todo`, `Rework`;
  - runner: `codex-app-server`;
  - verify commands: `npm ci`, `npm run lint`, `npm run test -- --run`,
    `npm run build`;
  - final action: comment PR URL, then set `Human Review`.
- Reviewer worker:
  - active state: `Human Review`;
  - runner: `codex-app-server`;
  - review-only prompt, no code edits;
  - executable checks mirror maker verify commands plus Playwright when the
    issue requires it;
  - final action: `Done` after merge confirmation, or `Rework`.
- Optional low-turn stress worker:
  - separate workflow, workspace root, and mirror root;
  - `max_turns: 1`, `max_continuation_turns: 2`;
  - used only for the continuation-budget control issue.

All workers must use separate `workspace.root` and `AIOPS_MIRROR_ROOT` values.

## Product Under Test

The disposable Gitea repository is `crowd-runner-product`. It starts from a
minimal seed repository containing the product brief and CI workflow. The real
Codex maker agents turn issue bodies into the product incrementally.

The product target is a production-grade web game rather than a demo:

- Vite + React + TypeScript + Three.js / WebGL.
- Canvas or DOM HUD over the 3D scene.
- Core game logic separated from rendering so gate math, level generation,
  collisions, combat, economy, save data, and deterministic daily challenges are
  testable without WebGL.
- Playwright smoke and visual checks for desktop and mobile portrait viewports.
- Performance instrumentation with a p95 frame-time target and adaptive quality
  behavior.

## Evidence Flow

Reusable scripts own the operator workflow:

- `scripts/e2e-crowdrunner-bootstrap.sh` creates the run root, workflow files,
  seed repo files, issue bodies, and next-step checklist.
- `scripts/e2e-crowdrunner-capture.py` captures worker state JSON, TUI raw
  frames, and optional Playwright screenshots.
- `scripts/e2e-crowdrunner-report.py` builds the final Markdown report and
  promotion notes from saved state, PR, screenshot, and verification artifacts.
- `docs/runbooks/local-gitea-crowdrunner-lifecycle-e2e.md` documents the full
  release-binary, Gitea, Codex, screenshot, and final verification SOP.

## Failure Handling

- Missing Gitea credentials or bot clone URLs block the live run, not the
  repository-side preparation.
- Missing Playwright blocks required screenshots but does not prevent JSON/TUI
  evidence capture.
- Slow CI leaves issues in `Human Review` until reviewer confirms merge.
- Repeated reviewer failures stay visible through issue comments and the report.
- Secrets are excluded from reports; only masked workflow output and evidence
  file paths are committed.
