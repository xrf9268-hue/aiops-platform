#!/usr/bin/env bash
set -euo pipefail

mode="mock"
workflow="${AIOPS_SMOKE_WORKFLOW:-${AIOPS_WORKFLOW_PATH:-}}"
dashboard_url="${AIOPS_SMOKE_DASHBOARD_URL:-http://127.0.0.1:4010}"
report_dir="${AIOPS_SMOKE_REPORT_DIR:-docs/validation/smoke}"
issue="${AIOPS_SMOKE_ISSUE:-}"
timeout_seconds="${AIOPS_SMOKE_TIMEOUT_SECONDS:-180}"
worker_bin="${AIOPS_SMOKE_WORKER_BIN:-worker}"
state_api_token="${AIOPS_STATE_API_TOKEN:-}"

usage() {
  printf 'usage: %s [--mode mock|real] --workflow PATH [--issue IDENTIFIER] [--dashboard-url URL] [--report-dir DIR]\n' "$0" >&2
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --mode)
      mode="${2:-}"; shift 2 ;;
    --workflow)
      workflow="${2:-}"; shift 2 ;;
    --issue)
      issue="${2:-}"; shift 2 ;;
    --dashboard-url)
      dashboard_url="${2:-}"; shift 2 ;;
    --report-dir)
      report_dir="${2:-}"; shift 2 ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      usage; exit 2 ;;
  esac
done

if [ "$mode" != "mock" ] && [ "$mode" != "real" ]; then
  printf 'mode must be mock or real\n' >&2
  exit 2
fi
if [ -z "$workflow" ]; then
  printf 'workflow is required\n' >&2
  usage
  exit 2
fi
dashboard_hostport="${dashboard_url#*://}"
dashboard_hostport="${dashboard_hostport%%/*}"
if [ "$dashboard_hostport" = "$dashboard_url" ] || [ "${dashboard_hostport##*:}" = "$dashboard_hostport" ] || [ -z "${dashboard_hostport##*:}" ]; then
  printf 'dashboard-url must include an explicit host:port, got %s\n' "$dashboard_url" >&2
  exit 2
fi
dashboard_port="${dashboard_hostport##*:}"
case "$dashboard_port" in
  *[!0-9]*)
    printf 'dashboard-url port must be numeric, got %s\n' "$dashboard_port" >&2
    exit 2 ;;
esac

api_curl() {
  local args=(-fsS)
  if [ -n "$state_api_token" ]; then
    args+=(-H "Authorization: Bearer $state_api_token")
  fi
  curl "${args[@]}" "$@"
}

state_array_contains_issue() {
  local field="$1"
  local issue_id="$2"
  local file="$3"
  grep -q "\"$field\":[[:space:]]*\[[^]]*\"$issue_id\"" "$file"
}

mkdir -p "$report_dir"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
report="$report_dir/${stamp}-todo-${mode}.md"
workspace_root="${AIOPS_SMOKE_WORKSPACE_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/aiops-smoke-workspaces.XXXXXX")}"
log_file="$(mktemp "${TMPDIR:-/tmp}/aiops-smoke-worker.log.XXXXXX")"
state_file="$(mktemp "${TMPDIR:-/tmp}/aiops-smoke-state.json.XXXXXX")"

cleanup() {
  if [ "${worker_pid:-}" ]; then
    kill "$worker_pid" >/dev/null 2>&1 || true
    wait "$worker_pid" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

{
  printf '# aiops todo smoke report\n\n'
  printf -- '- timestamp: `%s`\n' "$stamp"
  printf -- '- mode: `%s`\n' "$mode"
  printf -- '- workflow: `%s`\n' "$workflow"
  printf -- '- dashboard_url: `%s`\n' "$dashboard_url"
  printf -- '- issue: `%s`\n' "${issue:-not specified}"
  printf -- '- workspace_root: `%s`\n\n' "$workspace_root"
} >"$report"

"$worker_bin" --doctor --mode="$mode" "$workflow" >>"$report"

AIOPS_WORKFLOW_PATH="$workflow" \
AIOPS_WORKSPACE_ROOT="$workspace_root" \
AIOPS_SERVER_HOST=127.0.0.1 \
"$worker_bin" --port="$dashboard_port" "$workflow" >"$log_file" 2>&1 &
worker_pid="$!"

deadline=$(( $(date +%s) + timeout_seconds ))
ready="false"
while [ "$(date +%s)" -lt "$deadline" ]; do
  if ! kill -0 "$worker_pid" >/dev/null 2>&1; then
    printf '\n## result\n\nFAIL worker exited before smoke completed\n\n' >>"$report"
    printf 'Worker log: `%s`\n' "$log_file" >>"$report"
    exit 1
  fi
  if curl -fsS "$dashboard_url/readyz" >/dev/null 2>&1; then
    ready="true"
    break
  fi
  sleep 2
done

if [ "$ready" != "true" ]; then
  printf '\n## result\n\nFAIL timed out waiting for worker readiness.\n\n' >>"$report"
  printf 'State snapshot: `%s`\nWorker log: `%s`\n' "$state_file" "$log_file" >>"$report"
  exit 1
fi

api_curl -X POST -H 'X-AIOPS-Refresh: true' "$dashboard_url/api/v1/refresh" >/dev/null

completed_before="0"
failed_before="0"
selected_issue_id=""
selected_observed="false"
if api_curl "$dashboard_url/api/v1/state" >"$state_file"; then
  completed_before="$(sed -n 's/.*"completed_total":[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$state_file" | head -1)"
  failed_before="$(sed -n 's/.*"failed_total":[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$state_file" | head -1)"
fi
completed_before="${completed_before:-0}"
failed_before="${failed_before:-0}"

while [ "$(date +%s)" -lt "$deadline" ]; do
  api_curl "$dashboard_url/api/v1/state" >"$state_file"
  if [ -n "$issue" ] && [ "$selected_observed" = "false" ]; then
    issue_file="$(mktemp "${TMPDIR:-/tmp}/aiops-smoke-issue.json.XXXXXX")"
    if api_curl "$dashboard_url/api/v1/$issue" >"$issue_file"; then
      selected_observed="true"
      selected_issue_id="$(sed -n 's/.*"issue_id":[[:space:]]*"\([^"]*\)".*/\1/p' "$issue_file" | head -1)"
      printf '\n## selected issue\n\nObserved `%s` in runtime state as `%s`.\n\n' "$issue" "${selected_issue_id:-unknown}" >>"$report"
    fi
  fi
  completed_now="$(sed -n 's/.*"completed_total":[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$state_file" | head -1)"
  failed_now="$(sed -n 's/.*"failed_total":[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$state_file" | head -1)"
  completed_now="${completed_now:-0}"
  failed_now="${failed_now:-0}"
  if [ -n "$selected_issue_id" ] && state_array_contains_issue completed "$selected_issue_id" "$state_file"; then
    printf '\n## result\n\nPASS selected issue `%s` completed.\n\n' "$issue" >>"$report"
    printf 'State snapshot: `%s`\nWorker log: `%s`\n' "$state_file" "$log_file" >>"$report"
    printf '%s\n' "$report"
    exit 0
  fi
  if [ -n "$selected_issue_id" ] && state_array_contains_issue failed "$selected_issue_id" "$state_file"; then
    printf '\n## result\n\nFAIL selected issue `%s` failed.\n\n' "$issue" >>"$report"
    printf 'State snapshot: `%s`\nWorker log: `%s`\n' "$state_file" "$log_file" >>"$report"
    exit 1
  fi
  if [ -z "$issue" ] && [ "$completed_now" -gt "$completed_before" ]; then
    printf '\n## result\n\nPASS completed_total advanced from `%s` to `%s`.\n\n' "$completed_before" "$completed_now" >>"$report"
    printf 'State snapshot: `%s`\nWorker log: `%s`\n' "$state_file" "$log_file" >>"$report"
    printf '%s\n' "$report"
    exit 0
  fi
  if [ -z "$issue" ] && [ "$failed_now" -gt "$failed_before" ]; then
    printf '\n## result\n\nFAIL failed_total advanced from `%s` to `%s`.\n\n' "$failed_before" "$failed_now" >>"$report"
    printf 'State snapshot: `%s`\nWorker log: `%s`\n' "$state_file" "$log_file" >>"$report"
    exit 1
  fi
  if [ -n "$issue" ]; then
    if [ "$completed_now" -gt "$completed_before" ] && [ "$selected_observed" != "true" ]; then
      printf '\n## result\n\nFAIL completed_total advanced, but selected issue `%s` was never observed in runtime state.\n\n' "$issue" >>"$report"
      printf 'State snapshot: `%s`\nWorker log: `%s`\n' "$state_file" "$log_file" >>"$report"
      exit 1
    fi
  fi
  sleep 3
done

printf '\n## result\n\nFAIL timed out waiting for one worker lifecycle.\n\n' >>"$report"
printf 'State snapshot: `%s`\nWorker log: `%s`\n' "$state_file" "$log_file" >>"$report"
exit 1
