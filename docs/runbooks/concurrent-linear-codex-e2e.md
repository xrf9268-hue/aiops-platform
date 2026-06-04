# Concurrent Linear/Codex local binary E2E

Use this runbook after the single-issue smoke in
[`first-run-docker-linear-codex.md`](first-run-docker-linear-codex.md) proves
the install can run one disposable task. This flow validates the real local
binary path with two concurrent Codex agents, agent-owned Linear lifecycle
writes, dashboard state, and TUI rendering.

The worker is still only the scheduler/runner and tracker reader. Linear state
updates must be performed by the agent through the advertised `linear_graphql`
tool.

## Contract

The Linear project workflow must expose these visible states:

```text
Backlog, Todo, In Progress, In Review, Done
```

The active handoff path under test is:

```text
Todo/In Progress active work -> In Review
```

Backlog and Done are schema checks, not required agent transitions. Create the
disposable issues in Todo, require the agent to comment when work starts, and
require the agent to move each issue to In Review after it opens the draft PR
and records completion evidence. Do not require an already-active current issue
to be moved back into In Progress; D35 rejects current-issue active-target
`issueUpdate` writes so an operator terminal stop cannot be reverted by the
running agent. Do not make the worker, a wrapper script, or an operator cleanup
step move issues to Done.

## 1. Prepare disposable work

Create two disposable Linear issues in Todo in an isolated disposable project.
Do not run this smoke against a shared Linear project: the worker polls every
issue in `tracker.active_states`, so unrelated Todo or In Progress issues can
dispatch real agents and can make `running: 2` pass for the wrong work. Before
starting the worker, verify that exactly two active issues exist in the project,
and that their identifiers are the two disposable issue ids for this run. Use
the issue body as the task source of truth. The bodies can point at a disposable
fixture repo, a temporary GitHub issue, or a tiny docs-only task, but they must
not depend on copied task text in `WORKFLOW.md`.

Before starting the worker, verify the project schema independently in Linear
UI or through the API. The GraphQL query should inspect the project's team
workflow states:

```graphql
query ProjectWorkflowStates($projectSlug: String!) {
  projects(filter: { slugId: { eq: $projectSlug } }, first: 1) {
    nodes {
      name
      slugId
      teams {
        nodes {
          states {
            nodes { name type }
          }
        }
      }
    }
  }
}
```

Record sanitized evidence that the visible state names include Backlog, Todo,
In Progress, In Review, Done. Do not store API keys in the evidence file.

## 2. Render a generic workflow

Use a fresh `WORKFLOW.md` for the run. Keep the prompt generic and issue-body
driven so the validation cannot inherit stale issue numbers or old task text;
do not copy issue-specific task text into WORKFLOW.md.

```yaml
---
repo:
  owner: OWNER
  name: REPO
  clone_url: https://github.com/OWNER/REPO.git
  default_branch: main
agent:
  default: codex-app-server
  max_concurrent_agents: 2
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: PROJECT-SLUG
  active_states:
    - Todo
    - In Progress
  terminal_states:
    - In Review
    - Done
    - Canceled
    - Cancelled
    - Duplicate
codex:
  linear_graphql:
    allow_mutations: true
    allowed_mutations:
      - issueUpdate
      - commentCreate
---
Use the issue body as the task source of truth.
Do not copy issue-specific task text into WORKFLOW.md.

On start:
- Read the Linear issue body and comments.
- Use linear_graphql commentCreate to record that work started.
- Do not move an already-active current issue back into In Progress.

While working:
- Implement only the task described in this issue body.
- Open a draft PR when code or docs change.
- Do not use shell curl for Linear writes.

On finish:
- Record validation evidence in the draft PR body or final issue comment.
- Use linear_graphql commentCreate to summarize the result.
- Use linear_graphql issueUpdate to move this issue to In Review.
- Do not move the issue to Done.
```

Keep `tracker.active_states` limited to Todo and In Progress for this run. That
lets the worker discover new Todo issues and continue already-started In
Progress issues, while In Review is the agent-owned handoff state. The start
comment is the start signal; the only required state write in this smoke is the
final non-active In Review handoff.

## 3. Protect credentials

Keep credentials in environment variables, restricted files, a keychain helper,
or the existing launchd secret wrapper. Never pass Linear tokens as command-line arguments.
`LINEAR_API_KEY` may be present in the worker environment or loaded from a
restricted file, but it must not appear in repo files, launchd plist files,
shell history, worker logs, dashboard captures, or TUI captures.

For a file-backed run, export the two tokens into the current operator shell
from restricted files before running `--doctor`, rendering curl config, or
capturing the TUI. The worker and TUI read the environment, while the command
line only carries file paths:

```bash
: "${LINEAR_API_KEY_FILE:?set LINEAR_API_KEY_FILE to a restricted token file}"
: "${AIOPS_STATE_API_TOKEN_FILE:?set AIOPS_STATE_API_TOKEN_FILE to a restricted token file}"
export LINEAR_API_KEY="$(cat "$LINEAR_API_KEY_FILE")"
export AIOPS_STATE_API_TOKEN="$(cat "$AIOPS_STATE_API_TOKEN_FILE")"
```

Use the state API token only through headers or `AIOPS_STATE_API_TOKEN`:

```bash
mkdir -p /tmp/aiops-linear-e2e
curl_cfg="/tmp/aiops-linear-e2e/curl.cfg"
rm -f "$curl_cfg"
(umask 077 && : > "$curl_cfg")
chmod 600 "$curl_cfg"
printf 'header = "Authorization: Bearer %s"\n' \
  "$AIOPS_STATE_API_TOKEN" > "$curl_cfg"
```

After the run and closeout cleanup, scan the evidence paths for accidental token
exposure before sharing or committing any report:

```bash
if [ -n "${LINEAR_API_KEY:-}" ] || [ -n "${AIOPS_STATE_API_TOKEN:-}" ]; then
  {
    [ -n "${LINEAR_API_KEY:-}" ] && printf '%s\n' "$LINEAR_API_KEY"
    [ -n "${AIOPS_STATE_API_TOKEN:-}" ] && printf '%s\n' "$AIOPS_STATE_API_TOKEN"
  } | rg -n --fixed-strings -f - \
    docs/validation/smoke /tmp/aiops-linear-e2e || true
fi
```

The scan command is for a local operator shell only; do not paste secrets into
issue comments, PR bodies, or runbook output.

## 4. Preflight and start the worker

Build the local binary and run the same preflight checks used by the first-run
guide:

```bash
go build -o /tmp/aiops-worker ./cmd/worker
/tmp/aiops-worker --doctor --mode=real --deploy=binary WORKFLOW.md
/tmp/aiops-worker --print-config "$(pwd)" >/tmp/aiops-linear-e2e-config.txt
```

Start a dedicated per-user launchd worker for this smoke. Do not reuse an
already-loaded `com.aiops-platform.worker` job, because `launchctl kickstart`
only restarts the loaded program and environment; it does not repoint the job at
the local binary or workflow rendered for this run.

```bash
workflow_path="$(pwd)/WORKFLOW.md"
wrapper_path="/tmp/aiops-linear-e2e/worker-launch"
plist_path="$HOME/Library/LaunchAgents/com.aiops-platform.linear-e2e.plist"
label="com.aiops-platform.linear-e2e"
worker_port="${AIOPS_LINEAR_E2E_PORT:-4015}"
worker_url="http://127.0.0.1:${worker_port}"

: "${LINEAR_API_KEY_FILE:?set LINEAR_API_KEY_FILE to a restricted token file}"
: "${AIOPS_STATE_API_TOKEN_FILE:?set AIOPS_STATE_API_TOKEN_FILE to a restricted token file}"

launchctl bootout "gui/$(id -u)/$label" >/dev/null 2>&1 || true
launchctl bootout "gui/$(id -u)" "$plist_path" >/dev/null 2>&1 || true
sleep 1

if lsof -nP -iTCP:"$worker_port" -sTCP:LISTEN >/dev/null 2>&1; then
  echo "FAIL port $worker_port already has a listener; set AIOPS_LINEAR_E2E_PORT to an unused port" >&2
  exit 1
fi

cat > "$wrapper_path" <<EOF
#!/usr/bin/env bash
set -euo pipefail
export LINEAR_API_KEY="\$(cat "$LINEAR_API_KEY_FILE")"
export AIOPS_STATE_API_TOKEN="\$(cat "$AIOPS_STATE_API_TOKEN_FILE")"
exec /tmp/aiops-worker --port="$worker_port" "$workflow_path"
EOF
chmod 700 "$wrapper_path"

cat > "$plist_path" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "https://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key><array><string>$wrapper_path</string></array>
  <key>WorkingDirectory</key><string>$(pwd)</string>
  <key>RunAtLoad</key><true/>
  <key>StandardOutPath</key><string>/tmp/aiops-linear-e2e/worker.out.log</string>
  <key>StandardErrorPath</key><string>/tmp/aiops-linear-e2e/worker.err.log</string>
</dict></plist>
EOF
chmod 600 "$plist_path"

launchctl bootstrap "gui/$(id -u)" "$plist_path"
launchctl kickstart -k "gui/$(id -u)/$label"
launchctl print "gui/$(id -u)/$label" >/tmp/aiops-linear-e2e/launchd.txt
grep -F "$wrapper_path" /tmp/aiops-linear-e2e/launchd.txt
grep -F "$workflow_path" "$wrapper_path"

worker_ready=false
for attempt in $(seq 1 60); do
  if curl --fail --silent --show-error "$worker_url/livez" >/dev/null 2>&1 &&
    curl --fail --silent --show-error "$worker_url/readyz" >/dev/null 2>&1; then
    worker_ready=true
    break
  fi
  sleep 2
done
if [ "$worker_ready" != true ]; then
  echo "FAIL worker did not become ready on $worker_url" >&2
  tail -50 /tmp/aiops-linear-e2e/worker.err.log >&2 || true
  exit 1
fi
```

If the launchd job uses a secret wrapper, keep `LINEAR_API_KEY` and
`AIOPS_STATE_API_TOKEN` out of the plist. The plist may point at a restricted
wrapper path, but it must not inline secret values.

## 5. Trigger and assert concurrency

Trigger a poll after both disposable issues are in Todo:

```bash
curl --fail --silent --show-error --config "$curl_cfg" \
  -H 'X-AIOPS-Refresh: true' \
  -X POST "$worker_url/api/v1/refresh"
```

Poll `/api/v1/state` until it shows `running: 2` or an equivalent JSON count
with two running rows from the two disposable issue ids:

```bash
saw_running_2=false
for attempt in $(seq 1 60); do
  curl --fail --silent --show-error --config "$curl_cfg" \
    "$worker_url/api/v1/state" \
    >/tmp/aiops-linear-e2e/state.json
  jq '.counts.running // (.running | length)' \
    /tmp/aiops-linear-e2e/state.json
  if jq -e '(.counts.running == 2) or ((.running | length) == 2)' \
    /tmp/aiops-linear-e2e/state.json; then
    saw_running_2=true
    break
  fi
  sleep 5
done
if [ "$saw_running_2" != true ]; then
  echo "FAIL did not observe running: 2" >&2
  exit 1
fi
```

Keep the state snapshot as evidence. If the count never reaches two, record the
worker log path and stop; do not reinterpret a serialized run as passing the
concurrency requirement.

## 6. Verify Linear lifecycle

For each disposable issue, verify the Linear activity or API history shows the
agent-owned start comment and final In Review handoff. The In Review transition
must occur after the completion comment and draft PR link.

Required evidence for each issue:

- the issue started in Todo;
- the agent posted a start comment;
- the agent opened a draft PR for the issue body task;
- the agent posted completion evidence;
- the agent moved it to In Review through `linear_graphql`;
- the worker did not move it to Done.

Use independent Linear UI/API inspection for this step. Worker logs alone prove
tool calls were advertised, not that Linear accepted the lifecycle changes.

## 7. Verify dashboard and TUI

Capture the dashboard state API and one raw TUI frame after the two runs finish:

```bash
curl --fail --silent --show-error --config "$curl_cfg" \
  "$worker_url/api/v1/state" \
  >/tmp/aiops-linear-e2e/final-state.json

timeout_bin="${AIOPS_TIMEOUT_BIN:-$(command -v timeout || command -v gtimeout || true)}"
if [ -z "$timeout_bin" ]; then
  echo "FAIL install GNU coreutils or set AIOPS_TIMEOUT_BIN to timeout/gtimeout" >&2
  exit 1
fi

"$timeout_bin" 15s env AIOPS_STATE_API_TOKEN="$AIOPS_STATE_API_TOKEN" \
  go run ./cmd/tui --url "$worker_url/" --raw \
  >/tmp/aiops-linear-e2e/tui-raw.txt \
  || test "$?" -eq 124
```

The final state must be nonblank and error-free. The TUI capture must include
the run rows or terminal handoff signal for the disposable issues; a connection
error, authentication error, or empty frame fails the E2E.

## 8. Closeout

Write a sanitized report under `docs/validation/smoke/` or another local
operator evidence directory. Include:

- Linear project name and sanitized issue identifiers;
- confirmation that the schema contained Backlog, Todo, In Progress, In Review,
  Done;
- `WORKFLOW.md` path and `max_concurrent_agents: 2`;
- `/api/v1/state` evidence showing `running: 2`;
- lifecycle evidence for both issues;
- draft PR URLs;
- dashboard and `cmd/tui --raw` evidence paths;
- secret scan command and result.

Dispose of the draft PRs and test issues according to the normal PR
follow-through gate. Only move issues to Done as an explicit human cleanup
action after validation is complete.

Stop and remove the temporary LaunchAgent even when the smoke fails, so it
cannot keep polling the disposable project or hold the E2E port:

```bash
label="com.aiops-platform.linear-e2e"
plist_path="$HOME/Library/LaunchAgents/com.aiops-platform.linear-e2e.plist"
wrapper_path="/tmp/aiops-linear-e2e/worker-launch"
curl_cfg="/tmp/aiops-linear-e2e/curl.cfg"

launchctl bootout "gui/$(id -u)" "$plist_path" >/dev/null 2>&1 || true
rm -f "$plist_path" "$wrapper_path" "$curl_cfg"
```
