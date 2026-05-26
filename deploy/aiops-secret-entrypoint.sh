#!/bin/sh
set -eu

if [ -s /run/secrets/linear_api_key ]; then
  export LINEAR_API_KEY="$(cat /run/secrets/linear_api_key)"
fi

if [ -s /run/secrets/gitea_token ]; then
  export GITEA_TOKEN="$(cat /run/secrets/gitea_token)"
fi

if [ -s /run/secrets/aiops_state_api_token ]; then
  export AIOPS_STATE_API_TOKEN="$(cat /run/secrets/aiops_state_api_token)"
fi

if [ -s /run/secrets/github_token ]; then
  home="${HOME:-/home/aiops}"
  mkdir -p "$home/.config/gh"
  chmod 0700 "$home/.config" "$home/.config/gh"
  {
    printf 'github.com:\n'
    printf '    git_protocol: https\n'
    printf '    oauth_token: %s\n' "$(cat /run/secrets/github_token)"
  } > "$home/.config/gh/hosts.yml"
  chmod 0600 "$home/.config/gh/hosts.yml"
  if command -v gh >/dev/null 2>&1; then
    gh auth setup-git -h github.com >/dev/null 2>&1 || true
  fi
fi

if [ "$#" -eq 0 ]; then
  set -- worker
elif [ "${1#-}" != "$1" ]; then
  set -- worker "$@"
fi

exec "$@"
