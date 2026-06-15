#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: ./scripts/dev-test.sh [quick]

Runs the credential-free local Go validation path:
  - verify the effective Go toolchain satisfies go.mod
  - check gofmt output
  - run go mod tidy and require go.mod/go.sum to stay clean
  - run go test ./...
USAGE
}

die() {
  echo "dev-test: $*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required on PATH"
}

version_ge() {
  local have="${1#go}"
  local need="$2"
  local have_major have_minor have_patch need_major need_minor need_patch
  IFS=. read -r have_major have_minor have_patch _ <<<"$have"
  IFS=. read -r need_major need_minor need_patch _ <<<"$need"
  have_patch="${have_patch:-0}"
  need_patch="${need_patch:-0}"
  for part in "$have_major" "$have_minor" "$have_patch" "$need_major" "$need_minor" "$need_patch"; do
    [[ "$part" =~ ^[0-9]+$ ]] || return 1
  done
  if ((have_major != need_major)); then
    ((have_major > need_major))
    return
  fi
  if ((have_minor != need_minor)); then
    ((have_minor > need_minor))
    return
  fi
  ((have_patch >= need_patch))
}

check_go_toolchain() {
  local required current
  required="$(awk '/^go [0-9]/ {print $2; exit}' go.mod)"
  [[ -n "$required" ]] || die "could not read Go version from go.mod"

  if ! current="$(go env GOVERSION 2>/dev/null)"; then
    die "go cannot satisfy go.mod's Go $required floor. With GOTOOLCHAIN=auto, modern Go can download the pinned toolchain; if this machine is offline, install Go $required first or pre-seed the toolchain."
  fi
  if ! version_ge "$current" "$required"; then
    die "effective Go toolchain is $current, but go.mod requires Go $required or newer. Leave GOTOOLCHAIN=auto to let Go download it, or install Go $required before running local validation."
  fi
  echo "Go toolchain OK: $current satisfies go.mod Go $required"
}

check_gofmt() {
  local go_files fmt_out
  go_files="$(git ls-files '*.go')"
  if [[ -z "$go_files" ]]; then
    return
  fi
  fmt_out="$(printf '%s\n' "$go_files" | xargs gofmt -l)"
  if [[ -n "$fmt_out" ]]; then
    echo "dev-test: gofmt needed:" >&2
    echo "$fmt_out" >&2
    exit 1
  fi
}

mode="${1:-quick}"
case "$mode" in
  -h|--help)
    usage
    exit 0
    ;;
  quick)
    ;;
  *)
    usage >&2
    die "unknown mode: $mode"
    ;;
esac

require_command git
require_command go
require_command gofmt

repo_root="$(git rev-parse --show-toplevel 2>/dev/null)" || die "must run inside a git checkout"
cd "$repo_root"

check_go_toolchain

echo "==> checking gofmt"
check_gofmt

echo "==> verifying go mod tidy"
go mod tidy
git diff --exit-code -- go.mod go.sum

echo "==> running Go unit tests"
go test ./...
