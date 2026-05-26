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

if [ "$#" -eq 0 ]; then
  set -- worker
elif [ "${1#-}" != "$1" ]; then
  set -- worker "$@"
fi

exec "$@"
