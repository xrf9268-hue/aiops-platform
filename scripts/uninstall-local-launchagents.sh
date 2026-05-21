#!/usr/bin/env bash
set -euo pipefail

uid="$(id -u)"
launch_agents_dir="$HOME/Library/LaunchAgents"

for label in \
  com.aiops-platform.github-worker \
  com.aiops-platform.github-pr-follow-through
do
  plist="$launch_agents_dir/${label}.plist"
  launchctl bootout "gui/${uid}" "$plist" >/dev/null 2>&1 || true
  rm -f "$plist"
done

echo "Uninstalled aiops-platform local LaunchAgents"
