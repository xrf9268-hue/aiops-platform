# real-codex-crowdrunner-product-e2e

## Goal

Run a production-style end-to-end validation of the latest aiops-platform
release binary with real `codex app-server`, using a local Gitea maker/reviewer
topology to build a new 3D crowd-runner product from issues.

## Requirements

- Use the latest GitHub release binary under test: `v0.1.9`, asset
  `aiops-platform_v0.1.9_darwin_arm64.tar.gz` on this host.
- Verify the downloaded release with SHA256 and `gh attestation verify`.
- Use local Gitea with two real workers:
  - maker: real Codex implements `Todo` / `Rework`, opens one PR per issue, and
    stops at `Human Review`.
  - reviewer: real Codex reviews in a fresh context, sends failures to `Rework`,
    or approves and enables CI-gated auto-merge before setting `Done`.
- Create a new disposable repository named `crowd-runner-product`.
- Do not reuse the old private `crowdrunner` implementation. Treat it only as
  historical context for what not to repeat.
- Design the product from the real commercial crowd-runner shape:
  - best gate selection and crowd multiplication;
  - runner control and portrait-first 3D camera;
  - traps, pits, and obstacle avoidance;
  - enemy crowd clashes;
  - coins, upgrades, skins, save/progression;
  - castle / King finale;
  - performance and mobile acceptance.
- Seed at least 14 Gitea issues:
  - at least 10 product-building issues;
  - at least 3 abnormal/control scenarios;
  - one issue must intentionally exercise `Rework`;
  - one scenario must exercise reconcile cancellation;
  - one scenario must exercise continuation / turn-budget behavior.
- Capture evidence under `/tmp/aiops-real-crowdrunner-e2e-<timestamp>`.
- Produce a final report that separates:
  - aiops-platform lifecycle verdict;
  - Codex product-delivery verdict;
  - final product quality verdict.

## Acceptance Criteria

- [ ] Release binary provenance and checksum evidence is saved.
- [ ] Maker and reviewer `worker --doctor --deploy=binary --mode=real` logs are saved.
- [ ] `/livez`, `/readyz`, and `/api/v1/state` snapshots are captured at start,
      mid-run, and closeout.
- [ ] At least 10 product issues are consumed by real Codex.
- [ ] At least one issue reaches `Rework` and then either converges or is
      clearly reported as blocked with reviewer evidence.
- [ ] A running issue is canceled or made ineligible and the worker records a
      reconcile/operator stop rather than a silent success.
- [ ] A continuation / turn-budget scenario is captured with the worker reason.
- [ ] Final fresh clone runs `npm ci`, `npm run lint`, `npm run test -- --run`,
      `npm run test:e2e`, and `npm run build`.
- [ ] Playwright captures at least 8 screenshots, including Gitea issue state,
      maker/reviewer dashboards, a Rework issue, a merged PR, gameplay, boss or
      finale, and mobile/performance views.
- [ ] The report references all evidence paths and gives an issue-by-issue
      result table.
