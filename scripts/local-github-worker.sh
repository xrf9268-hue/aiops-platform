#!/usr/bin/env bash
set -euo pipefail

export PATH="$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/Applications/Codex.app/Contents/Resources:${PATH:-}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workflow_path="${AIOPS_WORKFLOW_PATH:-"$repo_root/examples/github-local-WORKFLOW.md"}"
workspace_root="${WORKSPACE_ROOT:-"$HOME/aiops-workspaces/github/xrf9268-hue-aiops-platform"}"
bin_dir="${AIOPS_BIN_DIR:-"$HOME/Library/Application Support/aiops-platform/bin"}"
worker_bin="$bin_dir/worker"

cd "$repo_root"

if [[ -z "${GITHUB_TOKEN:-}" ]]; then
  if ! command -v gh >/dev/null 2>&1; then
    echo "GITHUB_TOKEN is unset and gh is not available" >&2
    exit 2
  fi
  GITHUB_TOKEN="$(gh auth token -h github.com)"
  export GITHUB_TOKEN
fi

if [[ -z "${GITHUB_TOKEN:-}" ]]; then
  echo "GITHUB_TOKEN is required" >&2
  exit 2
fi

mkdir -p "$workspace_root" "$bin_dir"
export WORKSPACE_ROOT="$workspace_root"

go build -o "$worker_bin" ./cmd/worker
exec "$worker_bin" "$workflow_path"
