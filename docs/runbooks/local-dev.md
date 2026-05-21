# Local development runbook

This is the first document to open after cloning. It walks through bootstrapping the local loop: the worker, optional transitional pollers, and smoke checks for tracker polling.

The repository ships with a `deploy/docker-compose.yml` that runs the same components in containers. This runbook documents both the all-in-one Docker path and the run-from-source path that is more convenient when iterating on Go code.

## Prerequisites

- Go 1.25 or newer (matches `go.mod`).
- Docker and Docker Compose v2.
- `git` and `curl`.
- Optional: a Gitea instance and bot token if you want to exercise the Gitea poller path.
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

## 3. Run the worker

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

## 4. Run a tracker poller (optional)

The Linear and Gitea pollers read `examples/WORKFLOW.md` for the repo, tracker, and poll interval, then enqueue a task per active issue.

### Linear

The Linear poller enqueues issues in configured active Linear workflow states.
For Linear workflows, `tracker.project_slug` in the selected `WORKFLOW.md` maps
to Linear's project `slugId` and is required unless `services[]` routes define
per-service `tracker.project_slug` values. The worker fails workflow loading
before the first poll when the applicable project slug is missing.

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

### Gitea

The Gitea poller treats Gitea as a tracker reader: issues are selected by `aiops/*` state labels, and label writes happen through the agent tool surface rather than the poller. The default state labels are:

| Workflow state | Gitea label |
| --- | --- |
| `AI Ready` | `aiops/todo` |
| `In Progress` | `aiops/in-progress` |
| `Rework` | `aiops/rework` |
| `Done` | `aiops/done` |
| `Canceled` | `aiops/canceled` |

The worker-owned `tracker.kind: gitea` path uses the same label mapping for
per-tick reconciliation. After a run starts, moving the issue to `aiops/done`
or `aiops/canceled` makes the next poll refresh that issue by ID and cancel the
active worker.

Option A: from source.

```bash
export DATABASE_URL=postgres://aiops:aiops@localhost:5432/aiops?sslmode=disable
export GITEA_BASE_URL=http://localhost:3000
export GITEA_TOKEN=your-gitea-bot-token
go run ./cmd/gitea-poller examples/gitea-WORKFLOW.md
```

The poller exits immediately with `tracker.kind must be gitea` if its workflow is not configured for Gitea. It logs `skip <issue>: repo.clone_url missing in WORKFLOW.md` if `repo.clone_url` is empty.

Gitea issue listing is capped at 20 pages of 50 issues per state label (1000 issues). When the `+1` probe page proves more issues exist, the poller keeps running with the capped result set and increments `aiops_gitea_issue_pagination_cap_hits_total`. Expose that counter by setting `GITEA_POLLER_METRICS_ADDR` before starting the poller:

```bash
export GITEA_POLLER_METRICS_ADDR=:9091
go run ./cmd/gitea-poller examples/gitea-WORKFLOW.md
curl http://localhost:9091/metrics | grep aiops_gitea_issue_pagination_cap_hits_total
```

The metric is cumulative for the poller process. A non-zero value means at least one poll since startup observed an active state label with more than 1000 matching Gitea issues; alert on recent increases, not just the absolute value. When it increases, operators should reduce the active backlog or split labels before relying on the poller for exhaustive dispatch.

## 5. Smoke test

The fastest way to verify local configuration without a real Gitea or Linear is
to load the workflow and print the effective worker config:

```bash
go run ./cmd/worker --print-config .
```

To exercise dispatch, configure `examples/WORKFLOW.md` for a real Linear or
Gitea tracker, keep `agent.default: mock`, move one issue into an active state,
and run the worker plus the matching poller. The poller discovers the active
tracker issue; there is no webhook or manual enqueue endpoint in the normal
loop.

You can also inspect the queue directly:

```bash
docker compose --env-file .env -f deploy/docker-compose.yml exec postgres \
  psql -U aiops -d aiops -c \
  "select id,status,repo_owner,repo_name,work_branch,updated_at from tasks order by created_at desc limit 5;"
```

## Common failure modes

### `connection refused` against Postgres

A transitional poller logs `dial tcp 127.0.0.1:5432: connect: connection refused`.

- Check `docker compose ps` and ensure the `postgres` service is healthy.
- Confirm `DATABASE_URL` matches the host you are running from. Inside compose use `postgres:5432`, from your host use `localhost:5432`.

### `tasks` table does not exist

A transitional poller logs `relation "tasks" does not exist`.

- This means the init script in `migrations/001_init.sql` did not run. It only runs on the very first start of the Postgres volume. Run `docker compose down -v` to drop the volume and start again, or apply the SQL manually as shown in step 2.

### Linear poller exits with `tracker.kind must be linear`

`examples/WORKFLOW.md` is not configured for Linear. Set `tracker.kind: linear`, provide an `api_key` (the value can reference `$LINEAR_API_KEY`), and set `tracker.project_slug` to the Linear project slug used for SPEC §11.2 candidate filtering.

### Worker fails to push or open PRs

- `GITEA_BASE_URL` and `GITEA_TOKEN` must be set and the bot user must have write access to the target repository.
- The worker bind-mounts `~/.ssh` read-only into the container. SSH clone URLs require a working key on the host, with the Gitea host already in `~/.ssh/known_hosts`.

### Port `5432` already in use

Stop the conflicting local service or change the published port in `deploy/docker-compose.yml` and `DATABASE_URL` accordingly.

### `go: module lookup disabled` or `go.sum` mismatch

Run `go mod tidy`, then re-run the failing command. CI will reject changes that leave `go.mod` or `go.sum` dirty; see [CI/CD runbook](ci.md) for the local pre-push check list.

## Workspace cache and cleanup

After M2, the worker keeps a per-repo bare mirror under
`AIOPS_MIRROR_ROOT` (default `os.UserCacheDir()/aiops-platform/mirrors`)
and creates a per-task worktree under the selected workflow's `workspace.root`
for every claimed task. If `workspace.root` is omitted from `WORKFLOW.md`, the
worker falls back to `WORKSPACE_ROOT`. This avoids re-cloning on every retry and
lets two tasks run concurrently without sharing a working tree. See the dedicated
[workspace cache runbook](workspace-cache.md) for the on-disk layout,
configuration knobs, and recommended cleanup cadence (the
`(*workspace.Manager).Cleanup` API or removal of the effective workspace root
once old tasks no longer matter).

## Running e2e tests locally

The e2e suite under `test/e2e/` validates the Gitea poller loop against real
Postgres and Gitea containers. It is gated by the `e2e` build tag and does not
run as part of `go test ./...`.

Requirements: a working Docker daemon. Cold first run pulls ~600MB of
images and takes 2–3 minutes. Warm runs take ~10 seconds for all four tests.

```bash
go test -tags e2e -race -timeout 15m ./test/e2e/...
```

Common failure modes:

- `Cannot connect to the Docker daemon` — start Docker Desktop or `colima`.
- `go test` reports `build constraints exclude all Go files` — the `-tags
  e2e` flag is missing.
