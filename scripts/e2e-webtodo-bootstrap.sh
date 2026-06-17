#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
run_root="${AIOPS_WEBTODO_RUN_ROOT:-}"
gitea_url="${AIOPS_WEBTODO_GITEA_URL:-http://127.0.0.1:3107}"
repo_owner="${AIOPS_WEBTODO_REPO_OWNER:-aiops-bot}"
repo_name="${AIOPS_WEBTODO_REPO_NAME:-web-todo}"
port_base="${AIOPS_WEBTODO_PORT_BASE:-4100}"

usage() {
  printf 'usage: %s --run-root DIR [--gitea-url URL] [--repo-owner NAME] [--repo-name NAME] [--port-base PORT]\n' "$0" >&2
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
  "$run_root/downloads" \
  "$run_root/bin" \
  "$run_root/workflows" \
  "$run_root/issues" \
  "$run_root/logs" \
  "$run_root/state" \
  "$run_root/promo/screenshots" \
  "$run_root/promo/pages" \
  "$run_root/promo/notes" \
  "$run_root/final-verify/screenshots" \
  "$run_root/final-verify/videos" \
  "$run_root/reports" \
  "$run_root/workdirs/maker" \
  "$run_root/workdirs/reviewer" \
  "$run_root/workspaces/maker" \
  "$run_root/workspaces/reviewer" \
  "$run_root/mirrors/maker" \
  "$run_root/mirrors/reviewer"

render_workflow() {
  local src="$1"
  local dest="$2"
  local workspace_root="$3"
  while IFS= read -r line || [ -n "$line" ]; do
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
      *)
        printf '%s\n' "$line" ;;
    esac
  done <"$src" >"$dest"
}

render_workflow "$repo_root/examples/maker-WORKFLOW.md" "$run_root/workflows/maker-WORKFLOW.md" "$run_root/workspaces/maker"
render_workflow "$repo_root/examples/reviewer-automerge-WORKFLOW.md" "$run_root/workflows/reviewer-automerge-WORKFLOW.md" "$run_root/workspaces/reviewer"

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

write_issue 01 scaffold-stdlib-server "01 scaffold stdlib server" \
"Acceptance:
- Add Go module and cmd/server main package.
- Serve GET /healthz returning ok.
- Serve static assets from web/.
- Keep implementation stdlib-only.
- Add tests for health and static serving."

write_issue 02 json-todo-store "02 add JSON todo store" \
"Depends on #1

Acceptance:
- Add internal/store with Todo fields id, title, completed, created_at, and optional due.
- Persist todos to the configured JSON data file.
- Loading a missing file starts with an empty non-nil list.
- IDs are deterministic and incrementing.
- Add unit tests for create/list/update persistence."

write_issue 03 list-create-api "03 list and create todos API" \
"Depends on #2

Acceptance:
- GET /api/todos returns [] for an empty list.
- POST /api/todos accepts JSON title.
- Empty titles and malformed JSON return 400 JSON errors.
- Successful create returns 201 with the created todo.
- Add httptest coverage."

write_issue 04 patch-delete-api "04 patch and delete todo API" \
"Depends on #3

Acceptance:
- PATCH /api/todos/{id} updates title, completed, and due independently.
- completed must reject null and non-boolean JSON values.
- DELETE /api/todos/{id} removes the todo and returns 204.
- Unknown ids return 404 JSON errors.
- Tests prove partial updates preserve omitted fields."

write_issue 05 list-add-ui "05 render list and add form" \
"Depends on #4

Acceptance:
- web/index.html has a form with text input and submit button.
- web/app.js loads GET /api/todos and renders the list.
- Submitting creates a todo through POST /api/todos.
- Empty list renders a friendly empty state.
- Inline errors are shown for failed create/load."

write_issue 06 toggle-delete-ui "06 [EXPECT-REWORK] toggle and delete interactions" \
"Depends on #5

Acceptance:
- Each todo has a checkbox that PATCHes completed.
- Each todo has a delete control.
- UI returns to empty state after deleting the final todo.
- Failed toggle/delete displays an inline error and rolls back optimistic state.
- Add regression coverage for delete/toggle wiring."

write_issue 07 filters-counter "07 client-side filters and active counter" \
"Depends on #6

Acceptance:
- Hash routes #/all, #/active, and #/completed filter the already-loaded list.
- Changing filters does not refetch from the API.
- Active todo count is shown.
- Add behavior-level tests with mocked fetch and hashchange dispatches."

write_issue 08 due-overdue "08 due dates and overdue highlighting" \
"Depends on #7

Acceptance:
- Create and patch accept due as YYYY-MM-DD or null.
- Invalid dates return 400 JSON error.
- UI lets users set and clear due dates.
- Overdue active todos are visually marked.
- Add API and UI coverage."

write_issue 09 inline-edit "09 inline title editing" \
"Depends on #8

Acceptance:
- Double-clicking a title opens inline edit.
- Enter saves through PATCH /api/todos/{id}.
- Escape cancels without API call.
- Blur saves only if changed and valid.
- Blank title errors are shown and old title is preserved."

write_issue 10 docs-makefile-smoke "10 docs, Makefile, and final smoke" \
"Depends on #9

Acceptance:
- Add Makefile with run, test, and build targets.
- README documents flags, API endpoints, JSON storage path, and browser flow.
- Documentation matches actual server behavior.
- Add a small docs/Makefile smoke test if useful."

write_issue 11 control-no-ready "CONTROL no-ready issue must stay idle" \
"Control scenario:
- Do not add an aiops/* label.
- The maker worker must not dispatch this issue.
- No PR should be created."

write_issue 12 control-cancel-running "CONTROL cancel running codex issue" \
"Control scenario:
- Add aiops/todo only when ready to test cancellation.
- Wait for maker state to show this issue running.
- Replace the label with aiops/canceled.
- The maker should stop the active run and no PR should be created."

write_issue 13 control-blocked-held "CONTROL blocked issue held out of ready gate" \
"Control scenario:
- Keep this issue unlabeled or dependent on an unfinished blocker.
- It should not dispatch during the primary lifecycle.
- No PR should be created."

cat >"$run_root/env.example" <<EOF
# Copy to env.local, fill secrets, then source it with set -a.
export AIOPS_WEBTODO_RUN_ROOT="$run_root"
export AIOPS_WEBTODO_TOOLS_ROOT="\$HOME/.cache/aiops-webtodo-e2e-tools"
export AIOPS_WEBTODO_GITEA_URL="$gitea_url"
export AIOPS_WEBTODO_REPO_OWNER="$repo_owner"
export AIOPS_WEBTODO_REPO_NAME="$repo_name"

export GITEA_TOKEN="REPLACE_ME"
export MAKER_CLONE_URL="$maker_clone_url"
export REVIEWER_CLONE_URL="$reviewer_clone_url"

export AIOPS_WEBTODO_WORKER_BIN="$run_root/bin/worker"
export AIOPS_WEBTODO_TUI_BIN="$run_root/bin/tui"

export AIOPS_WEBTODO_MAKER_PORT="$maker_port"
export AIOPS_WEBTODO_REVIEWER_PORT="$reviewer_port"
export AIOPS_WEBTODO_MAKER_DASHBOARD_URL="http://127.0.0.1:$maker_port"
export AIOPS_WEBTODO_REVIEWER_DASHBOARD_URL="http://127.0.0.1:$reviewer_port"

export AIOPS_WEBTODO_MAKER_WORKDIR="$run_root/workdirs/maker"
export AIOPS_WEBTODO_REVIEWER_WORKDIR="$run_root/workdirs/reviewer"
export AIOPS_WEBTODO_MAKER_WORKFLOW="$run_root/workflows/maker-WORKFLOW.md"
export AIOPS_WEBTODO_REVIEWER_WORKFLOW="$run_root/workflows/reviewer-automerge-WORKFLOW.md"
export AIOPS_WEBTODO_MAKER_MIRROR_ROOT="$run_root/mirrors/maker"
export AIOPS_WEBTODO_REVIEWER_MIRROR_ROOT="$run_root/mirrors/reviewer"

# Optional: set this for reproducible production-style Codex runs.
# export CODEX_HOME="$run_root/codex-home"
EOF

cat >"$run_root/NEXT-STEPS.md" <<EOF
# Web Todo lifecycle E2E next steps

1. Install or verify host tools from the runbook, including the Playwright
   venv under \`AIOPS_WEBTODO_TOOLS_ROOT\` when screenshot evidence is needed.
2. Copy \`env.example\` to \`env.local\`, fill secrets, and source it.
3. Put the downloaded release \`worker\` and \`tui\` binaries in \`bin/\`.
4. Create the local Gitea repo, labels, and issues from \`issues/*.md\`.
5. Run \`worker --doctor --deploy=binary --mode=real\` for maker and reviewer.
6. Start maker on port $maker_port and reviewer on port $reviewer_port.
7. Activate the Playwright venv, then use \`scripts/e2e-webtodo-capture.py\`
   at milestones.
8. Run final fresh-clone verification and browser smoke.
9. Run \`scripts/e2e-webtodo-report.py --run-root "$run_root"\`.

See \`docs/runbooks/local-gitea-webtodo-lifecycle-e2e.md\` for the full SOP.
EOF

printf 'Prepared Web Todo lifecycle run root: %s\n' "$run_root"
printf 'Edit and source: %s/env.local\n' "$run_root"
printf 'Issue seed files: %s/issues\n' "$run_root"
