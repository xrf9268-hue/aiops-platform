#!/usr/bin/env bash
set -euo pipefail

export PATH="$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/Applications/Codex.app/Contents/Resources:${PATH:-}"
export GH_PROMPT_DISABLED=1

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pr_label="${AIOPS_PR_LABEL:-ai-generated}"
pr_scan_limit="${AIOPS_PR_SCAN_LIMIT:-1000}"
auto_merge="${AIOPS_AUTO_MERGE:-1}"
repo_path="${AIOPS_GITHUB_REPO:-xrf9268-hue/aiops-platform}"
repo_path="${repo_path#github.com/}"
repo_owner="${repo_path%/*}"
repo_name="${repo_path##*/}"
pr_worktree="${AIOPS_PR_WORKTREE:-"$HOME/aiops-workspaces/github/xrf9268-hue-aiops-platform-pr-follow-through"}"
gh_timeout="${AIOPS_GH_TIMEOUT:-60s}"
gate_mode="${AIOPS_GATE_MODE:-auto}"
review_timeout="${AIOPS_REVIEW_TIMEOUT:-20m}"
checks_timeout="${AIOPS_CHECKS_TIMEOUT:-30m}"
github_codex_review_timeout="${AIOPS_GITHUB_CODEX_REVIEW_TIMEOUT:-20m}"
github_codex_review_poll_seconds="${AIOPS_GITHUB_CODEX_REVIEW_POLL_SECONDS:-30}"
timeout_bin="${AIOPS_TIMEOUT_BIN:-}"
follow_lock_dir="${AIOPS_FOLLOW_THROUGH_LOCK_DIR:-"$HOME/Library/Caches/aiops-platform/pr-follow-through.lock"}"
follow_lock_stale_seconds="${AIOPS_FOLLOW_THROUGH_LOCK_STALE_SECONDS:-3600}"
review_state_dir="${AIOPS_REVIEW_STATE_DIR:-"$HOME/Library/Caches/aiops-platform/reviews"}"
github_codex_review_state_dir="${AIOPS_GITHUB_CODEX_REVIEW_STATE_DIR:-"$HOME/Library/Caches/aiops-platform/github-codex-review"}"

cd "$repo_root"

audit_log() {
  local event="$1"
  shift || true
  printf '%s component=github-pr-follow-through event=%s %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$event" "$*"
}

release_follow_through_lock() {
  if [[ -n "${follow_lock_acquired:-}" ]]; then
    rm -rf "$follow_lock_dir"
  fi
}

lock_mtime_epoch() {
  stat -f %m "$follow_lock_dir" 2>/dev/null || stat -c %Y "$follow_lock_dir" 2>/dev/null || printf '0\n'
}

lock_age_seconds() {
  local now mtime
  now="$(date +%s)"
  mtime="$(lock_mtime_epoch)"
  if [[ "$mtime" =~ ^[0-9]+$ ]]; then
    printf '%s\n' "$((now - mtime))"
  else
    printf '0\n'
  fi
}

acquire_follow_through_lock() {
  if ! [[ "$follow_lock_stale_seconds" =~ ^[0-9]+$ ]]; then
    echo "AIOPS_FOLLOW_THROUGH_LOCK_STALE_SECONDS must be an integer number of seconds" >&2
    exit 2
  fi
  mkdir -p "$(dirname "$follow_lock_dir")"
  if mkdir "$follow_lock_dir" 2>/dev/null; then
    follow_lock_acquired=1
    if ! printf '%s\n' "$$" > "$follow_lock_dir/pid"; then
      rm -rf "$follow_lock_dir"
      echo "failed to write follow-through lock pid at $follow_lock_dir/pid" >&2
      exit 2
    fi
    audit_log "lock_acquired" "pid=$$ lock_dir=$follow_lock_dir"
    trap 'release_follow_through_lock' EXIT
    return 0
  fi

  local existing_pid
  existing_pid="$(cat "$follow_lock_dir/pid" 2>/dev/null || true)"
  if [[ -z "$existing_pid" ]]; then
    local lock_age
    lock_age="$(lock_age_seconds)"
    if ((lock_age < follow_lock_stale_seconds)); then
      audit_log "lock_initializing" "age_seconds=$lock_age stale_after_seconds=$follow_lock_stale_seconds lock_dir=$follow_lock_dir"
      echo "follow-through lock is initializing; exiting"
      exit 0
    fi
  fi
  if [[ -n "$existing_pid" ]] && kill -0 "$existing_pid" 2>/dev/null; then
    audit_log "lock_busy" "pid=$existing_pid lock_dir=$follow_lock_dir"
    echo "follow-through already running (pid $existing_pid); exiting"
    exit 0
  fi

  audit_log "lock_stale" "pid=${existing_pid:-unknown} lock_dir=$follow_lock_dir"
  rm -rf "$follow_lock_dir"
  if mkdir "$follow_lock_dir" 2>/dev/null; then
    follow_lock_acquired=1
    if ! printf '%s\n' "$$" > "$follow_lock_dir/pid"; then
      rm -rf "$follow_lock_dir"
      echo "failed to write follow-through lock pid at $follow_lock_dir/pid" >&2
      exit 2
    fi
    audit_log "lock_acquired" "pid=$$ lock_dir=$follow_lock_dir"
    trap 'release_follow_through_lock' EXIT
    return 0
  fi

  audit_log "lock_race_lost" "lock_dir=$follow_lock_dir"
  echo "follow-through already running; exiting"
  exit 0
}

state_repo_key() {
  printf '%s' "$repo_path" | tr '/:@' '____'
}

safe_key_component() {
  printf '%s' "$1" | tr -c '[:alnum:]._-' '_'
}

state_file_for() {
  local root="$1"
  local pr="$2"
  local head_oid="$3"
  local dir="$root/$(state_repo_key)"
  mkdir -p "$dir"
  printf '%s/pr-%s-%s.json\n' "$dir" "$pr" "$head_oid"
}

review_state_file() {
  state_file_for "$review_state_dir" "$1" "$2-$3-$(safe_key_component "$4")"
}

review_artifact_dir() {
  local state_file
  state_file="$(review_state_file "$1" "$2" "$3" "$4")"
  printf '%s.artifacts\n' "${state_file%.json}"
}

github_codex_review_state_file() {
  state_file_for "$github_codex_review_state_dir" "$1" "$2-$3-$(safe_key_component "$4")"
}

closing_issue_report_for_prs() {
  local pr payload tmp status
  tmp="$(mktemp "${TMPDIR:-/tmp}/aiops-pr-closing-issues.XXXXXX")"
  for pr in "$@"; do
    payload="$(gh_cmd pr view -R "$repo_path" "$pr" --json number,title,body,url)"
    printf '%s\n' "$payload" >> "$tmp"
  done
  set +e
  python3 - "$tmp" <<'PY'
import json
import re
import sys

path = sys.argv[1]
pattern = re.compile(r"\b(?:(?:close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved)|(?:(?:assigned|github)\s+)?issue)\s*:?\s+(?:[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)?#([0-9]+)\b", re.I)
by_issue = {}
with open(path, "r", encoding="utf-8") as handle:
    for line in handle:
        if not line.strip():
            continue
        pr = json.loads(line)
        text = f"{pr.get('title') or ''}\n{pr.get('body') or ''}"
        for issue in sorted(set(pattern.findall(text)), key=int):
            by_issue.setdefault(issue, []).append(str(pr["number"]))
duplicates = {issue: prs for issue, prs in by_issue.items() if len(prs) > 1}
if duplicates:
    print(" ".join(f"issue={issue} prs={','.join(prs)}" for issue, prs in sorted(duplicates.items(), key=lambda item: int(item[0]))))
    sys.exit(1)
PY
  status=$?
  set -e
  rm -f "$tmp"
  return "$status"
}

assert_no_duplicate_closing_issue_prs() {
  local report
  if ! report="$(closing_issue_report_for_prs "$@")"; then
    audit_log "duplicate_prs_detected" "$report"
    echo "duplicate open PRs closing the same issue: $report" >&2
    return 1
  fi
}

assert_prs_claim_issues() {
  local pr payload tmp report status
  tmp="$(mktemp "${TMPDIR:-/tmp}/aiops-pr-claimed-issues.XXXXXX")"
  for pr in "$@"; do
    payload="$(gh_cmd pr view -R "$repo_path" "$pr" --json number,title,body,url)"
    printf '%s\n' "$payload" >> "$tmp"
  done
  set +e
  report="$(python3 - "$tmp" <<'PY'
import json
import re
import sys

path = sys.argv[1]
pattern = re.compile(r"\b(?:(?:close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved)|(?:(?:assigned|github)\s+)?issue)\s*:?\s+(?:[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)?#([0-9]+)\b", re.I)
missing = []
with open(path, "r", encoding="utf-8") as handle:
    for line in handle:
        if not line.strip():
            continue
        pr = json.loads(line)
        text = f"{pr.get('title') or ''}\n{pr.get('body') or ''}"
        if not pattern.findall(text):
            missing.append(str(pr["number"]))
if missing:
    print("prs=" + ",".join(missing))
    sys.exit(1)
PY
)"
  status=$?
  set -e
  rm -f "$tmp"
  if [[ "$status" -ne 0 ]]; then
    audit_log "missing_pr_issue_claim" "$report"
    echo "open PRs missing explicit issue claim: $report" >&2
    return "$status"
  fi
}

list_open_pr_numbers() {
  local label="${1:-}"
  if ! [[ "$pr_scan_limit" =~ ^[0-9]+$ ]] || ((pr_scan_limit < 1)); then
    echo "AIOPS_PR_SCAN_LIMIT must be a positive integer" >&2
    return 2
  fi
  if [[ -n "$label" ]]; then
    gh_cmd pr list -R "$repo_path" --state open --label "$label" --limit "$pr_scan_limit" --json number --jq '.[].number'
  else
    gh_cmd pr list -R "$repo_path" --state open --limit "$pr_scan_limit" --json number --jq '.[].number'
  fi
}

assert_pr_scan_not_truncated() {
  local scope="$1"
  shift
  if (( $# >= pr_scan_limit )); then
    audit_log "pr_scan_limit_reached" "scope=$scope limit=$pr_scan_limit"
    echo "open PR scan reached AIOPS_PR_SCAN_LIMIT=$pr_scan_limit for $scope; refusing to continue on a possibly truncated PR set" >&2
    return 1
  fi
}

current_pr_head_oid() {
  gh_cmd pr view -R "$repo_path" "$1" --json headRefOid --jq '.headRefOid'
}

current_pr_ref_json() {
  gh_cmd pr view -R "$repo_path" "$1" --json headRefOid,baseRefOid,baseRefName
}

assert_local_head_matches_pr() {
  local pr="$1"
  local expected_head="$2"
  local local_head
  local_head="$(git rev-parse HEAD)"
  if [[ "$local_head" != "$expected_head" ]]; then
    audit_log "checkout_head_mismatch" "pr=$pr expected_head=$expected_head local_head=$local_head"
    echo "PR #$pr local checkout head mismatch: $local_head != $expected_head" >&2
    return 1
  fi
}

assert_pr_refs_unchanged() {
  local pr="$1"
  local expected_head="$2"
  local expected_base_oid="$3"
  local expected_base_ref="$4"
  local stage="$5"
  local refs current_head current_base_oid current_base_ref
  refs="$(current_pr_ref_json "$pr")"
  current_head="$(jq -r '.headRefOid' <<<"$refs")"
  current_base_oid="$(jq -r '.baseRefOid' <<<"$refs")"
  current_base_ref="$(jq -r '.baseRefName' <<<"$refs")"
  if [[ "$current_head" != "$expected_head" || "$current_base_oid" != "$expected_base_oid" || "$current_base_ref" != "$expected_base_ref" ]]; then
    audit_log "pr_refs_changed" "pr=$pr stage=$stage expected_head=$expected_head current_head=$current_head expected_base=$expected_base_oid current_base=$current_base_oid expected_base_ref=$expected_base_ref current_base_ref=$current_base_ref"
    echo "PR #$pr refs changed at $stage: head $current_head != $expected_head or base $current_base_ref@$current_base_oid != $expected_base_ref@$expected_base_oid" >&2
    return 1
  fi
}

assert_pr_head_unchanged() {
  local pr="$1"
  local expected_head="$2"
  local stage="$3"
  local current_head
  current_head="$(current_pr_head_oid "$pr")"
  if [[ "$current_head" != "$expected_head" ]]; then
    audit_log "head_changed" "pr=$pr stage=$stage expected_head=$expected_head current_head=$current_head"
    echo "PR #$pr head changed at $stage: $current_head != $expected_head" >&2
    return 1
  fi
}

local_reviews_already_passed() {
  local pr="$1"
  local head_oid="$2"
  local base_oid="$3"
  local base_ref="$4"
  local state_file
  state_file="$(review_state_file "$pr" "$head_oid" "$base_oid" "$base_ref")"
  [[ -s "$state_file" ]] && jq -e --arg head_oid "$head_oid" --arg base_oid "$base_oid" --arg base_ref "$base_ref" '.status == "passed" and .head_oid == $head_oid and .base_oid == $base_oid and .base_ref == $base_ref and .claude == "passed" and .codex == "passed" and (.artifacts_dir // "") != ""' "$state_file" >/dev/null 2>&1
}

local_reviews_already_failed() {
  local pr="$1"
  local head_oid="$2"
  local base_oid="$3"
  local base_ref="$4"
  local state_file
  state_file="$(review_state_file "$pr" "$head_oid" "$base_oid" "$base_ref")"
  [[ -s "$state_file" ]] && jq -e --arg head_oid "$head_oid" --arg base_oid "$base_oid" --arg base_ref "$base_ref" '.status == "failed" and .head_oid == $head_oid and .base_oid == $base_oid and .base_ref == $base_ref' "$state_file" >/dev/null 2>&1
}

mark_local_reviews_passed() {
  local pr="$1"
  local head_oid="$2"
  local base_oid="$3"
  local base_ref="$4"
  local artifacts_dir="$5"
  local state_file tmp
  state_file="$(review_state_file "$pr" "$head_oid" "$base_oid" "$base_ref")"
  tmp="${state_file}.$$"
  jq -n \
    --arg pr "$pr" \
    --arg head_oid "$head_oid" \
    --arg base_oid "$base_oid" \
    --arg base_ref "$base_ref" \
    --arg artifacts_dir "$artifacts_dir" \
    --arg reviewed_at "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
    '{status:"passed", pr:$pr, head_oid:$head_oid, base_oid:$base_oid, base_ref:$base_ref, claude:"passed", codex:"passed", artifacts_dir:$artifacts_dir, reviewed_at:$reviewed_at}' > "$tmp"
  mv "$tmp" "$state_file"
  audit_log "local_reviews_cached" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref state_file=$state_file artifacts_dir=$artifacts_dir"
}

mark_local_reviews_failed() {
  local pr="$1"
  local head_oid="$2"
  local base_oid="$3"
  local base_ref="$4"
  local claude_status="$5"
  local codex_status="$6"
  local artifacts_dir="$7"
  local state_file tmp claude_result codex_result
  claude_result="failed"
  codex_result="failed"
  if [[ "$claude_status" -eq 0 ]]; then
    claude_result="passed"
  fi
  if [[ "$codex_status" -eq 0 ]]; then
    codex_result="passed"
  fi
  state_file="$(review_state_file "$pr" "$head_oid" "$base_oid" "$base_ref")"
  tmp="${state_file}.$$"
  jq -n \
    --arg pr "$pr" \
    --arg head_oid "$head_oid" \
    --arg base_oid "$base_oid" \
    --arg base_ref "$base_ref" \
    --arg artifacts_dir "$artifacts_dir" \
    --arg reviewed_at "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
    --arg claude_result "$claude_result" \
    --arg codex_result "$codex_result" \
    --argjson claude_status "$claude_status" \
    --argjson codex_status "$codex_status" \
    '{status:"failed", pr:$pr, head_oid:$head_oid, base_oid:$base_oid, base_ref:$base_ref, claude:$claude_result, codex:$codex_result, claude_exit_code:$claude_status, codex_exit_code:$codex_status, artifacts_dir:$artifacts_dir, reviewed_at:$reviewed_at}' > "$tmp"
  mv "$tmp" "$state_file"
  audit_log "local_reviews_failed_cached" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref claude_status=$claude_status codex_status=$codex_status state_file=$state_file artifacts_dir=$artifacts_dir"
}

preserve_local_review_artifacts() {
  local pr="$1"
  local head_oid="$2"
  local base_oid="$3"
  local base_ref="$4"
  local claude_file="$5"
  local codex_file="$6"
  local artifacts_dir
  artifacts_dir="$(review_artifact_dir "$pr" "$head_oid" "$base_oid" "$base_ref")"
  rm -rf "$artifacts_dir"
  mkdir -p "$artifacts_dir"
  if [[ -s "$claude_file" ]]; then
    cp "$claude_file" "$artifacts_dir/claude-review.json"
  fi
  if [[ -s "${claude_file}.raw.json" ]]; then
    cp "${claude_file}.raw.json" "$artifacts_dir/claude-review.raw.json"
  fi
  if [[ -s "${claude_file}.prompt" ]]; then
    cp "${claude_file}.prompt" "$artifacts_dir/claude-review.prompt"
  fi
  if [[ -s "$codex_file" ]]; then
    cp "$codex_file" "$artifacts_dir/codex-review.json"
  fi
  if [[ -s "${codex_file}.stdout" ]]; then
    cp "${codex_file}.stdout" "$artifacts_dir/codex-review.stdout"
  fi
  if [[ -s "${codex_file}.prompt" ]]; then
    cp "${codex_file}.prompt" "$artifacts_dir/codex-review.prompt"
  fi
  printf '%s\n' "$artifacts_dir"
}

resolve_timeout_bin() {
  if [[ -n "$timeout_bin" ]]; then
    if command -v "$timeout_bin" >/dev/null 2>&1; then
      command -v "$timeout_bin"
      return 0
    fi
    echo "AIOPS_TIMEOUT_BIN=$timeout_bin was not found on PATH" >&2
    return 2
  fi
  if command -v timeout >/dev/null 2>&1; then
    command -v timeout
    return 0
  fi
  if command -v gtimeout >/dev/null 2>&1; then
    command -v gtimeout
    return 0
  fi
  echo "GNU timeout is required for auditable bounded follow-through runs; install coreutils or set AIOPS_TIMEOUT_BIN" >&2
  return 2
}

gh_cmd() {
  "$timeout_bin" "$gh_timeout" gh "$@"
}

run_with_timeout() {
  local duration="$1"
  shift
  "$timeout_bin" "$duration" "$@"
}

timeout_bin="$(resolve_timeout_bin)"

if [[ -z "${GITHUB_TOKEN:-}" ]]; then
  GITHUB_TOKEN="$(gh_cmd auth token -h github.com)"
  export GITHUB_TOKEN
fi

acquire_follow_through_lock

prepare_pr_worktree() {
  mkdir -p "$(dirname "$pr_worktree")"
  if [[ ! -d "$pr_worktree/.git" ]]; then
    rm -rf "$pr_worktree"
    git clone "https://github.com/${repo_path}.git" "$pr_worktree"
  fi
  cd "$pr_worktree"
  git remote set-url origin "https://github.com/${repo_path}.git"
  git fetch --prune origin --quiet
}

prepare_pr_worktree

if [[ "$#" -gt 0 ]]; then
  prs=("$@")
else
  prs=()
  while IFS= read -r pr; do
    if [[ -n "$pr" ]]; then
      prs+=("$pr")
    fi
  done < <(list_open_pr_numbers "$pr_label")
  assert_pr_scan_not_truncated "label:$pr_label" "${prs[@]}"
fi

if [[ "${#prs[@]}" -eq 0 ]]; then
  audit_log "no_open_prs" "label=$pr_label"
  echo "No open PRs with label $pr_label"
  exit 0
fi
all_open_prs=()
while IFS= read -r pr; do
  if [[ -n "$pr" ]]; then
    all_open_prs+=("$pr")
  fi
done < <(list_open_pr_numbers)
assert_pr_scan_not_truncated "all_open" "${all_open_prs[@]}"
assert_no_duplicate_closing_issue_prs "${all_open_prs[@]}"
assert_prs_claim_issues "${prs[@]}"

run_local_gates() {
  local fmt_out
  fmt_out="$(gofmt -l $(git ls-files '*.go'))"
  if [[ -n "$fmt_out" ]]; then
    echo "gofmt needed:" >&2
    echo "$fmt_out" >&2
    return 1
  fi
  go mod tidy
  git diff --exit-code -- go.mod go.sum
  run_go_quality_gate
  if [[ "${AIOPS_DOCKER_GATE:-0}" == "1" ]]; then
    docker build --tag "${AIOPS_DOCKER_GATE_TAG:-aiops-platform:local-gate}" .
  fi
}

run_go_quality_gate() {
  local mode="$gate_mode"
  if [[ "$mode" == "auto" ]]; then
    mode="local"
    if [[ "$(uname -s)" == "Darwin" ]]; then
      mode="docker"
    fi
  fi
  case "$mode" in
    local)
      go test -race -covermode=atomic ./...
      go build ./cmd/worker ./cmd/linear-poller ./cmd/gitea-poller
      ;;
    docker)
      run_go_quality_gate_in_docker
      ;;
    *)
      echo "unsupported AIOPS_GATE_MODE=$gate_mode (allowed: auto, local, docker)" >&2
      return 2
      ;;
  esac
}

run_go_quality_gate_in_docker() {
  local go_build_cache go_mod_cache image
  image="${AIOPS_GO_GATE_IMAGE:-golang:1.26-bookworm}"
  go_build_cache="${AIOPS_DOCKER_GO_BUILD_CACHE:-"$HOME/Library/Caches/aiops-platform/go-build"}"
  go_mod_cache="${AIOPS_DOCKER_GOMODCACHE:-"$HOME/Library/Caches/aiops-platform/go-mod"}"
  mkdir -p "$go_build_cache" "$go_mod_cache"
  docker run --rm \
    -v "$PWD:/src" \
    -v "$go_build_cache:/root/.cache/go-build" \
    -v "$go_mod_cache:/go/pkg/mod" \
    -w /src \
    "$image" \
    bash -c 'export PATH=/usr/local/go/bin:$PATH; go test -race -covermode=atomic ./... && go build ./cmd/worker ./cmd/linear-poller ./cmd/gitea-poller'
}

validate_review_json() {
  local reviewer="$1"
  local review_file="$2"
  python3 - "$reviewer" "$review_file" <<'PY'
import json
import sys
reviewer, path = sys.argv[1], sys.argv[2]
text = open(path, "r", encoding="utf-8").read().strip()
try:
    data = json.loads(text)
except json.JSONDecodeError as exc:
    print(f"{reviewer} review did not return valid JSON: {exc}", file=sys.stderr)
    print(text, file=sys.stderr)
    sys.exit(1)
findings = data.get("blocking_findings")
if not isinstance(findings, list):
    print(f"{reviewer} review JSON missing blocking_findings list", file=sys.stderr)
    sys.exit(1)
if findings:
    print(json.dumps({"reviewer": reviewer, **data}, ensure_ascii=False, indent=2), file=sys.stderr)
    sys.exit(2)
print(f"{reviewer} independent review: no blocking findings")
PY
}

write_review_schema() {
  local path="$1"
  cat > "$path" <<'JSON'
{
  "type": "object",
  "properties": {
    "blocking_findings": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "severity": { "type": "string", "enum": ["high", "medium", "low"] },
          "file": { "type": "string" },
          "line": { "type": "integer", "minimum": 1 },
          "issue": { "type": "string" }
        },
        "required": ["severity", "file", "line", "issue"],
        "additionalProperties": false
      }
    }
  },
  "required": ["blocking_findings"],
  "additionalProperties": false
}
JSON
}

run_claude_review() {
  local pr="$1"
  local diff_file="$2"
  local review_file="$3"
  local prompt_file="${review_file}.prompt"
  local schema_file="${review_file}.schema.json"
  local raw_file="${review_file}.raw.json"
  local max_turns="${AIOPS_CLAUDE_REVIEW_MAX_TURNS:-6}"
  if ! command -v claude >/dev/null 2>&1; then
    echo "Claude Code is required for the local independent review gate" >&2
    return 1
  fi
  write_review_schema "$schema_file"
  {
    cat <<'PROMPT'
Review this PR diff for correctness, races, SPEC alignment, security, and missing tests.
Use only the supplied diff. Do not inspect repository files. Do not call tools.
Return JSON only with this exact shape:
{"blocking_findings":[{"severity":"high|medium|low","file":"path","line":1,"issue":"text"}]}
If there are no blocking findings, return {"blocking_findings":[]}.
Do not include Markdown fences.

<diff>
PROMPT
    cat "$diff_file"
    printf '\n</diff>\n'
  } > "$prompt_file"
  if ! run_with_timeout "$review_timeout" claude -p \
    --permission-mode bypassPermissions \
    --no-session-persistence \
    --tools "" \
    --output-format json \
    --json-schema "$(cat "$schema_file")" \
    --max-turns "$max_turns" \
    < "$prompt_file" > "$raw_file"; then
    echo "Claude local independent review failed for PR #$pr" >&2
    if [[ -s "$raw_file" ]]; then
      cat "$raw_file" >&2
    fi
    return 1
  fi
  if ! jq -e '.is_error == false and (.structured_output | type == "object")' "$raw_file" >/dev/null; then
    echo "Claude local independent review did not return structured output for PR #$pr" >&2
    cat "$raw_file" >&2
    return 1
  fi
  jq -c '.structured_output' "$raw_file" > "$review_file"
  validate_review_json "Claude Code" "$review_file"
}

run_codex_review() {
  local pr="$1"
  local diff_file="$2"
  local review_file="$3"
  local output_file="${review_file}.stdout"
  local prompt_file="${review_file}.prompt"
  local schema_file="${review_file}.schema.json"
  if ! command -v codex >/dev/null 2>&1; then
    echo "Codex is required for the local independent review gate" >&2
    return 1
  fi
  {
    cat <<'PROMPT'
Review this PR diff for correctness, races, SPEC alignment, security, and missing tests.
Use only the supplied diff and repository context. Do not edit files.
Return JSON only with this exact shape:
{"blocking_findings":[{"severity":"high|medium|low","file":"path","line":1,"issue":"text"}]}
If there are no blocking findings, return {"blocking_findings":[]}.
Do not include Markdown fences.

<diff>
PROMPT
    cat "$diff_file"
    printf '\n</diff>\n'
  } > "$prompt_file"
  write_review_schema "$schema_file"
  if ! run_with_timeout "$review_timeout" codex exec \
    --sandbox read-only \
    --skip-git-repo-check \
    --ephemeral \
    --cd "$PWD" \
    --output-schema "$schema_file" \
    -o "$review_file" \
    - \
    < "$prompt_file" \
    > "$output_file" 2>&1; then
    echo "Codex local independent review failed for PR #$pr" >&2
    cat "$output_file" >&2
    rm -f "$schema_file"
    return 1
  fi
  rm -f "$schema_file"
  if [[ ! -s "$review_file" ]]; then
    cp "$output_file" "$review_file"
  fi
  validate_review_json "Codex" "$review_file"
}

run_local_reviews() {
  local pr="$1"
  local head_oid="$2"
  local base_oid="$3"
  local base_ref="$4"
  local diff_file claude_file codex_file artifacts_dir
  if local_reviews_already_passed "$pr" "$head_oid" "$base_oid" "$base_ref"; then
    audit_log "local_reviews_cache_hit" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref state_file=$(review_state_file "$pr" "$head_oid" "$base_oid" "$base_ref") artifacts_dir=$(review_artifact_dir "$pr" "$head_oid" "$base_oid" "$base_ref")"
    echo "Local independent reviews already passed for PR #$pr at $head_oid against base $base_ref@$base_oid"
    return 0
  fi
  if local_reviews_already_failed "$pr" "$head_oid" "$base_oid" "$base_ref"; then
    audit_log "local_reviews_failed_cache_hit" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref state_file=$(review_state_file "$pr" "$head_oid" "$base_oid" "$base_ref") artifacts_dir=$(review_artifact_dir "$pr" "$head_oid" "$base_oid" "$base_ref")"
    echo "Local independent reviews previously failed for PR #$pr at $head_oid against base $base_ref@$base_oid; waiting for a new head/base SHA"
    return 1
  fi
  diff_file="$(mktemp "${TMPDIR:-/tmp}/aiops-pr-${pr}-diff.XXXXXX")"
  claude_file="$(mktemp "${TMPDIR:-/tmp}/aiops-pr-${pr}-claude-review.XXXXXX")"
  codex_file="$(mktemp "${TMPDIR:-/tmp}/aiops-pr-${pr}-codex-review.XXXXXX")"
  git diff "${base_oid}...HEAD" > "$diff_file"
  if [[ ! -s "$diff_file" ]]; then
    echo "PR #$pr has no diff against $base_ref@$base_oid"
    rm -f "$diff_file" "$claude_file" "$codex_file" "${codex_file}.stdout"
    return 1
  fi
  run_claude_review "$pr" "$diff_file" "$claude_file" &
  claude_pid=$!
  run_codex_review "$pr" "$diff_file" "$codex_file" &
  codex_pid=$!

  claude_status=0
  codex_status=0
  wait "$claude_pid" || claude_status=$?
  wait "$codex_pid" || codex_status=$?
  artifacts_dir="$(preserve_local_review_artifacts "$pr" "$head_oid" "$base_oid" "$base_ref" "$claude_file" "$codex_file")"
  rm -f "$diff_file" "$claude_file" "$codex_file" "${claude_file}.prompt" "${claude_file}.raw.json" "${codex_file}.stdout" "${codex_file}.prompt" "${claude_file}.schema.json" "${codex_file}.schema.json"
  if [[ "$claude_status" -ne 0 || "$codex_status" -ne 0 ]]; then
    mark_local_reviews_failed "$pr" "$head_oid" "$base_oid" "$base_ref" "$claude_status" "$codex_status" "$artifacts_dir"
    return 1
  fi
  mark_local_reviews_passed "$pr" "$head_oid" "$base_oid" "$base_ref" "$artifacts_dir"
}

assert_no_actionable_threads() {
  local pr="$1"
  local cursor after_clause payload active has_next
  cursor=""
  while true; do
    after_clause=""
    if [[ -n "$cursor" ]]; then
      after_clause=", after: \"${cursor}\""
    fi
    payload="$(gh_cmd api graphql \
      -F owner="$repo_owner" \
      -F name="$repo_name" \
      -F number="$pr" \
      -f query="query(\$owner:String!, \$name:String!, \$number:Int!) {
      repository(owner:\$owner, name:\$name) {
        pullRequest(number:\$number) {
          reviewThreads(first:100${after_clause}) {
            nodes { id isResolved isOutdated }
            pageInfo { hasNextPage endCursor }
          }
        }
      }
    }")"
    active="$(jq -r '[.data.repository.pullRequest.reviewThreads.nodes[] | select((.isResolved | not) and (.isOutdated | not)) | .id] | join(",")' <<<"$payload")"
    if [[ -n "$active" ]]; then
      echo "unresolved actionable review threads: $active" >&2
      return 1
    fi
    has_next="$(jq -r '.data.repository.pullRequest.reviewThreads.pageInfo.hasNextPage' <<<"$payload")"
    if [[ "$has_next" != "true" ]]; then
      return 0
    fi
    cursor="$(jq -r '.data.repository.pullRequest.reviewThreads.pageInfo.endCursor // ""' <<<"$payload")"
    if [[ -z "$cursor" ]]; then
      echo "reviewThreads pagination reported hasNextPage without endCursor" >&2
      return 1
    fi
  done
}

assert_review_decision_clean() {
  local pr="$1"
  local decision
  decision="$(gh_cmd pr view -R "$repo_path" "$pr" --json reviewDecision --jq '.reviewDecision // ""')"
  if [[ "$decision" == "CHANGES_REQUESTED" ]]; then
    echo "PR #$pr has CHANGES_REQUESTED review decision" >&2
    return 1
  fi
}

find_existing_github_codex_review_trigger() {
  local pr="$1"
  local head_oid="$2"
  local base_oid="$3"
  local base_ref="$4"
  local head_commit_at comments_json
  head_commit_at="$(gh_cmd api "repos/${repo_owner}/${repo_name}/commits/${head_oid}" --jq '.commit.committer.date // .commit.author.date')"
  comments_json="$(gh_cmd api --paginate --slurp "repos/${repo_owner}/${repo_name}/issues/${pr}/comments?per_page=100")"
  COMMENTS_JSON="$comments_json" python3 - "$head_commit_at" "$head_oid" "$base_oid" "$base_ref" <<'PY'
import json
import os
import sys

head_commit_at = sys.argv[1]
head_oid = sys.argv[2]
base_oid = sys.argv[3]
base_ref = sys.argv[4]
raw_comments = json.loads(os.environ["COMMENTS_JSON"])
if raw_comments and isinstance(raw_comments[0], list):
    comments = [comment for page in raw_comments for comment in page]
else:
    comments = raw_comments
matches = []
for comment in comments:
    body = (comment.get("body") or "").strip()
    created_at = comment.get("created_at") or ""
    if "@codex review" not in body.lower():
        continue
    if head_oid not in body or base_oid not in body or base_ref not in body:
        continue
    if created_at < head_commit_at:
        continue
    matches.append(comment)
if not matches:
    sys.exit(1)
latest = sorted(matches, key=lambda item: item.get("created_at") or "")[-1]
print(json.dumps({"trigger_id": str(latest["id"]), "started_at": latest["created_at"]}))
PY
}

wait_for_github_codex_review() {
  local pr="$1"
  local head_oid="$2"
  local base_oid="$3"
  local base_ref="$4"
  local cached_comment_json cached_body existing_trigger trigger_json trigger_id started_at state_file tmp
  state_file="$(github_codex_review_state_file "$pr" "$head_oid" "$base_oid" "$base_ref")"
  trigger_id="$(jq -r '.trigger_id // empty' "$state_file" 2>/dev/null || true)"
  started_at="$(jq -r '.started_at // empty' "$state_file" 2>/dev/null || true)"
  if [[ -n "$trigger_id" && -n "$started_at" ]] && cached_comment_json="$(gh_cmd api "repos/${repo_owner}/${repo_name}/issues/comments/${trigger_id}" 2>/dev/null)"; then
    cached_body="$(jq -r '.body // ""' <<<"$cached_comment_json")"
  fi
  if [[ -n "$trigger_id" && -n "$started_at" && "$cached_body" == *"$head_oid"* && "$cached_body" == *"$base_oid"* && "$cached_body" == *"$base_ref"* ]]; then
    audit_log "github_codex_review_reused" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref trigger_id=$trigger_id state_file=$state_file"
    echo "Reusing GitHub Codex review trigger for PR #$pr at $head_oid against $base_ref@$base_oid (comment $trigger_id)"
  elif existing_trigger="$(find_existing_github_codex_review_trigger "$pr" "$head_oid" "$base_oid" "$base_ref")"; then
    trigger_id="$(jq -r '.trigger_id' <<<"$existing_trigger")"
    started_at="$(jq -r '.started_at' <<<"$existing_trigger")"
    tmp="${state_file}.$$"
    jq -n \
      --arg pr "$pr" \
      --arg head_oid "$head_oid" \
      --arg base_oid "$base_oid" \
      --arg base_ref "$base_ref" \
      --arg trigger_id "$trigger_id" \
      --arg started_at "$started_at" \
      '{pr:$pr, head_oid:$head_oid, base_oid:$base_oid, base_ref:$base_ref, trigger_id:$trigger_id, started_at:$started_at, source:"existing_comment"}' > "$tmp"
    mv "$tmp" "$state_file"
    audit_log "github_codex_review_existing_trigger_found" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref trigger_id=$trigger_id state_file=$state_file"
    echo "Using existing GitHub Codex review trigger for PR #$pr at $head_oid against $base_ref@$base_oid (comment $trigger_id)"
  else
    trigger_json="$(gh_cmd api \
      -X POST \
      "repos/${repo_owner}/${repo_name}/issues/${pr}/comments" \
      -f body="@codex review

head: ${head_oid}
base: ${base_oid}
base_ref: ${base_ref}")"
    trigger_id="$(jq -r '.id' <<<"$trigger_json")"
    started_at="$(jq -r '.created_at' <<<"$trigger_json")"
    tmp="${state_file}.$$"
    jq -n \
      --arg pr "$pr" \
      --arg head_oid "$head_oid" \
      --arg base_oid "$base_oid" \
      --arg base_ref "$base_ref" \
      --arg trigger_id "$trigger_id" \
      --arg started_at "$started_at" \
      '{pr:$pr, head_oid:$head_oid, base_oid:$base_oid, base_ref:$base_ref, trigger_id:$trigger_id, started_at:$started_at}' > "$tmp"
    mv "$tmp" "$state_file"
    audit_log "github_codex_review_triggered" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref trigger_id=$trigger_id state_file=$state_file"
    echo "Triggered GitHub Codex review for PR #$pr at $head_oid against $base_ref@$base_oid (comment $trigger_id)"
  fi

  run_with_timeout "$github_codex_review_timeout" bash -c '
    set -euo pipefail
    repo_owner="$1"
    repo_name="$2"
    pr="$3"
    head_oid="$4"
    base_oid="$5"
    base_ref="$6"
    trigger_id="$7"
    started_at="$8"
    poll_seconds="$9"
    while true; do
      current_refs="$(gh api "repos/${repo_owner}/${repo_name}/pulls/${pr}" --jq "{head:.head.sha, base:.base.sha, base_ref:.base.ref}")"
      current_head="$(jq -r ".head" <<<"$current_refs")"
      current_base="$(jq -r ".base" <<<"$current_refs")"
      current_base_ref="$(jq -r ".base_ref" <<<"$current_refs")"
      if [[ "$current_head" != "$head_oid" ]]; then
        echo "PR #$pr head changed during GitHub Codex review: $current_head != $head_oid" >&2
        exit 1
      fi
      if [[ "$current_base" != "$base_oid" || "$current_base_ref" != "$base_ref" ]]; then
        echo "PR #$pr base changed during GitHub Codex review: $current_base_ref@$current_base != $base_ref@$base_oid" >&2
        exit 1
      fi
      comment_json="$(gh api "repos/${repo_owner}/${repo_name}/issues/comments/${trigger_id}")"
      comment_body="$(jq -r ".body // \"\"" <<<"$comment_json")"
      if [[ "$comment_body" != *"$head_oid"* || "$comment_body" != *"$base_oid"* || "$comment_body" != *"$base_ref"* ]]; then
        echo "GitHub Codex review trigger comment is not bound to head $head_oid and base $base_ref@$base_oid" >&2
        exit 1
      fi
      eyes="$(jq -r ".reactions.eyes // 0" <<<"$comment_json")"
      reactions_json="$(gh api --paginate --slurp -H "Accept: application/vnd.github+json" "repos/${repo_owner}/${repo_name}/issues/comments/${trigger_id}/reactions?per_page=100")"
      bot_plus_one="$(REACTIONS_JSON="$reactions_json" python3 - <<'"'"'PY'"'"'
import json
import os

raw_reactions = json.loads(os.environ["REACTIONS_JSON"])
if raw_reactions and isinstance(raw_reactions[0], list):
    reactions = [reaction for page in raw_reactions for reaction in page]
else:
    reactions = raw_reactions
count = 0
for reaction in reactions:
    user = reaction.get("user") or {}
    if reaction.get("content") == "+1" and user.get("login") == "chatgpt-codex-connector":
        count += 1
print(count)
PY
)"
      if [[ "$eyes" == "0" && "$bot_plus_one" != "0" ]]; then
        exit 0
      fi
      sleep "$poll_seconds"
    done
  ' bash "$repo_owner" "$repo_name" "$pr" "$head_oid" "$base_oid" "$base_ref" "$trigger_id" "$started_at" "$github_codex_review_poll_seconds"
  assert_no_actionable_threads "$pr"
}

for pr in "${prs[@]}"; do
  echo "== PR #$pr =="
  audit_log "pr_started" "pr=$pr"
  cd "$pr_worktree"
  git reset --hard HEAD >/dev/null
  git clean -fdx >/dev/null
  gh_cmd pr checkout -R "$repo_path" "$pr" --force
  pr_refs="$(current_pr_ref_json "$pr")"
  head_oid="$(jq -r '.headRefOid' <<<"$pr_refs")"
  base_oid="$(jq -r '.baseRefOid' <<<"$pr_refs")"
  base_ref="$(jq -r '.baseRefName' <<<"$pr_refs")"
  git fetch origin "+refs/heads/${base_ref}:refs/remotes/origin/${base_ref}" --quiet
  fetched_base_oid="$(git rev-parse "origin/${base_ref}")"
  if [[ "$fetched_base_oid" != "$base_oid" ]]; then
    audit_log "base_fetch_mismatch" "pr=$pr base_ref=$base_ref expected_base=$base_oid fetched_base=$fetched_base_oid"
    echo "PR #$pr base fetch mismatch: origin/$base_ref is $fetched_base_oid, GitHub reported $base_oid" >&2
    exit 1
  fi
  assert_local_head_matches_pr "$pr" "$head_oid"

  if local_reviews_already_failed "$pr" "$head_oid" "$base_oid" "$base_ref"; then
    audit_log "local_reviews_failed_cache_hit" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref state_file=$(review_state_file "$pr" "$head_oid" "$base_oid" "$base_ref") artifacts_dir=$(review_artifact_dir "$pr" "$head_oid" "$base_oid" "$base_ref") stage=before_local_gates"
    echo "Local independent reviews previously failed for PR #$pr at $head_oid against base $base_ref@$base_oid; skipping local gates until the PR head or base changes"
    continue
  fi

  audit_log "local_gates_started" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref mode=$gate_mode"
  run_local_gates
  audit_log "local_gates_passed" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref"
  assert_pr_refs_unchanged "$pr" "$head_oid" "$base_oid" "$base_ref" "after_local_gates"
  audit_log "local_reviews_started" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref"
  run_local_reviews "$pr" "$head_oid" "$base_oid" "$base_ref"
  audit_log "local_reviews_passed" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref"
  assert_pr_refs_unchanged "$pr" "$head_oid" "$base_oid" "$base_ref" "after_local_reviews"
  assert_no_actionable_threads "$pr"
  assert_review_decision_clean "$pr"

  gh_cmd pr ready -R "$repo_path" "$pr" >/dev/null 2>&1 || true
  assert_pr_refs_unchanged "$pr" "$head_oid" "$base_oid" "$base_ref" "before_github_codex_review"
  wait_for_github_codex_review "$pr" "$head_oid" "$base_oid" "$base_ref"
  audit_log "github_codex_review_passed" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref"
  assert_pr_refs_unchanged "$pr" "$head_oid" "$base_oid" "$base_ref" "before_github_checks"
  audit_log "github_checks_started" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref timeout=$checks_timeout"
  run_with_timeout "$checks_timeout" gh pr checks -R "$repo_path" "$pr" --watch
  audit_log "github_checks_passed" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref"
  assert_pr_refs_unchanged "$pr" "$head_oid" "$base_oid" "$base_ref" "after_github_checks"
  assert_no_actionable_threads "$pr"
  assert_review_decision_clean "$pr"

  if [[ "$auto_merge" == "1" ]]; then
    audit_log "merge_requested" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref method=squash auto=1"
    gh_cmd pr merge -R "$repo_path" "$pr" --squash --auto --delete-branch --match-head-commit "$head_oid"
    audit_log "merge_request_completed" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref"
  else
    audit_log "merge_skipped" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref auto_merge=$auto_merge"
    echo "AIOPS_AUTO_MERGE=$auto_merge; leaving PR #$pr unmerged"
  fi
  audit_log "pr_completed" "pr=$pr head=$head_oid base=$base_oid base_ref=$base_ref"
done
