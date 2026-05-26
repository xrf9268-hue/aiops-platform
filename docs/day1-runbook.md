# Day 1 Runbook: Gitea Tracker Polling

This runbook proves the current vertical slice of the AI coding platform:

```text
Gitea issue with an active aiops/* label
  -> worker poll tick
  -> orchestrator in-memory dispatch
  -> workspace prepare
  -> configured agent run
  -> verification and policy gates
```

The agent, not the worker, is responsible for branch pushes, PR creation, and
tracker writes through the tool surface advertised by the workflow.

## 1. Configure Environment

Copy `.env.example` to `.env` and set:

```bash
GITEA_BASE_URL=http://your-gitea-host
GITEA_TOKEN=<token for an aiops bot with repo access>
```

Do not commit real tokens.

## 2. Configure Workflow

Use a Gitea workflow such as `examples/gitea-WORKFLOW.md`. The important
tracker fields are:

```yaml
tracker:
  kind: gitea
  endpoint: http://your-gitea-host
  active_states:
    - AI Ready
    - Rework
  terminal_states:
    - Done
    - Canceled
```

Gitea issue state is encoded by `aiops/*` labels. For example, `aiops/todo`
maps to `AI Ready`.

## 3. Start Worker

From repository root:

```bash
docker compose --env-file .env -f deploy/docker-compose.yml up --build worker
```

Health check:

```bash
curl http://127.0.0.1:4000/livez
```

Expected:

```text
ok
```

## 4. Trigger A Task

Create an issue in the target Gitea repository, add the `aiops/todo` label, and
wait for the next worker poll tick.

## 5. Verify Runtime State

```bash
curl http://127.0.0.1:4000/api/v1/state
```

The state response should show the issue as a recent candidate or running task,
then eventually as completed or failed depending on the configured agent and
verification commands.

## 6. Verify Workspace Result

The worker creates deterministic workspaces under the configured
`workspace.root` (or `AIOPS_WORKSPACE_ROOT` fallback). Inspect the issue
workspace for `.aiops/RUN_SUMMARY.md`, `.aiops/VERIFICATION.txt`, and
`.aiops/CHANGED_FILES.txt`.

## 7. Current Day 1 Limitations

- Scheduler state is in memory by design; restart recovery comes from tracker
  polling plus deterministic workspace reconciliation.
- The `mock` agent is useful for loop validation, but real branch/PR behavior
  requires a configured coding agent workflow.
- Human review is still required unless the workflow and repository protection
  explicitly allow automated merge.
