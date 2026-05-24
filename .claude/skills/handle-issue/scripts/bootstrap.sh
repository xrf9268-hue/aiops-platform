#!/usr/bin/env bash
# Bootstrap context for the handle-issue skill: refresh the SPEC upstream
# mirror, then print the target issue (full body) and current main HEAD.
# Fails loudly (set -euo pipefail) so the agent never proceeds on a stale
# mirror, a wrong-repo mirror, a bad argument, or a failed fetch.
#
# Usage: bootstrap.sh <numeric-issue-number>
set -euo pipefail

UPSTREAM_URL="https://github.com/openai/symphony.git"
MIRROR="/tmp/symphony-upstream"
REPO="xrf9268-hue/aiops-platform"

# Arg first: distinguish a bad/missing argument (exit 2) from later fetch
# failures (which exit non-zero via set -e). Require exactly one numeric arg
# so stray extra args (e.g. "123 456") are rejected, not silently dropped.
if [ "$#" -ne 1 ]; then
	echo "usage: bootstrap.sh <numeric-issue-number>" >&2
	exit 2
fi
issue="$1"
case "$issue" in
'' | *[!0-9]*)
	echo "usage: bootstrap.sh <numeric-issue-number>" >&2
	exit 2
	;;
esac

# 1. SPEC upstream mirror: only fast-forward when the existing clone really is
# the symphony mirror; otherwise (re)clone. `git pull --ff-only` / `git clone`
# failing aborts the script under set -e rather than leaving a stale mirror.
# Note: a non-zero from `git remote get-url` inside this if-condition is
# intentionally exempt from set -e (POSIX: conditions don't trigger it), so a
# missing/non-git MIRROR cleanly falls through to the (re)clone branch; the
# 2>/dev/null just hides the first-run "not a git repo" stderr. Keep both.
if [ -d "$MIRROR/.git" ] && [ "$(git -C "$MIRROR" remote get-url origin 2>/dev/null)" = "$UPSTREAM_URL" ]; then
	git -C "$MIRROR" pull --ff-only
else
	rm -rf "$MIRROR"
	git clone --depth 1 "$UPSTREAM_URL" "$MIRROR"
fi
echo "upstream mirror: $(git -C "$MIRROR" rev-parse --short HEAD) @ $UPSTREAM_URL"

# 2. Target issue, full body (no truncation), pinned to the repo.
echo "=== issue #$issue ==="
gh issue view "$issue" --repo "$REPO" --json number,title,labels,body \
	--jq '"#\(.number) \(.title)\nlabels: \(.labels | map(.name) | join(", "))\n\n\(.body)"'

# 3. Current main HEAD. A failed fetch aborts here (no pipe masks its status).
echo "=== main HEAD ==="
git fetch origin main
git --no-pager log --oneline origin/main -1
