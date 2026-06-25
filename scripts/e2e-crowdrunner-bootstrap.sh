#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
run_root="${AIOPS_CROWDRUNNER_RUN_ROOT:-}"
gitea_url="${AIOPS_CROWDRUNNER_GITEA_URL:-http://127.0.0.1:3107}"
repo_owner="${AIOPS_CROWDRUNNER_REPO_OWNER:-aiops-bot}"
repo_name="${AIOPS_CROWDRUNNER_REPO_NAME:-crowd-runner-product}"
port_base="${AIOPS_CROWDRUNNER_PORT_BASE:-4200}"
release_tag="${AIOPS_CROWDRUNNER_RELEASE_TAG:-v0.1.9}"

usage() {
  printf 'usage: %s --run-root DIR [--gitea-url URL] [--repo-owner NAME] [--repo-name NAME] [--port-base PORT] [--release-tag TAG]\n' "$0" >&2
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --run-root)
      run_root="${2:-}"; shift 2 ;;
    --gitea-url)
      gitea_url="${2:-}"; shift 2 ;;
    --repo-owner)
      repo_owner="${2:-}"; shift 2 ;;
    --repo-name)
      repo_name="${2:-}"; shift 2 ;;
    --port-base)
      port_base="${2:-}"; shift 2 ;;
    --release-tag)
      release_tag="${2:-}"; shift 2 ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      usage; exit 2 ;;
  esac
done

if [ -z "$run_root" ]; then
  printf -- '--run-root is required\n' >&2
  usage
  exit 2
fi
case "$port_base" in
  *[!0-9]*|"")
    printf -- '--port-base must be numeric, got %s\n' "$port_base" >&2
    exit 2 ;;
esac

gitea_url="${gitea_url%/}"
maker_port=$((port_base + 1))
reviewer_port=$((port_base + 2))
stress_port=$((port_base + 3))

case "$gitea_url" in
  http://*)
    clone_host="${gitea_url#http://}"
    maker_clone_url="http://maker-bot:REPLACE_ME@$clone_host/$repo_owner/$repo_name.git"
    reviewer_clone_url="http://review-bot:REPLACE_ME@$clone_host/$repo_owner/$repo_name.git"
    ;;
  https://*)
    clone_host="${gitea_url#https://}"
    maker_clone_url="https://maker-bot:REPLACE_ME@$clone_host/$repo_owner/$repo_name.git"
    reviewer_clone_url="https://review-bot:REPLACE_ME@$clone_host/$repo_owner/$repo_name.git"
    ;;
  *)
    maker_clone_url="http://maker-bot:REPLACE_ME@$gitea_url/$repo_owner/$repo_name.git"
    reviewer_clone_url="http://review-bot:REPLACE_ME@$gitea_url/$repo_owner/$repo_name.git"
    ;;
esac

mkdir -p \
  "$run_root/artifacts" \
  "$run_root/bin" \
  "$run_root/downloads" \
  "$run_root/workflows" \
  "$run_root/issues" \
  "$run_root/logs" \
  "$run_root/state" \
  "$run_root/promo/screenshots" \
  "$run_root/promo/pages" \
  "$run_root/promo/notes" \
  "$run_root/final-verify/screenshots" \
  "$run_root/final-verify/videos" \
  "$run_root/final-verify/traces" \
  "$run_root/final-verify/playwright-report" \
  "$run_root/reports" \
  "$run_root/workdirs/maker" \
  "$run_root/workdirs/reviewer" \
  "$run_root/workdirs/stress" \
  "$run_root/workspaces/maker" \
  "$run_root/workspaces/reviewer" \
  "$run_root/workspaces/stress" \
  "$run_root/mirrors/maker" \
  "$run_root/mirrors/reviewer" \
  "$run_root/mirrors/stress" \
  "$run_root/seed-repo/.gitea/workflows" \
  "$run_root/seed-repo/docs/product" \
  "$run_root/seed-repo/scripts"

render_workflow() {
  local src="$1"
  local dest="$2"
  local workspace_root="$3"
  local mode="${4:-normal}"
  local skip_next_go_test=0
  while IFS= read -r line || [ -n "$line" ]; do
    if [ "$skip_next_go_test" -eq 1 ]; then
      skip_next_go_test=0
      case "$line" in
        "    - go test ./...")
          continue ;;
      esac
    fi
    case "$line" in
      "  owner: your-gitea-user")
        printf '  owner: %s\n' "$repo_owner" ;;
      "  name: your-repo")
        printf '  name: %s\n' "$repo_name" ;;
      "  endpoint: http://gitea.local")
        printf '  endpoint: %s\n' "$gitea_url" ;;
      "  clone_url: \$MAKER_CLONE_URL  #"*)
        printf '  clone_url: $MAKER_CLONE_URL  # set in env.local\n' ;;
      "  clone_url: \$REVIEWER_CLONE_URL  #"*)
        printf '  clone_url: $REVIEWER_CLONE_URL  # set in env.local\n' ;;
      "  root: ~/aiops-workspaces/maker"|"  root: ~/aiops-workspaces/reviewer")
        printf '  root: %s\n' "$workspace_root" ;;
      "    - go build ./...")
        printf '    - npm ci\n'
        printf '    - npm run lint\n'
        printf '    - npm run test -- --run\n'
        printf '    - npm run build\n'
        skip_next_go_test=1 ;;
      "  max_turns: 30")
        if [ "$mode" = "stress" ]; then
          printf '  max_turns: 1\n'
          printf '  max_continuation_turns: 2\n'
        else
          printf '%s\n' "$line"
        fi ;;
      "    - Todo")
        if [ "$mode" = "stress" ]; then
          printf '    - Stress\n'
        else
          printf '%s\n' "$line"
        fi ;;
      "    - Rework")
        if [ "$mode" = "stress" ]; then
          continue
        else
          printf '%s\n' "$line"
        fi ;;
      *)
        line="${line//go build .\/.../npm ci, npm run lint, npm run test -- --run, and npm run build}"
        line="${line//go test .\/.../npm run test -- --run}"
        printf '%s\n' "$line" ;;
    esac
  done <"$src" >"$dest"
}

render_workflow "$repo_root/examples/maker-WORKFLOW.md" "$run_root/workflows/maker-WORKFLOW.md" "$run_root/workspaces/maker" normal
render_workflow "$repo_root/examples/reviewer-automerge-WORKFLOW.md" "$run_root/workflows/reviewer-automerge-WORKFLOW.md" "$run_root/workspaces/reviewer" normal
render_workflow "$repo_root/examples/maker-WORKFLOW.md" "$run_root/workflows/maker-low-turn-WORKFLOW.md" "$run_root/workspaces/stress" stress

cat >"$run_root/seed-repo/package.json" <<'EOF'
{
  "name": "crowd-runner-product",
  "version": "0.0.0",
  "private": true,
  "type": "module",
  "scripts": {
    "lint": "node scripts/placeholder-check.mjs",
    "test": "node scripts/placeholder-check.mjs",
    "build": "node scripts/placeholder-check.mjs"
  }
}
EOF

cat >"$run_root/seed-repo/package-lock.json" <<'EOF'
{
  "name": "crowd-runner-product",
  "version": "0.0.0",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": {
      "name": "crowd-runner-product",
      "version": "0.0.0"
    }
  }
}
EOF

cat >"$run_root/seed-repo/scripts/placeholder-check.mjs" <<'EOF'
console.log('placeholder check passes until issue #1 installs the real Vite app');
EOF

cat >"$run_root/seed-repo/.gitignore" <<'EOF'
node_modules/
dist/
coverage/
playwright-report/
test-results/
.env
.env.*
EOF

cat >"$run_root/seed-repo/.gitea/workflows/ci.yml" <<'EOF'
name: ci
on:
  pull_request:
    branches: [main]
jobs:
  build-test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
        with:
          node-version: '24'
          cache: npm
      - run: npm ci
      - run: npm run lint
      - run: npm run test -- --run
      - run: npm run build
EOF

cat >"$run_root/seed-repo/README.md" <<'EOF'
# Crowd Runner Product

This disposable repository is built by real Codex agents during the
aiops-platform release-binary lifecycle exercise.

The target is a production-quality mobile-first 3D crowd-runner web game:
gate math, crowd simulation, traps, enemy clashes, upgrades, deterministic
daily challenges, castle finale, performance instrumentation, and Playwright
acceptance. Do not copy code, assets, or design from the previous private
crowdrunner implementation.
EOF

cat >"$run_root/seed-repo/docs/product/brief.md" <<'EOF'
# Product Brief

## Reference Product Shape

The product borrows genre mechanics, not protected assets or branding:

- choose the best gate to multiply or add runners;
- guide a portrait-first crowd through traps, pits, and narrow lanes;
- resolve enemy crowd clashes by count and formation;
- collect coins and spend them on start size, income, skins, and themes;
- end levels with a castle / King finale;
- keep the core loop understandable in seconds, then deepen through progression.

## Quality Bar

- Core logic is testable without WebGL.
- The Three.js scene is nonblank and responsive on mobile portrait.
- Final verification includes unit tests, Playwright, build, screenshots, and a
  frame-time/performance summary.
- The game must be a real product foundation, not a one-screen demo.
EOF

write_issue() {
  local number="$1"
  local slug="$2"
  local title="$3"
  local body="$4"
  local file="$run_root/issues/${number}-${slug}.md"
  {
    printf '# %s\n\n' "$title"
    printf '%s\n' "$body"
  } >"$file"
}

write_issue 01 product-foundation "01 product foundation and real Vite game architecture" \
"Acceptance:
- Replace the placeholder package with a real Vite + React + TypeScript app.
- Add Three.js and a portrait-first full-canvas game shell.
- Add docs/product/GDD.md with product pillars, loop, controls, economy, visual style, and non-goals.
- Create src/game/core, src/game/rendering, src/game/systems, src/ui, and test folders with clear boundaries.
- Configure npm scripts: lint, test, test:e2e, build, preview.
- Add at least one unit test and one Playwright smoke that proves the canvas renders nonblank pixels."

write_issue 02 runner-loop-controls "02 runner loop, camera, and mobile controls" \
"Depends on #1

Acceptance:
- Implement start, running, paused, win, and fail states.
- Add pointer/touch drag lane controls and keyboard fallback.
- Camera follows the crowd in portrait view without clipping the track.
- HUD shows level, crowd count, coins, distance, and state.
- Add unit tests for state transitions and Playwright coverage for pointer drag."

write_issue 03 procedural-level-gates "03 deterministic levels and gate math" \
"Depends on #2

Acceptance:
- Generate deterministic levels from a seed and level number.
- Add add, subtract, multiply, divide, and clone gates with clear visual labels.
- Gate choice mutates crowd count through pure core logic.
- Ensure generated levels always contain at least one beneficial path.
- Add tests for seed stability, gate math, and impossible-level prevention."

write_issue 04 crowd-renderer-formation "04 instanced crowd renderer and formation simulation" \
"Depends on #3

Acceptance:
- Render at least 200 runners using an efficient instancing or pooled mesh approach.
- Crowd formation should compress and expand without hiding gate choices.
- Add runner color/theme hooks without per-frame React re-rendering.
- Capture frame-time samples in runtime diagnostics.
- Add tests or Playwright evidence proving 200 runners render nonblank and remain responsive."

write_issue 05 traps-pits-obstacles "05 traps, pits, and obstacle collision outcomes" \
"Depends on #4

Acceptance:
- Add at least four obstacle families: rotating bars, crushers, pits, and moving blockers.
- Collisions remove runners predictably, not randomly.
- Pit avoidance and edge loss are visible and recoverable.
- Add level fixtures that exercise each obstacle family.
- Add unit tests for collision outcomes and Playwright screenshots for two obstacle types."

write_issue 06 enemy-crowd-combat "06 enemy crowd clashes and battle resolution" \
"Depends on #5

Acceptance:
- Add red enemy groups with count labels and route placement.
- Resolve clashes by count while preserving survivors and state.
- Show battle feedback without freezing controls longer than needed.
- Failing a fight transitions to fail state with restart.
- Add tests for win/loss/equal-count combat cases."

write_issue 07 rework-quality-gate "07 [EXPECT-REWORK] behavior tests for gates and collisions" \
"Depends on #6

Acceptance:
- Strengthen tests so deleting gate application or obstacle collision handling fails.
- Tests must execute behavior, not only search source strings or assert static markup.
- Add at least one Playwright or Vitest scenario that drives a real level fixture through gate + obstacle + combat.
- Reviewer should reject placebo coverage and request Rework if tests would still pass after removing the behavior."

write_issue 08 coins-upgrades-economy "08 coins, upgrades, and balanced progression" \
"Depends on #7

Acceptance:
- Award coins from level performance and finale result.
- Add upgrades for start crowd, coin multiplier, and revive chance.
- Persist economy and upgrades in localStorage behind a versioned save schema.
- Add balance caps so upgrades cannot make every level trivial.
- Add tests for save migration, purchase validation, and reward calculation."

write_issue 09 castle-finale-boss "09 castle finale and King battle" \
"Depends on #8

Acceptance:
- End each level with a castle or King encounter that consumes remaining crowd strength.
- Show satisfying but lightweight Three.js feedback for castle damage and win/fail result.
- Every fifth level should use a stronger boss/finale layout.
- Add deterministic tests for finale thresholds.
- Add Playwright screenshot coverage for a boss or castle win state."

write_issue 10 daily-save-skins "10 daily challenge, skins, themes, and save UX" \
"Depends on #9

Acceptance:
- Add deterministic daily challenge from local date.
- Add at least six unlockable skins or color themes using generated materials, not external assets.
- Add shop/progression screens that are usable on mobile portrait.
- Add save reset/export/import for debugging with clear confirmation.
- Add tests for daily seed stability and save import validation."

write_issue 11 performance-adaptive-quality "11 performance mode, p95 frame budget, and canvas validation" \
"Depends on #10

Acceptance:
- Add a lightweight performance overlay or diagnostics panel.
- Measure p95 frame time over a representative run and expose it to tests.
- Add adaptive quality toggles for shadows, runner detail, and effects.
- Playwright must verify the canvas is nonblank, the HUD is readable, and p95 stays under the documented budget on the test machine.
- Document the performance budget in docs/product/PERFORMANCE.md."

write_issue 12 release-polish-pwa "12 product polish, PWA metadata, and final smoke" \
"Depends on #11

Acceptance:
- Add app metadata, icons generated in-repo, and PWA manifest.
- Add README instructions for install, run, test, build, and gameplay.
- Add final Playwright smoke that covers start -> gate -> obstacle -> fight -> finale.
- Add screenshot capture instructions for product marketing/evidence.
- Ensure npm ci, lint, test, test:e2e, and build pass from a fresh clone."

write_issue 13 control-no-ready "CONTROL no-ready issue must stay idle" \
"Control scenario:
- Do not add an aiops/* label.
- The maker worker must not dispatch this issue.
- No PR should be created."

write_issue 14 control-cancel-running "CONTROL cancel running Codex issue" \
"Control scenario:
- Add aiops/todo only when ready to test cancellation.
- Wait for maker state to show this issue running.
- Replace the label with aiops/canceled.
- The maker should stop the active run through reconcile/operator terminal handling.
- No PR should be created for this control issue."

write_issue 15 control-blocked-held "CONTROL blocked issue held out of ready gate" \
"Control scenario:
- Add this issue with a dependency on a non-terminal issue or leave it unlabeled.
- It should not dispatch during the primary lifecycle.
- No PR should be created."

write_issue 16 control-continuation-budget "CONTROL continuation budget stress worker" \
"Control scenario:
- Run only with workflows/maker-low-turn-WORKFLOW.md on the Stress state.
- Label this issue aiops/stress, not aiops/todo.
- The task intentionally asks for broad product analysis plus implementation so one turn is unlikely to complete.
- Capture runner timeout, continuation, clean-turn budget, or local blocked state evidence from the stress worker.
- Do not mix this worker with the main maker/reviewer evidence."

cat >"$run_root/env.example" <<EOF
# Copy to env.local, fill secrets, then source it with set -a.
export AIOPS_CROWDRUNNER_RUN_ROOT="$run_root"
export AIOPS_CROWDRUNNER_RELEASE_TAG="$release_tag"
export AIOPS_CROWDRUNNER_TOOLS_ROOT="\$HOME/.cache/aiops-crowdrunner-e2e-tools"
export AIOPS_CROWDRUNNER_GITEA_URL="$gitea_url"
export AIOPS_CROWDRUNNER_REPO_OWNER="$repo_owner"
export AIOPS_CROWDRUNNER_REPO_NAME="$repo_name"

export GITEA_TOKEN="REPLACE_ME"
export MAKER_CLONE_URL="$maker_clone_url"
export REVIEWER_CLONE_URL="$reviewer_clone_url"

export AIOPS_CROWDRUNNER_WORKER_BIN="$run_root/bin/worker"
export AIOPS_CROWDRUNNER_TUI_BIN="$run_root/bin/tui"

export AIOPS_CROWDRUNNER_MAKER_PORT="$maker_port"
export AIOPS_CROWDRUNNER_REVIEWER_PORT="$reviewer_port"
export AIOPS_CROWDRUNNER_STRESS_PORT="$stress_port"
export AIOPS_CROWDRUNNER_MAKER_DASHBOARD_URL="http://127.0.0.1:$maker_port"
export AIOPS_CROWDRUNNER_REVIEWER_DASHBOARD_URL="http://127.0.0.1:$reviewer_port"
export AIOPS_CROWDRUNNER_STRESS_DASHBOARD_URL="http://127.0.0.1:$stress_port"

export AIOPS_CROWDRUNNER_MAKER_WORKFLOW="$run_root/workflows/maker-WORKFLOW.md"
export AIOPS_CROWDRUNNER_REVIEWER_WORKFLOW="$run_root/workflows/reviewer-automerge-WORKFLOW.md"
export AIOPS_CROWDRUNNER_STRESS_WORKFLOW="$run_root/workflows/maker-low-turn-WORKFLOW.md"
export AIOPS_CROWDRUNNER_MAKER_MIRROR_ROOT="$run_root/mirrors/maker"
export AIOPS_CROWDRUNNER_REVIEWER_MIRROR_ROOT="$run_root/mirrors/reviewer"
export AIOPS_CROWDRUNNER_STRESS_MIRROR_ROOT="$run_root/mirrors/stress"

# Optional: set this for reproducible production-style Codex runs.
# export CODEX_HOME="$run_root/codex-home"
EOF

cat >"$run_root/NEXT-STEPS.md" <<EOF
# Crowd Runner product lifecycle E2E next steps

1. Install or verify host tools from the runbook, including Playwright Chromium
   under \`AIOPS_CROWDRUNNER_TOOLS_ROOT\`.
2. Copy \`env.example\` to \`env.local\`, fill secrets, and source it.
3. Download and verify the $release_tag release archive into \`downloads/\`,
   then extract \`worker\` and \`tui\` into \`bin/\`.
4. Create the local Gitea repo from \`seed-repo/\`.
5. Create labels and issues from \`issues/*.md\`; initially label product issues
   01-12 with \`aiops/todo\`, keep controls staged as described in their bodies.
6. Run maker/reviewer \`worker --doctor --deploy=binary --mode=real\`.
7. Start maker on port $maker_port and reviewer on port $reviewer_port.
8. Use \`scripts/e2e-crowdrunner-capture.py\` at start, mid-run, Rework,
   cancellation, stress, and final milestones.
9. Fresh-clone final main into \`final-verify/crowd-runner-product\` and run
   npm verification plus Playwright smoke.
10. Run \`scripts/e2e-crowdrunner-report.py --run-root "$run_root"\`.

See \`docs/runbooks/local-gitea-crowdrunner-lifecycle-e2e.md\` for the full SOP.
EOF

printf 'Prepared Crowd Runner lifecycle run root: %s\n' "$run_root"
printf 'Edit and source: %s/env.local\n' "$run_root"
printf 'Seed repository: %s/seed-repo\n' "$run_root"
printf 'Issue seed files: %s/issues\n' "$run_root"
