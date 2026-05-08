# Local development runbook

This is the first document to open after cloning. It walks through bootstrapping the full loop locally: Postgres, the trigger API, the worker, and the optional Linear poller, and ends with a smoke test that exercises the queue end-to-end.

The repository ships with a `deploy/docker-compose.yml` that runs the same components in containers. This runbook documents both the all-in-one Docker path and the run-from-source path that is more convenient when iterating on Go code.

## Prerequisites

- Go 1.25 or newer (matches `go.mod`).
- Docker and Docker Compose v2.
- `git` and `curl`.
- Optional: a Gitea instance and bot token if you want to exercise the webhook path.
- Optional: a Linear personal API key if you want to exercise the Linear poller path.

## 1. Configure environment

Copy the example file and adjust values as needed:

```bash
cp .env.example .env
```

The defaults assume Postgres listens on `localhost:5432` with user `aiops`, password `aiops`, database `aiops`. You only need to change the Gitea and Linear values when you actually use those integrations. Never commit a real `.env`.

## 2. Start Postgres and apply migrations

The compose file mounts `migrations/` into Postgres's `/docker-entrypoint-initdb.d` directory, so the schema is applied automatically on first startup.

Start Postgres only:

```bash
docker compose --env-file .env -f deploy/docker-compose.yml up -d postgres
```

Verify the schema:

```bash
docker compose --env-file .env -f deploy/docker-compose.yml \
  exec postgres psql -U aiops -d aiops -c '\dt'
```

You should see `tasks` and `task_events`.

If you need to reapply `migrations/001_init.sql` against an existing database, the SQL is written with `CREATE TABLE IF NOT EXISTS`, so it is safe to run again:

```bash
docker compose --env-file .env -f deploy/docker-compose.yml \
  exec -T postgres psql -U aiops -d aiops < migrations/001_init.sql
```

To wipe state during development:

```bash
docker compose --env-file .env -f deploy/docker-compose.yml down -v
```

## 3. Run the trigger API

Option A: from source.

```bash
export DATABASE_URL=postgres://aiops:aiops@localhost:5432/aiops?sslmode=disable
export GITEA_WEBHOOK_SECRET=dev-secret
export ADDR=:8080
go run ./cmd/trigger-api
```

Option B: in Docker.

```bash
docker compose --env-file .env -f deploy/docker-compose.yml up -d trigger-api
```

Health check:

```bash
curl -fsS http://localhost:8080/healthz
```

Expected response:

```json
{"ok":true}
```

## 4. Run the worker

The worker claims queued tasks, prepares a deterministic Git workspace, runs the configured runner (`mock`, `codex`, or `claude`), enforces policy, and opens a draft PR through the Gitea client.

Option A: from source.

```bash
export DATABASE_URL=postgres://aiops:aiops@localhost:5432/aiops?sslmode=disable
export WORKSPACE_ROOT=/tmp/aiops-workspaces
# Required: cmd/worker always calls the Gitea client at the end of every task.
# Both values must be non-empty or every task fails after the agent run with
# "GITEA_BASE_URL and GITEA_TOKEN are required". For local-only smoke testing
# without a real Gitea, any non-empty placeholder is enough to get past the
# precondition (the HTTP call will then fail, but that happens after the
# verification step you usually care about in a smoke test).
export GITEA_BASE_URL=http://gitea.local
export GITEA_TOKEN=replace-with-gitea-bot-token
go run ./cmd/worker
```

A minimal `.env` snippet that satisfies the worker's required vars:

```bash
DATABASE_URL=postgres://aiops:aiops@localhost:5432/aiops?sslmode=disable
WORKSPACE_ROOT=/tmp/aiops-workspaces
GITEA_BASE_URL=http://gitea.local
GITEA_TOKEN=placeholder-not-used-for-mock-only-smoke-test
```

Option B: in Docker.

```bash
docker compose --env-file .env -f deploy/docker-compose.yml up -d worker
```

For first-time local testing, keep `agent.default: mock` in `examples/WORKFLOW.md`. The mock runner produces a deterministic change without calling any external model.

## 5. Run the Linear poller (optional)

The poller reads `examples/WORKFLOW.md` for the repo, tracker, and poll interval, then enqueues a task per active Linear issue.

Option A: from source.

```bash
export DATABASE_URL=postgres://aiops:aiops@localhost:5432/aiops?sslmode=disable
export LINEAR_API_KEY=your-linear-personal-key
go run ./cmd/linear-poller examples/WORKFLOW.md
```

Option B: in Docker.

```bash
docker compose --env-file .env -f deploy/docker-compose.yml --profile linear up -d linear-poller
```

The poller exits immediately with `tracker.kind must be linear` if `examples/WORKFLOW.md` is not configured for Linear, and logs `skip <issue>: repo.clone_url missing in WORKFLOW.md` if `repo.clone_url` is empty.

## 6. Smoke test

The fastest way to verify the full local loop without a real Gitea or Linear is to enqueue a manual task with the helper script and inspect the resulting rows.

Enqueue a task. The `CLONE_URL` must be one the running worker can actually
clone, because `internal/workspace.PrepareGitWorkspace` runs `git clone $CLONE_URL`
before any agent runs. For a fully local smoke test we point at this checkout
through a `file://` URL so no network or remote Gitea is needed:

```bash
export AIOPS_API_URL=http://localhost:8080
export REPO_OWNER=local
export REPO_NAME=aiops-platform
export CLONE_URL="file://$(git rev-parse --show-toplevel)/.git"
export BASE_BRANCH=main
export TITLE="Local smoke task"
export MODEL=mock
scripts/enqueue-manual-task.sh
```

If you only want to verify that the trigger API enqueues the row and do not
plan to start the worker, any value in `CLONE_URL` works (the worker is what
actually performs the clone). To fully exercise the worker offline without
starting the API, run `scripts/test-enqueue-manual-task.sh` instead, which
stubs `curl`.

The script prints the assigned `task_id` and a follow-up `psql` command. Inspect the task and its events through the trigger API:

```bash
curl 'http://localhost:8080/v1/tasks?status=queued'
curl 'http://localhost:8080/v1/tasks/<task_id>'
curl 'http://localhost:8080/v1/tasks/<task_id>/events'
```

See [Task debugging API](task-api.md) for the full reference.

You can also inspect the queue directly:

```bash
docker compose --env-file .env -f deploy/docker-compose.yml exec postgres \
  psql -U aiops -d aiops -c \
  "select id,status,repo_owner,repo_name,work_branch,updated_at from tasks order by created_at desc limit 5;"
```

There is also an offline smoke test that exercises the script with a fake `curl`, useful in CI or when you do not want to start the API:

```bash
scripts/test-enqueue-manual-task.sh
```

It prints `PASS: enqueue-manual-task smoke test` on success.

## Common failure modes

### `connection refused` against Postgres

The trigger API or worker logs `dial tcp 127.0.0.1:5432: connect: connection refused`.

- Check `docker compose ps` and ensure the `postgres` service is healthy.
- Confirm `DATABASE_URL` matches the host you are running from. Inside compose use `postgres:5432`, from your host use `localhost:5432`.

### `tasks` table does not exist

The trigger API returns 500 and the logs mention `relation "tasks" does not exist`.

- This means the init script in `migrations/001_init.sql` did not run. It only runs on the very first start of the Postgres volume. Run `docker compose down -v` to drop the volume and start again, or apply the SQL manually as shown in step 2.

### Manual enqueue returns `409` or `deduped: true`

Each task is unique on `(source_type, source_event_id)`. Either pass a fresh `SOURCE_EVENT_ID` to the script or omit it so the script generates one.

### Worker logs `bad signature` for Gitea webhooks

`GITEA_WEBHOOK_SECRET` does not match the secret configured in Gitea. Both the trigger API and the Gitea webhook configuration must use the same value.

### Linear poller exits with `tracker.kind must be linear`

`examples/WORKFLOW.md` is not configured for Linear. Set `tracker.kind: linear` and provide an `api_key` (the value can reference `$LINEAR_API_KEY`).

### Worker fails to push or open PRs

- `GITEA_BASE_URL` and `GITEA_TOKEN` must be set and the bot user must have write access to the target repository.
- The worker bind-mounts `~/.ssh` read-only into the container. SSH clone URLs require a working key on the host, with the Gitea host already in `~/.ssh/known_hosts`.

### Port `5432` or `8080` already in use

Stop the conflicting local service or change the published port in `deploy/docker-compose.yml` and `ADDR` / `DATABASE_URL` accordingly.

### `go: module lookup disabled` or `go.sum` mismatch

Run `go mod tidy`, then re-run the failing command. CI will reject changes that leave `go.mod` or `go.sum` dirty; see [CI/CD runbook](ci.md) for the local pre-push check list.

## Workspace cache and cleanup

After M2, the worker keeps a per-repo bare mirror under
`AIOPS_MIRROR_ROOT` (default `os.UserCacheDir()/aiops-platform/mirrors`)
and creates a per-task worktree under `WORKSPACE_ROOT` for every claimed
task. This avoids re-cloning on every retry and lets two tasks run
concurrently without sharing a working tree. See the dedicated
[workspace cache runbook](workspace-cache.md) for the on-disk layout,
configuration knobs, and recommended cleanup cadence (the
`(*workspace.Manager).Cleanup` API or `rm -rf $WORKSPACE_ROOT/*` once
old tasks no longer matter).
