#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
run_root="${AIOPS_GHMR_RUN_ROOT:-}"
repo="${AIOPS_GHMR_REPO:-}"
port_base="${AIOPS_GHMR_PORT_BASE:-4300}"

usage() {
  printf 'usage: %s --run-root DIR --repo OWNER/NAME [--port-base PORT]\n' "$0" >&2
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --run-root)
      run_root="${2:-}"; shift 2 ;;
    --repo)
      repo="${2:-}"; shift 2 ;;
    --port-base)
      port_base="${2:-}"; shift 2 ;;
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
case "$repo" in
  */*) ;;
  *)
    printf -- '--repo must be OWNER/NAME, got %s\n' "$repo" >&2
    exit 2 ;;
esac
case "$port_base" in
  *[!0-9]*|"")
    printf -- '--port-base must be numeric, got %s\n' "$port_base" >&2
    exit 2 ;;
esac

owner="${repo%%/*}"
name="${repo#*/}"
maker_port=$((port_base + 1))
reviewer_port=$((port_base + 2))
clone_url="https://github.com/$repo.git"

mkdir -p \
  "$run_root/bin" \
  "$run_root/downloads" \
  "$run_root/workflows" \
  "$run_root/profiles/maker" \
  "$run_root/profiles/reviewer" \
  "$run_root/logs" \
  "$run_root/state" \
  "$run_root/screenshots" \
  "$run_root/forge-json" \
  "$run_root/final-verify/app" \
  "$run_root/final-verify/logs" \
  "$run_root/final-verify/screenshots" \
  "$run_root/reports" \
  "$run_root/tools" \
  "$run_root/secrets/gh/setup" \
  "$run_root/secrets/gh/maker" \
  "$run_root/secrets/gh/reviewer" \
  "$run_root/workspaces/maker" \
  "$run_root/workspaces/reviewer" \
  "$run_root/mirrors/maker" \
  "$run_root/mirrors/reviewer" \
  "$run_root/issues" \
  "$run_root/artifacts" \
  "$run_root/seed-repo"
chmod 700 "$run_root/secrets" "$run_root/secrets/gh" "$run_root/secrets/gh/setup" "$run_root/secrets/gh/maker" "$run_root/secrets/gh/reviewer"

render_workflow() {
  local src="$1"
  local dest="$2"
  local workspace_root="$3"
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      "  owner: your-github-owner")
        printf '  owner: %s\n' "$owner" ;;
      "  name: your-repo")
        printf '  name: %s\n' "$name" ;;
      "  clone_url: \$AIOPS_GITHUB_REPO_CLONE_URL")
        printf '  clone_url: $AIOPS_GITHUB_REPO_CLONE_URL\n' ;;
      "  root: ~/aiops-workspaces/github-maker"|"  root: ~/aiops-workspaces/github-reviewer")
        printf '  root: %s\n' "$workspace_root" ;;
      *)
        printf '%s\n' "$line" ;;
    esac
  done <"$src" >"$dest"
}

render_workflow "$repo_root/examples/github-maker-WORKFLOW.md" "$run_root/workflows/github-maker-WORKFLOW.md" "$run_root/workspaces/maker"
render_workflow "$repo_root/examples/github-reviewer-automerge-WORKFLOW.md" "$run_root/workflows/github-reviewer-automerge-WORKFLOW.md" "$run_root/workspaces/reviewer"

cp "$repo_root/scripts/github-maker-reviewer-release-preflight.sh" "$run_root/tools/release-preflight.sh"
cp "$repo_root/scripts/github-maker-reviewer-capture.py" "$run_root/tools/capture.py"
cp "$repo_root/scripts/github-maker-reviewer-final-verify.py" "$run_root/tools/final-verify.py"
cp "$repo_root/scripts/github-maker-reviewer-report.py" "$run_root/tools/report.py"

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

write_issue 01 happy-path-filters "Happy path: persistent filter tabs" \
"Acceptance:
- Add filter tabs for All, Active, and Completed todos.
- The selected filter is reflected in the URL hash.
- Changing filters must not refetch todos from the API.
- Add behavior-level tests that fail if hash filtering is removed.

Governance:
- Maker opens a PR with a non-closing issue reference.
- Reviewer approves, enables CI-gated auto-merge, confirms merged, then marks Done and closes."

write_issue 02 rework-candidate-offline-delete "Rework candidate: stale offline delete guard" \
"Depends on #1

Acceptance:
- Queue delete operations while offline and replay them when back online.
- If a stale delete acknowledgement arrives for an id that has since been
  replaced, ignore it instead of deleting the replacement todo.
- Add executable tests through the real queue/state path. Static source-string
  or markup checks do not count.

Expected review behavior:
- The first maker attempt may miss the stale acknowledgement proof.
- Reviewer must request Rework if that proof is absent, then pass after maker
  adds the executable regression."

write_issue 03 dependency-sequencing-bulk-actions "Dependency: bulk complete active todos" \
"Depends on #2

Acceptance:
- Add a bulk action that completes all currently active todos.
- The implementation must build on the offline delete guard merged for #2.
- Add tests proving completed todos are not included in the bulk action.

Sequencing check:
- Do not add aiops:todo until #1 and #2 are Done/closed."

write_issue 04 rework-control-forced-proof "Control Rework: forced stale delete proof" \
"Control scenario for deterministic Rework if issue #2 passes first review.

Acceptance:
- The reviewer must fail the first reviewed PR head unless it contains
  src/reworkControl.test.ts with a test named:
  ignores stale delete acknowledgements for replaced todo ids
- The test must drive the real queue/state path rather than a static
  source-string or markup-only check.
- After the maker adds the proof on a new head, reviewer may approve and
  auto-merge through the normal CI gate."

cat >"$run_root/env.example" <<EOF
# Copy to env.local, fill secrets/account names, then source it with set -a.
export AIOPS_GHMR_RUN_ROOT="$run_root"
export AIOPS_GHMR_REPO="$repo"
export AIOPS_GHMR_OWNER="$owner"
export AIOPS_GHMR_NAME="$name"
export AIOPS_GITHUB_REPO_CLONE_URL="$clone_url"

# Worker-held tracker token. This is denied from agent env_passthrough.
export GITHUB_TOKEN="REPLACE_ME_TRACKER_TOKEN"

export AIOPS_GHMR_WORKER_BIN="$run_root/bin/worker"
export AIOPS_GHMR_TUI_BIN="$run_root/bin/tui"
export AIOPS_GHMR_MAKER_WORKFLOW="$run_root/workflows/github-maker-WORKFLOW.md"
export AIOPS_GHMR_REVIEWER_WORKFLOW="$run_root/workflows/github-reviewer-automerge-WORKFLOW.md"
export AIOPS_GHMR_MAKER_MIRROR_ROOT="$run_root/mirrors/maker"
export AIOPS_GHMR_REVIEWER_MIRROR_ROOT="$run_root/mirrors/reviewer"

export AIOPS_GHMR_SETUP_GH_CONFIG_DIR="$run_root/secrets/gh/setup"
export AIOPS_GHMR_MAKER_GH_CONFIG_DIR="$run_root/secrets/gh/maker"
export AIOPS_GHMR_REVIEWER_GH_CONFIG_DIR="$run_root/secrets/gh/reviewer"
export AIOPS_GHMR_MAKER_LOGIN="REPLACE_ME_MAKER_LOGIN"
export AIOPS_GHMR_REVIEWER_LOGIN="REPLACE_ME_REVIEWER_LOGIN"

export AIOPS_GHMR_MAKER_PORT="$maker_port"
export AIOPS_GHMR_REVIEWER_PORT="$reviewer_port"
export AIOPS_GHMR_MAKER_DASHBOARD_URL="http://127.0.0.1:$maker_port"
export AIOPS_GHMR_REVIEWER_DASHBOARD_URL="http://127.0.0.1:$reviewer_port"

export NPM_CONFIG_CACHE="$run_root/state/npm-cache"
export PLAYWRIGHT_BROWSERS_PATH="$run_root/state/playwright-browsers"
EOF

cat >"$run_root/NEXT-STEPS.md" <<EOF
# GitHub maker/reviewer auto-merge E2E next steps

Run root: \`$run_root\`
Repository: \`$repo\`

1. Copy \`env.example\` to \`env.local\`, fill the three GitHub identities, then
   source it with \`set -a; . "\$AIOPS_GHMR_RUN_ROOT/env.local"; set +a\`.
2. Authenticate each role into its own file-backed gh home:
   - \`GH_CONFIG_DIR="\$AIOPS_GHMR_SETUP_GH_CONFIG_DIR" gh auth login\`
   - \`GH_CONFIG_DIR="\$AIOPS_GHMR_MAKER_GH_CONFIG_DIR" gh auth login\`
   - \`GH_CONFIG_DIR="\$AIOPS_GHMR_REVIEWER_GH_CONFIG_DIR" gh auth login\`
   - run \`gh auth setup-git\` once for each \`GH_CONFIG_DIR\`.
3. Run \`tools/release-preflight.sh --run-root "\$AIOPS_GHMR_RUN_ROOT"\`.
   This resolves the latest release, downloads worker/tui/SHA/SBOM, verifies
   checksum and attestation, records versions, and runs doctor when workflows
   are present.
4. Seed the disposable Vite React TypeScript Web Todo repo on \`main\`.
   GitHub Actions must run \`npm ci\`, \`npm test\`, \`npm run build\`, and
   \`npm run test:e2e\` in a required check named \`build-test\`.
5. Enable branch protection on \`main\`: required check \`build-test\`,
   one approving review, stale review dismissal, last-push approval, enforced
   admins, squash merge only, and repository auto-merge enabled.
6. Create labels \`aiops:todo\`, \`aiops:rework\`, \`aiops:human-review\`,
   \`aiops:done\`, and \`aiops:canceled\`.
7. Create issues from \`issues/*.md\` without ready labels. Activate #1 first by
   adding \`aiops:todo\`; activate downstream issues only after dependencies are
   Done/closed.
8. Start maker:
   \`GH_CONFIG_DIR="\$AIOPS_GHMR_MAKER_GH_CONFIG_DIR" AIOPS_MIRROR_ROOT="\$AIOPS_GHMR_MAKER_MIRROR_ROOT" AIOPS_EXPECTED_GITHUB_LOGIN="\$AIOPS_GHMR_MAKER_LOGIN" "\$AIOPS_GHMR_WORKER_BIN" --port "\$AIOPS_GHMR_MAKER_PORT" "\$AIOPS_GHMR_MAKER_WORKFLOW"\`
9. Start reviewer:
   \`GH_CONFIG_DIR="\$AIOPS_GHMR_REVIEWER_GH_CONFIG_DIR" AIOPS_MIRROR_ROOT="\$AIOPS_GHMR_REVIEWER_MIRROR_ROOT" AIOPS_EXPECTED_GITHUB_LOGIN="\$AIOPS_GHMR_REVIEWER_LOGIN" "\$AIOPS_GHMR_WORKER_BIN" --port "\$AIOPS_GHMR_REVIEWER_PORT" "\$AIOPS_GHMR_REVIEWER_WORKFLOW"\`
10. Capture evidence at every key transition:
    \`tools/capture.py --run-root "\$AIOPS_GHMR_RUN_ROOT" --repo "\$AIOPS_GHMR_REPO" --tag preflight --maker-url "\$AIOPS_GHMR_MAKER_DASHBOARD_URL" --reviewer-url "\$AIOPS_GHMR_REVIEWER_DASHBOARD_URL"\`
11. After all issues close, run \`tools/final-verify.py --run-root "\$AIOPS_GHMR_RUN_ROOT" --repo "\$AIOPS_GHMR_REPO"\`.
12. Generate reports:
    \`tools/report.py --run-root "\$AIOPS_GHMR_RUN_ROOT" --repo "\$AIOPS_GHMR_REPO"\`

Do not commit \`env.local\`, \`secrets/\`, downloaded binaries, raw auth homes,
or disposable run evidence.
EOF

printf 'prepared GitHub maker/reviewer E2E run root: %s\n' "$run_root"
printf 'next: cp %s/env.example %s/env.local\n' "$run_root" "$run_root"
