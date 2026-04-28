#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="$repo_root/scripts/enqueue-manual-task.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

help_output="$("$script" --help)"
for field in REPO_OWNER REPO_NAME CLONE_URL BASE_BRANCH; do
  grep -q "$field" <<<"$help_output" || fail "help output is missing $field"
done

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

cat >"$tmpdir/curl" <<'FAKE_CURL'
#!/usr/bin/env bash
set -euo pipefail

args_file="${FAKE_CURL_ARGS:?}"
payload_file="${FAKE_CURL_PAYLOAD:?}"

printf '%s\n' "$@" >"$args_file"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --data-binary)
      shift
      if [[ "${1:-}" == @* ]]; then
        cat "${1#@}" >"$payload_file"
      else
        printf '%s' "${1:-}" >"$payload_file"
      fi
      ;;
  esac
  shift || true
done

printf '{"task_id":"tsk_test_manual","deduped":false}\n'
FAKE_CURL
chmod +x "$tmpdir/curl"

export PATH="$tmpdir:$PATH"
export FAKE_CURL_ARGS="$tmpdir/curl.args"
export FAKE_CURL_PAYLOAD="$tmpdir/payload.json"
export AIOPS_API_URL="http://127.0.0.1:18080"
export REPO_OWNER="octo"
export REPO_NAME="demo"
export CLONE_URL="git@example.com:octo/demo.git"
export BASE_BRANCH="main"
export TITLE="Smoke test task"
export DESCRIPTION="Created by script test"
export ACTOR="tester"

output="$("$script")"

grep -q "Task enqueued: tsk_test_manual" <<<"$output" || fail "output does not print task id"
grep -q "select id,status" <<<"$output" || fail "output does not print next verification command"
grep -q -- "-X" "$FAKE_CURL_ARGS" || fail "curl was not called with -X"
grep -q "POST" "$FAKE_CURL_ARGS" || fail "curl was not called with POST"
grep -q "http://127.0.0.1:18080/v1/tasks" "$FAKE_CURL_ARGS" || fail "curl did not call /v1/tasks"
grep -q '"repo_owner":"octo"' "$FAKE_CURL_PAYLOAD" || fail "payload missing repo_owner"
grep -q '"repo_name":"demo"' "$FAKE_CURL_PAYLOAD" || fail "payload missing repo_name"
grep -q '"clone_url":"git@example.com:octo/demo.git"' "$FAKE_CURL_PAYLOAD" || fail "payload missing clone_url"
grep -q '"base_branch":"main"' "$FAKE_CURL_PAYLOAD" || fail "payload missing base_branch"

echo "PASS: enqueue-manual-task smoke test"
