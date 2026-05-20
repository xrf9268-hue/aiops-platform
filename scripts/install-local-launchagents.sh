#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
launch_agents_dir="$HOME/Library/LaunchAgents"
logs_dir="$HOME/Library/Logs/aiops-platform"
uid="$(id -u)"
follow_auto_merge="${AIOPS_AUTO_MERGE:-0}"
follow_review_timeout="${AIOPS_REVIEW_TIMEOUT:-8m}"

mkdir -p "$launch_agents_dir" "$logs_dir"

worker_plist="$launch_agents_dir/com.aiops-platform.github-worker.plist"
follow_plist="$launch_agents_dir/com.aiops-platform.github-pr-follow-through.plist"

cat > "$worker_plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "https://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.aiops-platform.github-worker</string>
  <key>ProgramArguments</key>
  <array>
    <string>${repo_root}/scripts/local-github-worker.sh</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>WorkingDirectory</key>
  <string>${repo_root}</string>
  <key>StandardOutPath</key>
  <string>${logs_dir}/github-worker.out.log</string>
  <key>StandardErrorPath</key>
  <string>${logs_dir}/github-worker.err.log</string>
</dict>
</plist>
PLIST

cat > "$follow_plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "https://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.aiops-platform.github-pr-follow-through</string>
  <key>ProgramArguments</key>
  <array>
    <string>${repo_root}/scripts/local-pr-follow-through.sh</string>
  </array>
  <key>StartInterval</key>
  <integer>600</integer>
  <key>RunAtLoad</key>
  <true/>
  <key>WorkingDirectory</key>
  <string>${repo_root}</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>AIOPS_AUTO_MERGE</key>
    <string>${follow_auto_merge}</string>
    <key>AIOPS_REVIEW_TIMEOUT</key>
    <string>${follow_review_timeout}</string>
  </dict>
  <key>StandardOutPath</key>
  <string>${logs_dir}/github-pr-follow-through.out.log</string>
  <key>StandardErrorPath</key>
  <string>${logs_dir}/github-pr-follow-through.err.log</string>
</dict>
</plist>
PLIST

chmod 644 "$worker_plist" "$follow_plist"

launchctl bootout "gui/${uid}" "$worker_plist" >/dev/null 2>&1 || true
launchctl bootout "gui/${uid}" "$follow_plist" >/dev/null 2>&1 || true
launchctl bootstrap "gui/${uid}" "$worker_plist"
launchctl bootstrap "gui/${uid}" "$follow_plist"
launchctl enable "gui/${uid}/com.aiops-platform.github-worker"
launchctl enable "gui/${uid}/com.aiops-platform.github-pr-follow-through"

echo "Installed LaunchAgents:"
echo "  $worker_plist"
echo "  $follow_plist"
echo "Logs:"
echo "  $logs_dir/github-worker.out.log"
echo "  $logs_dir/github-pr-follow-through.out.log"
