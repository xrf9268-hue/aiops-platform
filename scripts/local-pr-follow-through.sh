#!/usr/bin/env bash
set -euo pipefail

export PATH="$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/Applications/Codex.app/Contents/Resources:${PATH:-}"
export GH_PROMPT_DISABLED=1

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
base_branch="${AIOPS_BASE_BRANCH:-main}"
pr_label="${AIOPS_PR_LABEL:-ai-generated}"
auto_merge="${AIOPS_AUTO_MERGE:-1}"
repo_path="${AIOPS_GITHUB_REPO:-xrf9268-hue/aiops-platform}"
repo_path="${repo_path#github.com/}"
repo_owner="${repo_path%/*}"
repo_name="${repo_path##*/}"
pr_worktree="${AIOPS_PR_WORKTREE:-"$HOME/aiops-workspaces/github/xrf9268-hue-aiops-platform-pr-follow-through"}"
gh_timeout="${AIOPS_GH_TIMEOUT:-60s}"
gate_mode="${AIOPS_GATE_MODE:-auto}"
review_timeout="${AIOPS_REVIEW_TIMEOUT:-20m}"
github_codex_review_timeout="${AIOPS_GITHUB_CODEX_REVIEW_TIMEOUT:-20m}"
github_codex_review_poll_seconds="${AIOPS_GITHUB_CODEX_REVIEW_POLL_SECONDS:-30}"

cd "$repo_root"

gh_cmd() {
  if command -v timeout >/dev/null 2>&1; then
    timeout "$gh_timeout" gh "$@"
  else
    gh "$@"
  fi
}

run_with_timeout() {
  local duration="$1"
  shift
  if command -v timeout >/dev/null 2>&1; then
    timeout "$duration" "$@"
  else
    "$@"
  fi
}

if [[ -z "${GITHUB_TOKEN:-}" ]]; then
  GITHUB_TOKEN="$(gh_cmd auth token -h github.com)"
  export GITHUB_TOKEN
fi

prepare_pr_worktree() {
  mkdir -p "$(dirname "$pr_worktree")"
  if [[ ! -d "$pr_worktree/.git" ]]; then
    rm -rf "$pr_worktree"
    git clone "https://github.com/${repo_path}.git" "$pr_worktree"
  fi
  cd "$pr_worktree"
  git remote set-url origin "https://github.com/${repo_path}.git"
  git fetch --prune origin "$base_branch" --quiet
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
  done < <(gh_cmd pr list -R "$repo_path" --state open --label "$pr_label" --json number --jq '.[].number')
fi

if [[ "${#prs[@]}" -eq 0 ]]; then
  echo "No open PRs with label $pr_label"
  exit 0
fi

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
    rm -f "$schema_file" "$prompt_file" "$raw_file"
    return 1
  fi
  if ! jq -e '.is_error == false and (.structured_output | type == "object")' "$raw_file" >/dev/null; then
    echo "Claude local independent review did not return structured output for PR #$pr" >&2
    cat "$raw_file" >&2
    rm -f "$schema_file" "$prompt_file" "$raw_file"
    return 1
  fi
  jq -c '.structured_output' "$raw_file" > "$review_file"
  rm -f "$schema_file" "$prompt_file" "$raw_file"
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
  local diff_file claude_file codex_file
  diff_file="$(mktemp "${TMPDIR:-/tmp}/aiops-pr-${pr}-diff.XXXXXX")"
  claude_file="$(mktemp "${TMPDIR:-/tmp}/aiops-pr-${pr}-claude-review.XXXXXX")"
  codex_file="$(mktemp "${TMPDIR:-/tmp}/aiops-pr-${pr}-codex-review.XXXXXX")"
  git diff "origin/${base_branch}...HEAD" > "$diff_file"
  if [[ ! -s "$diff_file" ]]; then
    echo "PR #$pr has no diff against origin/$base_branch"
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
  rm -f "$diff_file" "$claude_file" "$codex_file" "${claude_file}.prompt" "${claude_file}.raw.json" "${codex_file}.stdout" "${codex_file}.prompt" "${claude_file}.schema.json" "${codex_file}.schema.json"
  if [[ "$claude_status" -ne 0 || "$codex_status" -ne 0 ]]; then
    return 1
  fi
}

assert_no_actionable_threads() {
  local pr="$1"
  local payload
  payload="$(gh_cmd api graphql \
    -F owner="$repo_owner" \
    -F name="$repo_name" \
    -F number="$pr" \
    -f query='query($owner:String!, $name:String!, $number:Int!) {
      repository(owner:$owner, name:$name) {
        pullRequest(number:$number) {
          reviewThreads(first:100) {
            nodes { id isResolved isOutdated }
          }
        }
      }
    }')"
  python3 - "$payload" <<'PY'
import json
import sys
payload = json.loads(sys.argv[1])
threads = payload["data"]["repository"]["pullRequest"]["reviewThreads"]["nodes"]
active = [t["id"] for t in threads if not t["isResolved"] and not t["isOutdated"]]
if active:
    print("unresolved actionable review threads:", ", ".join(active), file=sys.stderr)
    sys.exit(1)
PY
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

wait_for_github_codex_review() {
  local pr="$1"
  local head_oid="$2"
  local trigger_json trigger_id started_at
  trigger_json="$(gh_cmd api \
    -X POST \
    "repos/${repo_owner}/${repo_name}/issues/${pr}/comments" \
    -f body='@codex review')"
  trigger_id="$(jq -r '.id' <<<"$trigger_json")"
  started_at="$(jq -r '.created_at' <<<"$trigger_json")"
  echo "Triggered GitHub Codex review for PR #$pr at $head_oid (comment $trigger_id)"

  run_with_timeout "$github_codex_review_timeout" bash -c '
    set -euo pipefail
    repo_owner="$1"
    repo_name="$2"
    pr="$3"
    head_oid="$4"
    trigger_id="$5"
    started_at="$6"
    poll_seconds="$7"
    while true; do
      current_head="$(gh api "repos/${repo_owner}/${repo_name}/pulls/${pr}" --jq ".head.sha")"
      if [[ "$current_head" != "$head_oid" ]]; then
        echo "PR #$pr head changed during GitHub Codex review: $current_head != $head_oid" >&2
        exit 1
      fi
      comment_json="$(gh api "repos/${repo_owner}/${repo_name}/issues/comments/${trigger_id}")"
      eyes="$(jq -r ".reactions.eyes // 0" <<<"$comment_json")"
      plus_one="$(jq -r ".reactions[\"+1\"] // 0" <<<"$comment_json")"
      comments_json="$(gh api "repos/${repo_owner}/${repo_name}/issues/${pr}/comments?per_page=100")"
      bot_activity="$(COMMENTS_JSON="$comments_json" python3 - "$started_at" <<'PY'
import json
import os
import sys

started_at = sys.argv[1]
comments = json.loads(os.environ["COMMENTS_JSON"])
print(sum(1 for c in comments if c.get("created_at", "") >= started_at and c.get("user", {}).get("login") == "chatgpt-codex-connector"))
PY
)"
      if [[ "$eyes" == "0" ]] && { [[ "$plus_one" != "0" ]] || [[ "$bot_activity" != "0" ]]; }; then
        exit 0
      fi
      sleep "$poll_seconds"
    done
  ' bash "$repo_owner" "$repo_name" "$pr" "$head_oid" "$trigger_id" "$started_at" "$github_codex_review_poll_seconds"
  assert_no_actionable_threads "$pr"
}

for pr in "${prs[@]}"; do
  echo "== PR #$pr =="
  cd "$pr_worktree"
  git reset --hard HEAD >/dev/null
  git clean -fdx >/dev/null
  gh_cmd pr checkout -R "$repo_path" "$pr" --force
  git fetch origin "$base_branch" --quiet

  run_local_gates
  run_local_reviews "$pr"
  assert_no_actionable_threads "$pr"
  assert_review_decision_clean "$pr"

  gh_cmd pr ready -R "$repo_path" "$pr" >/dev/null 2>&1 || true
  head_oid="$(gh_cmd pr view -R "$repo_path" "$pr" --json headRefOid --jq '.headRefOid')"
  wait_for_github_codex_review "$pr" "$head_oid"
  gh_cmd pr checks -R "$repo_path" "$pr" --watch
  assert_no_actionable_threads "$pr"
  assert_review_decision_clean "$pr"

  if [[ "$auto_merge" == "1" ]]; then
    gh_cmd pr merge -R "$repo_path" "$pr" --squash --auto --delete-branch
  else
    echo "AIOPS_AUTO_MERGE=$auto_merge; leaving PR #$pr unmerged"
  fi
done
