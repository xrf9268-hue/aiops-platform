#!/usr/bin/env bash
set -euo pipefail

export PATH="$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/Applications/Codex.app/Contents/Resources:${PATH:-}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workflow_path="${AIOPS_WORKFLOW_PATH:-"$repo_root/examples/github-local-WORKFLOW.md"}"
workspace_root="${WORKSPACE_ROOT:-"$HOME/aiops-workspaces/github/xrf9268-hue-aiops-platform"}"
bin_dir="${AIOPS_BIN_DIR:-"$HOME/Library/Application Support/aiops-platform/bin"}"
worker_bin="$bin_dir/worker"
worker_lock_key="$(printf '%s\n%s\n' "$workflow_path" "$workspace_root" | shasum -a 256 | awk '{print $1}')"
worker_lock_dir="${AIOPS_WORKER_LOCK_DIR:-"$HOME/Library/Caches/aiops-platform/github-worker-${worker_lock_key}.lock"}"
worker_lock_busy_sleep="${AIOPS_WORKER_LOCK_BUSY_SLEEP:-600}"
worker_lock_stale_seconds="${AIOPS_WORKER_LOCK_STALE_SECONDS:-3600}"

cd "$repo_root"

release_worker_lock() {
  if [[ -n "${worker_lock_acquired:-}" ]]; then
    rm -rf "$worker_lock_dir"
  fi
}

worker_lock_mtime_epoch() {
  stat -f %m "$worker_lock_dir" 2>/dev/null || stat -c %Y "$worker_lock_dir" 2>/dev/null || printf '0\n'
}

worker_lock_age_seconds() {
  local now mtime
  now="$(date +%s)"
  mtime="$(worker_lock_mtime_epoch)"
  if [[ "$mtime" =~ ^[0-9]+$ ]]; then
    printf '%s\n' "$((now - mtime))"
  else
    printf '0\n'
  fi
}

acquire_worker_lock() {
  if ! [[ "$worker_lock_stale_seconds" =~ ^[0-9]+$ ]]; then
    echo "AIOPS_WORKER_LOCK_STALE_SECONDS must be an integer number of seconds" >&2
    exit 2
  fi
  mkdir -p "$(dirname "$worker_lock_dir")"
  if mkdir "$worker_lock_dir" 2>/dev/null; then
    worker_lock_acquired=1
    if ! printf '%s\n' "$$" > "$worker_lock_dir/pid"; then
      rm -rf "$worker_lock_dir"
      echo "failed to write worker lock pid at $worker_lock_dir/pid" >&2
      exit 2
    fi
    trap 'release_worker_lock' EXIT INT TERM
    return 0
  fi

  local existing_pid lock_age
  existing_pid="$(cat "$worker_lock_dir/pid" 2>/dev/null || true)"
  if [[ -z "$existing_pid" ]]; then
    lock_age="$(worker_lock_age_seconds)"
    if ((lock_age < worker_lock_stale_seconds)); then
      echo "worker_lock_initializing age_seconds=$lock_age stale_after_seconds=$worker_lock_stale_seconds lock=$worker_lock_dir" >&2
      sleep "$worker_lock_busy_sleep"
      exit 0
    fi
  fi
  if [[ -n "$existing_pid" ]] && kill -0 "$existing_pid" 2>/dev/null; then
    echo "github worker already running for workflow/workspace (pid $existing_pid); lock=$worker_lock_dir" >&2
    sleep "$worker_lock_busy_sleep"
    exit 0
  fi

  rm -rf "$worker_lock_dir"
  if mkdir "$worker_lock_dir" 2>/dev/null; then
    worker_lock_acquired=1
    if ! printf '%s\n' "$$" > "$worker_lock_dir/pid"; then
      rm -rf "$worker_lock_dir"
      echo "failed to write worker lock pid at $worker_lock_dir/pid" >&2
      exit 2
    fi
    trap 'release_worker_lock' EXIT INT TERM
    return 0
  fi

  echo "github worker lock race lost; lock=$worker_lock_dir" >&2
  sleep "$worker_lock_busy_sleep"
  exit 0
}

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

acquire_worker_lock
go build -o "$worker_bin" ./cmd/worker
exec "$worker_bin" "$workflow_path"
