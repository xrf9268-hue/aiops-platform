#!/usr/bin/env bash
##
# SessionStart Hook for aiops-platform
#
# Runs automatically when a Claude Code session starts. In the remote/cloud/web
# container environment it installs the GitHub CLI (gh), which the project
# prefers over the GitHub MCP server for GitHub interactions (see AGENTS.md).
#
# Trigger: Only on 'startup' events (matcher configured in .claude/settings.json).
#
# Environment Detection:
# - CLAUDE_CODE_REMOTE=true: web/cloud/mobile container -> auto-install gh
# - CLAUDE_CODE_REMOTE=false or unset: local desktop -> skip auto-install
##

set -euo pipefail

echo "[SessionStart] Initializing environment..." >&2

# Detect the Claude Code remote/cloud/web container environment.
is_remote_environment() {
  [[ "${CLAUDE_CODE_REMOTE:-false}" == "true" ]]
}

# Only auto-install in the remote container; never touch a local desktop machine.
if ! is_remote_environment; then
  echo "[SessionStart] Local environment detected, skipping gh auto-installation" >&2
  echo "[SessionStart] To install gh manually: https://github.com/cli/cli#installation" >&2
  exit 0
fi

echo "[SessionStart] Remote/cloud environment detected, ensuring gh is available..." >&2

# Ensure ~/.local/bin exists and is on PATH for this and subsequent commands.
mkdir -p "${HOME}/.local/bin"
export PATH="${HOME}/.local/bin:${PATH}"

# Persist PATH for subsequent commands in the session via CLAUDE_ENV_FILE.
if [[ -n "${CLAUDE_ENV_FILE:-}" ]]; then
  echo "export PATH=\"\${HOME}/.local/bin:\${PATH}\"" >> "${CLAUDE_ENV_FILE}"
  echo "[SessionStart] PATH persisted to CLAUDE_ENV_FILE" >&2
fi

# Install GitHub CLI (gh) - official precompiled binary.
# Ref: https://github.com/cli/cli/blob/trunk/docs/install_linux.md
# Using the official binary release (not a distro package, which can lag behind).
install_gh() {
  local target="${HOME}/.local/bin/gh"

  # Skip if already installed.
  if command -v gh > /dev/null 2>&1; then
    echo "[SessionStart] gh already installed ($(gh --version 2>&1 | head -1 || echo 'unknown'))" >&2
    return 0
  fi

  # Resolve the latest release tag from the GitHub API.
  local gh_version
  gh_version="$(curl -fsSL https://api.github.com/repos/cli/cli/releases/latest \
    | grep -o '"tag_name":\s*"v[^"]*"' | head -1 | grep -o 'v[^"]*')"

  if [[ -z "${gh_version}" ]]; then
    echo "[SessionStart] Failed to determine latest gh version" >&2
    return 1
  fi

  # Strip the leading 'v' for the download URL path.
  local ver="${gh_version#v}"

  # Map host architecture to gh's release naming.
  local arch
  case "$(uname -m)" in
    x86_64) arch="amd64" ;;
    aarch64 | arm64) arch="arm64" ;;
    armv6*) arch="armv6" ;;
    i386 | i686) arch="386" ;;
    *)
      echo "[SessionStart] Unsupported architecture: $(uname -m)" >&2
      return 1
      ;;
  esac

  echo "[SessionStart] Installing gh ${gh_version} (latest, linux/${arch})..." >&2

  local tmp_dir="/tmp/gh-$$"
  mkdir -p "${tmp_dir}"

  if curl -fsSL -o "${tmp_dir}/gh.tar.gz" \
    "https://github.com/cli/cli/releases/download/${gh_version}/gh_${ver}_linux_${arch}.tar.gz"; then
    tar -xzf "${tmp_dir}/gh.tar.gz" -C "${tmp_dir}"
    mv "${tmp_dir}/gh_${ver}_linux_${arch}/bin/gh" "${target}"
    chmod +x "${target}"
    rm -rf "${tmp_dir}"
    echo "[SessionStart] gh ${gh_version} installed successfully" >&2

    # Report auth status; gh picks up GH_TOKEN / GITHUB_TOKEN from the environment.
    if gh auth status > /dev/null 2>&1; then
      echo "[SessionStart] gh already authenticated" >&2
    elif [[ -n "${GH_TOKEN:-}" ]] || [[ -n "${GITHUB_TOKEN:-}" ]]; then
      echo "[SessionStart] gh authenticated via environment token" >&2
    else
      echo "[SessionStart] gh installed but no token found for authentication" >&2
    fi
    return 0
  else
    echo "[SessionStart] Failed to install gh" >&2
    rm -rf "${tmp_dir}"
    return 1
  fi
}

install_gh

# Verify installation.
echo "[SessionStart] Tooling ready:" >&2
command -v gh > /dev/null 2>&1 && echo "  ✓ gh $(gh --version 2>&1 | head -1)" >&2

echo "[SessionStart] Environment initialized successfully" >&2
