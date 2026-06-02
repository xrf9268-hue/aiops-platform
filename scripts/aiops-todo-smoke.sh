#!/usr/bin/env bash
set -euo pipefail

mode="mock"
workflow="${AIOPS_SMOKE_WORKFLOW:-${AIOPS_WORKFLOW_PATH:-}}"
dashboard_url="${AIOPS_SMOKE_DASHBOARD_URL:-http://127.0.0.1:4010}"
report_dir="${AIOPS_SMOKE_REPORT_DIR:-docs/validation/smoke}"
issue="${AIOPS_SMOKE_ISSUE:-}"
github_issue="${AIOPS_SMOKE_GITHUB_ISSUE:-}"
github_repo="${AIOPS_SMOKE_GITHUB_REPO:-}"
github_issue_url=""
expect_draft_pr="${AIOPS_SMOKE_EXPECT_DRAFT_PR:-false}"
draft_pr_baseline=""
draft_pr_poll_attempts="${AIOPS_SMOKE_PR_POLL_ATTEMPTS:-30}"
draft_pr_poll_interval="${AIOPS_SMOKE_PR_POLL_INTERVAL_SECONDS:-1}"
timeout_seconds="${AIOPS_SMOKE_TIMEOUT_SECONDS:-180}"
worker_bin="${AIOPS_SMOKE_WORKER_BIN:-worker}"
state_api_token="${AIOPS_STATE_API_TOKEN:-}"

usage() {
  printf 'usage: %s [--mode mock|real] --workflow PATH [--issue IDENTIFIER] [--dashboard-url URL] [--report-dir DIR] [--github-repo OWNER/REPO --github-issue NUMBER --expect-draft-pr]\n' "$0" >&2
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --mode)
      mode="${2:-}"; shift 2 ;;
    --workflow)
      workflow="${2:-}"; shift 2 ;;
    --issue)
      issue="${2:-}"; shift 2 ;;
    --github-issue)
      github_issue="${2:-}"; shift 2 ;;
    --github-repo)
      github_repo="${2:-}"; shift 2 ;;
    --expect-draft-pr)
      expect_draft_pr="true"; shift ;;
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
case "$github_issue" in
  ""|*[!0-9]*)
    if [ -n "$github_issue" ]; then
      printf 'github-issue must be numeric, got %s\n' "$github_issue" >&2
      exit 2
    fi ;;
esac
if [ "$expect_draft_pr" = "true" ] && { [ -z "$github_issue" ] || [ -z "$github_repo" ]; }; then
  printf 'expect-draft-pr requires --github-issue and --github-repo\n' >&2
  exit 2
fi
if [ -n "$github_repo" ] && ! printf '%s\n' "$github_repo" | grep -Eq '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$'; then
  printf 'github-repo must be OWNER/REPO, got %s\n' "$github_repo" >&2
  exit 2
fi
if [ -n "$github_repo" ] && [ -n "$github_issue" ]; then
  github_issue_url="https://github.com/$github_repo/issues/$github_issue"
fi
case "$draft_pr_poll_attempts" in
  *[!0-9]*|"")
    printf 'AIOPS_SMOKE_PR_POLL_ATTEMPTS must be numeric, got %s\n' "$draft_pr_poll_attempts" >&2
    exit 2 ;;
esac
case "$draft_pr_poll_interval" in
  *[!0-9]*|"")
    printf 'AIOPS_SMOKE_PR_POLL_INTERVAL_SECONDS must be numeric, got %s\n' "$draft_pr_poll_interval" >&2
    exit 2 ;;
esac
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

matching_draft_prs() {
  local draft_lines
  local line
  local number
  local pr_line
  local viewed_pr
  local view_status
  draft_lines="$(open_draft_prs)" || return 1
  while IFS='|' read -r number pr_line; do
    if [ -z "$number" ] || baseline_has_pr "$number"; then
      continue
    fi
    view_status=0
    viewed_pr="$(gh pr view "$number" \
      --repo "$github_repo" \
      --json number,isDraft,url,closingIssuesReferences \
      --jq ". | select(.isDraft == true and (.closingIssuesReferences | any(.number == $github_issue and .url == \"$github_issue_url\"))) | \"\\(.number)|#\\(.number) \\(.url)\"")" || view_status=$?
    if [ "$view_status" -ne 0 ]; then
      return 1
    fi
    if [ -n "$viewed_pr" ]; then
      printf '%s\n' "$viewed_pr"
    fi
  done <<EOF
$draft_lines
EOF
}

open_draft_prs() {
  gh api --paginate "repos/$github_repo/pulls?state=open&per_page=100" \
    --jq '.[] | select(.draft == true) | "\(.number)|#\(.number) \(.html_url)"'
}

capture_draft_pr_baseline() {
  if [ "$expect_draft_pr" != "true" ]; then
    return 0
  fi
  if ! command -v gh >/dev/null 2>&1; then
    printf '\n## result\n\nFAIL `gh` is required to verify the draft PR for GitHub issue `%s`.\n\n' "$github_issue" >>"$report"
    return 1
  fi
  err_file="$(mktemp "${TMPDIR:-/tmp}/aiops-smoke-gh-pr-list.err.XXXXXX")"
  if ! draft_pr_baseline="$(open_draft_prs 2>"$err_file")"; then
    printf '\n## result\n\nFAIL `gh api` could not inspect draft PRs for `%s#%s`.\n\n' "$github_repo" "$github_issue" >>"$report"
    printf 'Error output: `%s`\n' "$err_file" >>"$report"
    return 1
  fi
  rm -f "$err_file"
}

baseline_has_pr() {
  local number="$1"
  local line
  while IFS= read -r line; do
    if [ "${line%%|*}" = "$number" ]; then
      return 0
    fi
  done <<EOF
$draft_pr_baseline
EOF
  return 1
}

verify_expected_draft_pr() {
  if [ "$expect_draft_pr" != "true" ]; then
    return 0
  fi
  attempt=1
  while [ "$attempt" -le "$draft_pr_poll_attempts" ]; do
    err_file="$(mktemp "${TMPDIR:-/tmp}/aiops-smoke-gh-pr-list.err.XXXXXX")"
    if ! pr_lines="$(matching_draft_prs 2>"$err_file")"; then
      printf '\n## result\n\nFAIL `gh` could not inspect draft PRs for `%s#%s`.\n\n' "$github_repo" "$github_issue" >>"$report"
      printf 'Error output: `%s`\n' "$err_file" >>"$report"
      return 1
    fi
    rm -f "$err_file"
    while IFS= read -r line; do
      number="${line%%|*}"
      pr_line="${line#*|}"
      if [ -n "$number" ]; then
        printf '\n## GitHub draft PR\n\nVerified new `%s` closes `%s#%s`.\n\n' "$pr_line" "$github_repo" "$github_issue" >>"$report"
        return 0
      fi
    done <<EOF
$pr_lines
EOF
    if [ "$attempt" -lt "$draft_pr_poll_attempts" ]; then
      sleep "$draft_pr_poll_interval"
    fi
    attempt=$((attempt + 1))
  done
  printf '\n## result\n\nFAIL no new open draft PR in `%s` closes GitHub issue `%s`.\n\n' "$github_repo" "$github_issue" >>"$report"
  return 1
}

mkdir -p "$report_dir"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
smoke_started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
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
  printf -- '- smoke_started_at: `%s`\n' "$smoke_started_at"
  printf -- '- mode: `%s`\n' "$mode"
  printf -- '- workflow: `%s`\n' "$workflow"
  printf -- '- dashboard_url: `%s`\n' "$dashboard_url"
  printf -- '- issue: `%s`\n' "${issue:-not specified}"
  printf -- '- github_repo: `%s`\n' "${github_repo:-not specified}"
  printf -- '- github_issue: `%s`\n' "${github_issue:-not specified}"
  printf -- '- expect_draft_pr: `%s`\n' "$expect_draft_pr"
  printf -- '- workspace_root: `%s`\n\n' "$workspace_root"
} >"$report"

doctor_args=(--doctor --mode="$mode")
if [ -n "$github_issue" ]; then
  doctor_args+=(--github-issue "$github_issue")
fi
if [ -n "$github_repo" ]; then
  doctor_args+=(--github-repo "$github_repo")
fi
doctor_args+=("$workflow")
"$worker_bin" "${doctor_args[@]}" >>"$report"
if ! capture_draft_pr_baseline; then
  exit 1
fi

AIOPS_WORKFLOW_PATH="$workflow" \
AIOPS_WORKSPACE_ROOT="$workspace_root" \
AIOPS_SERVER_HOST=127.0.0.1 \
"$worker_bin" --port="$dashboard_port" "$workflow" >"$log_file" 2>&1 &
worker_pid="$!"

deadline=$(( $(date +%s) + timeout_seconds ))
ready="false"
while [ "$(date +%s)" -lt "$deadline" ]; do
  if ! kill -0 "$worker_pid" >/dev/null 2>&1; then
    verify_expected_draft_pr || true
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
  verify_expected_draft_pr || true
  printf '\n## result\n\nFAIL timed out waiting for worker readiness.\n\n' >>"$report"
  printf 'State snapshot: `%s`\nWorker log: `%s`\n' "$state_file" "$log_file" >>"$report"
  exit 1
fi

if ! api_curl -X POST -H 'X-AIOPS-Refresh: true' "$dashboard_url/api/v1/refresh" >/dev/null; then
  verify_expected_draft_pr || true
  printf '\n## result\n\nFAIL refresh request failed.\n\n' >>"$report"
  printf 'State snapshot: `%s`\nWorker log: `%s`\n' "$state_file" "$log_file" >>"$report"
  exit 1
fi

completed_before="0"
selected_issue_id=""
selected_observed="false"
if api_curl "$dashboard_url/api/v1/state" >"$state_file"; then
  completed_before="$(sed -n 's/.*"completed_total":[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$state_file" | head -1)"
fi
completed_before="${completed_before:-0}"

while [ "$(date +%s)" -lt "$deadline" ]; do
  if ! api_curl "$dashboard_url/api/v1/state" >"$state_file"; then
    verify_expected_draft_pr || true
    printf '\n## result\n\nFAIL state request failed.\n\n' >>"$report"
    printf 'State snapshot: `%s`\nWorker log: `%s`\n' "$state_file" "$log_file" >>"$report"
    exit 1
  fi
  # Poll the per-issue drill-down every iteration. A failed run no longer lands
  # in a terminal `failed` set (removed in #584, D29); it is parked in `retrying`
  # on the SPEC §8.4 backoff, so the drill-down reports status=="retrying" with a
  # non-null `last_error` (and retry.kind=="failure"). `last_error` is the
  # failure signal — it is null for a clean §16.6 continuation, so a healthy run
  # never trips it.
  selected_error=""
  if [ -n "$issue" ]; then
    issue_file="$(mktemp "${TMPDIR:-/tmp}/aiops-smoke-issue.json.XXXXXX")"
    if api_curl "$dashboard_url/api/v1/$issue" >"$issue_file"; then
      # Match only a non-null string value; "last_error":null (clean run) yields
      # no match and leaves selected_error empty.
      selected_error="$(sed -n 's/.*"last_error":[[:space:]]*"\([^"]*\)".*/\1/p' "$issue_file" | head -1)"
      if [ "$selected_observed" = "false" ]; then
        selected_observed="true"
        selected_issue_id="$(sed -n 's/.*"issue_id":[[:space:]]*"\([^"]*\)".*/\1/p' "$issue_file" | head -1)"
        printf '\n## selected issue\n\nObserved `%s` in runtime state as `%s`.\n\n' "$issue" "${selected_issue_id:-unknown}" >>"$report"
      fi
    fi
  fi
  completed_now="$(sed -n 's/.*"completed_total":[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$state_file" | head -1)"
  completed_now="${completed_now:-0}"
  if [ -n "$selected_issue_id" ] && state_array_contains_issue completed "$selected_issue_id" "$state_file"; then
    if ! verify_expected_draft_pr; then
      printf 'State snapshot: `%s`\nWorker log: `%s`\n' "$state_file" "$log_file" >>"$report"
      exit 1
    fi
    printf '\n## result\n\nPASS selected issue `%s` completed.\n\n' "$issue" >>"$report"
    printf 'State snapshot: `%s`\nWorker log: `%s`\n' "$state_file" "$log_file" >>"$report"
    printf '%s\n' "$report"
    exit 0
  fi
  if [ -n "$selected_issue_id" ] && [ -n "$selected_error" ]; then
    verify_expected_draft_pr || true
    printf '\n## result\n\nFAIL selected issue `%s` failed: %s\n\n' "$issue" "$selected_error" >>"$report"
    printf 'State snapshot: `%s`\nWorker log: `%s`\n' "$state_file" "$log_file" >>"$report"
    exit 1
  fi
  if [ -z "$issue" ] && [ "$completed_now" -gt "$completed_before" ]; then
    if ! verify_expected_draft_pr; then
      printf 'State snapshot: `%s`\nWorker log: `%s`\n' "$state_file" "$log_file" >>"$report"
      exit 1
    fi
    printf '\n## result\n\nPASS completed_total advanced from `%s` to `%s`.\n\n' "$completed_before" "$completed_now" >>"$report"
    printf 'State snapshot: `%s`\nWorker log: `%s`\n' "$state_file" "$log_file" >>"$report"
    printf '%s\n' "$report"
    exit 0
  fi
  # No aggregate failure counter exists anymore (#584): a persistently failing
  # no-specific-issue run keeps retrying on the §8.4 backoff and is caught by the
  # readiness/lifecycle timeout below rather than a `failed_total` advance.
  if [ -n "$issue" ]; then
    if [ "$completed_now" -gt "$completed_before" ] && [ "$selected_observed" != "true" ]; then
      verify_expected_draft_pr || true
      printf '\n## result\n\nFAIL completed_total advanced, but selected issue `%s` was never observed in runtime state.\n\n' "$issue" >>"$report"
      printf 'State snapshot: `%s`\nWorker log: `%s`\n' "$state_file" "$log_file" >>"$report"
      exit 1
    fi
  fi
  sleep 3
done

verify_expected_draft_pr || true
printf '\n## result\n\nFAIL timed out waiting for one worker lifecycle.\n\n' >>"$report"
printf 'State snapshot: `%s`\nWorker log: `%s`\n' "$state_file" "$log_file" >>"$report"
exit 1
