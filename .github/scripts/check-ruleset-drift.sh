#!/usr/bin/env bash
set -euo pipefail

repo="${AIOPS_RULESET_REPOSITORY:-${GITHUB_REPOSITORY:-xrf9268-hue/aiops-platform}}"
ruleset_name="${AIOPS_RULESET_NAME:-main merge governance}"
ruleset_target="${AIOPS_RULESET_TARGET:-branch}"
source_file="${AIOPS_RULESET_SOURCE_FILE:-.github/governance/main-ruleset.json}"
source_display="${AIOPS_RULESET_SOURCE_DISPLAY:-.github/governance/main-ruleset.json}"

tmpdir="$(mktemp -d)"
cleanup() {
  rm -rf "$tmpdir"
}
trap cleanup EXIT

fail_with_annotation() {
  local message="$1"
  printf '::error file=%s,title=Main branch ruleset drift::%s\n' "$source_display" "$message"
}

normalize_ruleset() {
  local input="$1"
  jq -S '
    def normalize_rule:
      if .type == "pull_request" then
        .parameters |= if .required_reviewers? == [] then del(.required_reviewers) else . end
      else
        .
      end;

    {
      name,
      target,
      enforcement,
      conditions,
      rules: ((.rules // []) | map(normalize_rule))
    }
  ' "$input"
}

command -v jq >/dev/null 2>&1 || {
  fail_with_annotation "jq is required to normalize GitHub rulesets."
  exit 1
}

live_raw="$tmpdir/live-ruleset.json"
if [[ -n "${AIOPS_RULESET_DRIFT_LIVE_JSON:-}" ]]; then
  cp "$AIOPS_RULESET_DRIFT_LIVE_JSON" "$live_raw"
else
  command -v gh >/dev/null 2>&1 || {
    fail_with_annotation "gh is required to fetch the live GitHub ruleset."
    exit 1
  }

  rulesets_raw="$tmpdir/rulesets.json"
  gh api "repos/${repo}/rulesets?per_page=100&includes_parents=false&targets=${ruleset_target}" >"$rulesets_raw"
  match_count="$(
    jq -r --arg name "$ruleset_name" '
      [.[] | select(.name == $name)] | length
    ' "$rulesets_raw"
  )"
  if [[ "$match_count" != "1" ]]; then
    fail_with_annotation "Expected exactly one live ruleset named '${ruleset_name}' with target '${ruleset_target}', found ${match_count}."
    jq -r '.[] | "- \(.name) [\(.target // "target filtered by request")] id=\(.id)"' "$rulesets_raw"
    exit 1
  fi

  ruleset_id="$(
    jq -r --arg name "$ruleset_name" '
      [.[] | select(.name == $name)][0].id
    ' "$rulesets_raw"
  )"
  gh api "repos/${repo}/rulesets/${ruleset_id}?includes_parents=false" >"$live_raw"
fi

expected_normalized="$tmpdir/expected.normalized.json"
live_normalized="$tmpdir/live.normalized.json"
diff_file="$tmpdir/ruleset.diff"

normalize_ruleset "$source_file" >"$expected_normalized"
normalize_ruleset "$live_raw" >"$live_normalized"

if ! diff -u \
  --label "${source_display} (committed normalized)" \
  --label "live GitHub ruleset (normalized)" \
  "$expected_normalized" \
  "$live_normalized" >"$diff_file"; then
  fail_with_annotation "Live GitHub main ruleset differs from committed source; re-import the committed JSON or update it intentionally."
  cat "$diff_file"
  exit 1
fi

printf 'main branch ruleset matches %s after read-only normalization\n' "$source_display"
