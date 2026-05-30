#!/usr/bin/env bash
# Build the aiops-platform binaries (worker, tui) from source with the
# same flags CI uses, and optionally install them onto a PATH prefix.
# This is the binary (non-Docker) deployment helper documented in
# docs/runbooks/binary-deployment.md.
#
# Usage:
#   scripts/install.sh                      # build into ./dist
#   scripts/install.sh --prefix /usr/local  # build, then install to <prefix>/bin
#   scripts/install.sh --prefix ~/.local    # user-local install
#
# Run the --prefix form with sufficient privileges for the target
# (e.g. sudo for /usr/local).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dist_dir="${repo_root}/dist"
prefix=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --prefix)
      [ "$#" -ge 2 ] || { echo "error: --prefix requires a path" >&2; exit 2; }
      prefix="$2"
      shift 2
      ;;
    --prefix=*)
      prefix="${1#--prefix=}"
      shift
      ;;
    -h|--help)
      sed -n '2,13p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "error: unknown argument: $1" >&2
      exit 2
      ;;
  esac
done

command -v go >/dev/null 2>&1 || { echo "error: go toolchain not found on PATH (need Go 1.25 per go.mod)" >&2; exit 1; }

binaries=(worker tui)
mkdir -p "$dist_dir"
for binary in "${binaries[@]}"; do
  echo "building ${binary} -> dist/${binary}"
  ( cd "$repo_root" && go build -trimpath -ldflags="-s -w" -o "dist/${binary}" "./cmd/${binary}" )
done

if [ -n "$prefix" ]; then
  bin_dir="${prefix%/}/bin"
  install -d "$bin_dir"
  for binary in "${binaries[@]}"; do
    install -m 0755 "${dist_dir}/${binary}" "${bin_dir}/${binary}"
    echo "installed ${bin_dir}/${binary}"
  done
else
  echo "built into ${dist_dir} (pass --prefix <dir> to install onto PATH)"
fi
