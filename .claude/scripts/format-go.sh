#!/usr/bin/env bash
##
# PostToolUse Hook: gofmt edited Go files
#
# Official docs: https://code.claude.com/docs/en/hooks#posttooluse
#
# Triggered after Edit/Write. Runs `gofmt -w` on the edited file so it always
# matches the CI gate (`gofmt -l` must be empty; see AGENTS.md "Build, test,
# lint"). Silent and non-blocking: always exits 0 so it never nags or blocks.
##

set -euo pipefail

# jq parses the hook's stdin JSON; without it we can't find the file, so skip.
if ! command -v jq > /dev/null 2>&1; then
  exit 0
fi

# PostToolUse delivers {"tool_name":...,"tool_input":{"file_path":...},...} on stdin.
file="$(cat | jq -r '.tool_input.file_path // empty')"

# Only touch existing .go files; anything else is none of gofmt's business.
[[ -n "${file}" && "${file}" == *.go && -f "${file}" ]] || exit 0

if command -v gofmt > /dev/null 2>&1; then
  # Swallow errors: a mid-edit syntax error is surfaced later by build/test,
  # not something this formatting hook should block on.
  gofmt -w "${file}" > /dev/null 2>&1 || true
fi

exit 0
