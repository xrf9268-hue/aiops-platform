#!/usr/bin/env bash
#
# Regenerate the vendored Codex app-server protocol schema after a Codex CLI
# upgrade. Run this instead of hand-editing the vendored bundle — that is the
# hand-curation that rotted the old 6-line snapshot (#446) into a placebo.
#
# The schema MUST be generated with --experimental: the runner enables the
# experimental API (initialize capabilities.experimentalApi) and sends
# experimental request fields such as thread/start dynamicTools, which the
# default schema export strips. See internal/runner/codex_version.go.
#
# It regenerates the schema and prints the exact pin edits to make; it does not
# edit source files itself (the per-arch sha256 and Go/Docker edits stay under
# human review). After editing the pins, the contract + parity tests in
# internal/runner must go green.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v codex >/dev/null 2>&1 || { echo "codex not found on PATH" >&2; exit 1; }

ver="$(codex --version | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
[ -n "$ver" ] || { echo "could not parse 'codex --version'" >&2; exit 1; }
major="$(echo "$ver" | cut -d. -f1)"
minor="$(echo "$ver" | cut -d. -f2)"
patch="$(echo "$ver" | cut -d. -f3)"
# Include the patch in the stamp: codex generates the schema per exact version,
# so a patch bump must produce a new filename (the parity test asserts the full
# v<major>_<minor>_<patch> stamp), forcing a regenerate rather than silently
# reusing a stale patch schema (#629 P2).
schema_file="codex_app_server_protocol_v${major}_${minor}_${patch}.v2.schemas.json"
dest="internal/runner/testdata/${schema_file}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
codex app-server generate-json-schema --out "$tmp" --experimental >/dev/null
cp "$tmp/codex_app_server_protocol.v2.schemas.json" "$dest"
echo "wrote ${dest} (codex ${ver})"

# Drop any stale vendored bundle from a previous codex version.
for f in internal/runner/testdata/codex_app_server_protocol_v*.v2.schemas.json; do
  [ "$f" = "$dest" ] && continue
  echo "removing stale ${f}"
  git rm -q "$f" 2>/dev/null || rm -f "$f"
done

echo
echo "Fetching release sha256 for Dockerfile (codex ${ver})…"
for arch in x86_64-unknown-linux-musl aarch64-unknown-linux-musl; do
  url="https://github.com/openai/codex/releases/download/rust-v${ver}/codex-package-${arch}.tar.gz"
  if sha="$(curl -fsSL "$url" | shasum -a 256 | awk '{print $1}')" && [ -n "$sha" ]; then
    echo "  ${arch}: ${sha}"
  else
    echo "  ${arch}: FETCH FAILED (${url})"
  fi
done

cat <<EOF

Update these pins to ${ver}, then commit the regenerated schema with them:
  - internal/runner/codex_version.go
      CodexProtocolVersion    = "${ver}"
      codexProtocolSchemaFile = "${schema_file}"
  - Dockerfile: ARG CODEX_CLI_VERSION=${ver}  (+ the two codex_sha values above)
  - test/e2e/fixtures/dockerfile_buildlayer_test.go: the CODEX_CLI_VERSION assertion
  - docs/runbooks/codex-app-server-docker.md: version references

Then verify (both must pass):
  go test ./internal/runner -run 'CodexAppServer.*Schema|CodexVersionPinParity' -count=1
EOF
