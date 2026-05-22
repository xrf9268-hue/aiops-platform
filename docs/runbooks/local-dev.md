# Local development runbook

This is the first document to open after cloning. It walks through
bootstrapping the local loop: the SPEC-aligned `cmd/worker` against a
real or mock tracker, optional legacy queue-driven pollers (kept only
for D6/D7 transitional reasons), and smoke checks for the tracker poll.

> This runbook is the operator companion to README's "Quick start:
> worker-owned tracker polling path". If this runbook and the README
> ever diverge, **the README is canonical** — the runbook is meant to
> elaborate on the same flow, not contradict it.

## What `cmd/worker` does (and doesn't)

The worker is the SPEC-aligned entry point. Per D6 (#73), D7 (#74), and
D8 (#76), it has stopped owning behaviors that earlier prototypes
included:

- **No Postgres / queue.** `cmd/worker` does not read `DATABASE_URL`
  and does not enqueue or drain queue rows. Restart recovery follows
  SPEC §14.3: the worker starts with fresh runtime state, cleans
  terminal workspaces from tracker state, and re-dispatches eligible
  active issues on the next poll.
- **No PR creation, no tracker writes.** The worker prepares a
  deterministic Git workspace, runs the configured agent (`mock`,
  `codex`, or `claude`), enforces policy, and stops. Push, draft-PR
  creation, and tracker label transitions happen agent-side via the
  configured tool surface (`linear_graphql`, the workflow prompt's
  push step, etc.), not in `cmd/worker`.

If a doc, log line, or env var hints that the worker creates PRs or
needs Postgres, it is stale — file an issue.

## Prerequisites

- Go 1.25 or newer (matches `go.mod`).
- `git` and `curl`.
- Tracker credentials matching the workflow you're running:
  - `tracker.kind: linear` → a Linear personal API key.
  - `tracker.kind: gitea` → a Gitea base URL and bot token.
  - `tracker.kind: github` → a GitHub token (`gh auth token` works).
- A scratch workspace root. `workspace.root` in `WORKFLOW.md` is the
  source of truth; `WORKSPACE_ROOT` is the fallback when omitted.
- Optional: Docker and Docker Compose v2, only if you want the
  containerized worker or the legacy compose path.
- Optional: the Codex CLI installed and on `PATH`, only if you set
  `agent.default: codex` and want a real model loop.

`cmd/worker` does **not** need Postgres. Skip step 2 below unless you
are intentionally running the legacy queue path.

## 1. Configure the workflow and environment

Copy the example workflow and example env into place. The `.aiops/`
directory does not exist in a fresh clone; create it first:

```bash
mkdir -p .aiops
cp examples/WORKFLOW.md .aiops/WORKFLOW.md   # or wherever you point AIOPS_WORKFLOW_PATH
cp .env.example .env
```

Edit `.env` to fill in only the variables the worker actually reads for
your tracker kind (see [Prerequisites](#prerequisites)). Never commit
a real `.env`.

The worker resolves its workflow source from `AIOPS_WORKFLOW_PATH`
(prefixed) and its fallback workspace root from `WORKSPACE_ROOT`
(unprefixed). The `.env.example` file documents this naming
inconsistency; use the names as written there.

For first-time local testing keep `agent.default: mock` in the
selected `WORKFLOW.md`. The mock runner produces a deterministic change
without calling any external model.

## 2. Run the worker

The worker does **not** read `LINEAR_API_KEY` / `GITEA_TOKEN` /
`GITHUB_TOKEN` directly — those env vars only affect the worker when
`tracker.api_key` in your selected `WORKFLOW.md` references them via
`$VAR` syntax. Before running the worker, make sure the workflow file
has the right `tracker.api_key` mapping for your `tracker.kind`:

| `tracker.kind` | `tracker.api_key` line in `WORKFLOW.md` |
| --- | --- |
| `linear` | `api_key: $LINEAR_API_KEY` |
| `gitea`  | `api_key: $GITEA_TOKEN` |
| `github` | `api_key: $GITHUB_TOKEN` |

The shipped `examples/WORKFLOW.md` and
`examples/github-local-WORKFLOW.md` already use this pattern;
`examples/gitea-WORKFLOW.md` omits it and must be edited before the
worker can poll Gitea.

Option A: from source.

```bash
export AIOPS_WORKFLOW_PATH=$PWD/.aiops/WORKFLOW.md
export WORKSPACE_ROOT=$PWD/.aiops/workspaces

# For tracker.kind: linear  (consumed by WORKFLOW.md "api_key: $LINEAR_API_KEY")
export LINEAR_API_KEY=your-linear-personal-key

# For tracker.kind: gitea
# GITEA_BASE_URL is consumed at runtime as a base-URL fallback when
# tracker.project_slug is empty; GITEA_TOKEN must be wired through
# "api_key: $GITEA_TOKEN" in WORKFLOW.md to actually authenticate.
export GITEA_BASE_URL=https://gitea.example.com
export GITEA_TOKEN=your-gitea-bot-token

# For tracker.kind: github  (consumed via WORKFLOW.md "api_key: $GITHUB_TOKEN")
export GITHUB_TOKEN=$(gh auth token -h github.com)

go run ./cmd/worker
```

Set only the credential block matching your selected `tracker.kind`.

Option B: in Docker.

```bash
docker compose --env-file .env -f deploy/docker-compose.yml up --build worker
```

The Compose service hardcodes `AIOPS_WORKFLOW_PATH=/app/examples/WORKFLOW.md`
and mounts `../examples:/app/examples:ro`, so this command always runs
against the bundled Linear example workflow — edits to your local
`.aiops/WORKFLOW.md` are **not** picked up by the container. To run
the container against your own workflow, either bind-mount your file
over the example path (e.g.
`-v $PWD/.aiops/WORKFLOW.md:/app/examples/WORKFLOW.md:ro`) or override
`AIOPS_WORKFLOW_PATH` in `.env` to a path that resolves inside the
container.

The default Compose service starts only `worker` unless a legacy
profile is explicitly requested (see
[Section 4](#4-legacy-queue-driven-pollers-d6d7d8)).

## 3. Smoke test

The fastest way to verify local configuration is to print the
effective worker config. `--print-config <workdir>` resolves
`<workdir>/WORKFLOW.md` directly and does **not** consult
`AIOPS_WORKFLOW_PATH`, so pass the directory that holds the workflow
you want to inspect — not just `$PWD`:

```bash
go run ./cmd/worker --print-config "$(dirname "$AIOPS_WORKFLOW_PATH")"
```

If you keep your `WORKFLOW.md` at the repo root, `--print-config $PWD`
works the same way. Passing the wrong directory silently reports
default config (with `source: default`) instead of failing, so always
check the `resolution.source` and `resolution.path` fields against the
file you expected.

Secret-bearing fields (`tracker.api_key`, `repo.clone_url` userinfo,
`sandbox.credential_files`) are masked with `***` so the output is
safe to paste into chat or issues.

Once the worker is running, inspect live runtime state via the
loopback-only HTTP server:

```bash
# Aggregate state
curl http://127.0.0.1:4000/api/v1/state

# Per-issue detail
curl http://127.0.0.1:4000/api/v1/MT-649

# Force an immediate poll + reconcile (instead of waiting for the next tick)
curl -X POST -H 'X-AIOPS-Refresh: true' http://127.0.0.1:4000/api/v1/refresh
```

To exercise dispatch end-to-end, configure your `WORKFLOW.md` for a
real Linear, Gitea, or GitHub tracker, keep `agent.default: mock`,
move one issue into an active state, and start the worker. The worker
discovers the active tracker issue and dispatches it; there is no
webhook or manual enqueue endpoint in the normal loop.

## 4. Legacy queue-driven pollers (D6/D7/D8)

`cmd/linear-poller` and `cmd/gitea-poller` write to a Postgres queue
that `cmd/worker` no longer reads. The SPEC-aligned loop runs entirely
inside the worker against the tracker — see Section 2.

Treat anything below as legacy. It is kept only as transitional
ingress code until D7 cleanup completes. **Do not start
`linear-poller` (or `gitea-poller`) and `worker` together expecting a
working stack** — `worker` does not drain the queue, so the rows the
poller produces are never claimed.

### 4.1 Postgres for the legacy pollers

```bash
docker compose --env-file .env -f deploy/docker-compose.yml up -d postgres
docker compose --env-file .env -f deploy/docker-compose.yml \
  exec postgres psql -U aiops -d aiops -c '\dt'
```

You should see `tasks` and `task_events`. The compose file mounts
`migrations/` into Postgres's `/docker-entrypoint-initdb.d`, so the
schema is applied automatically on first startup.

If you need to reapply `migrations/001_init.sql` against an existing
database, the SQL uses `CREATE TABLE IF NOT EXISTS` and is safe to run
again:

```bash
docker compose --env-file .env -f deploy/docker-compose.yml \
  exec -T postgres psql -U aiops -d aiops < migrations/001_init.sql
```

To wipe state:

```bash
docker compose --env-file .env -f deploy/docker-compose.yml down -v
```

### 4.2 Linear legacy poller

```bash
export DATABASE_URL=postgres://aiops:aiops@localhost:5432/aiops?sslmode=disable
export LINEAR_API_KEY=your-linear-personal-key
go run ./cmd/linear-poller examples/WORKFLOW.md
```

The poller exits immediately with `tracker.kind must be linear` if the
workflow is not configured for Linear, and logs `skip <issue>:
repo.clone_url missing in WORKFLOW.md` if `repo.clone_url` is empty.

For Linear workflows, `tracker.project_slug` in the selected
`WORKFLOW.md` maps to Linear's project `slugId` and is required unless
`services[]` routes define per-service `tracker.project_slug` values.

### 4.3 Gitea legacy poller

```bash
export DATABASE_URL=postgres://aiops:aiops@localhost:5432/aiops?sslmode=disable
export GITEA_BASE_URL=http://localhost:3000
export GITEA_TOKEN=your-gitea-bot-token
go run ./cmd/gitea-poller examples/gitea-WORKFLOW.md
```

The poller treats Gitea as a tracker reader; label writes go through
the agent tool surface, not the poller. Default state labels:

| Workflow state | Gitea label |
| --- | --- |
| `AI Ready` | `aiops/todo` |
| `In Progress` | `aiops/in-progress` |
| `Rework` | `aiops/rework` |
| `Done` | `aiops/done` |
| `Canceled` | `aiops/canceled` |

Gitea issue listing is capped at 20 pages of 50 issues per state label
(1000 issues). When the `+1` probe page proves more issues exist, the
poller keeps running with the capped result set and increments
`aiops_gitea_issue_pagination_cap_hits_total`. Expose the counter via
`GITEA_POLLER_METRICS_ADDR`:

```bash
export GITEA_POLLER_METRICS_ADDR=:9091
go run ./cmd/gitea-poller examples/gitea-WORKFLOW.md
curl http://localhost:9091/metrics | grep aiops_gitea_issue_pagination_cap_hits_total
```

The same label mapping is also used by the **SPEC-aligned**
`tracker.kind: gitea` path in `cmd/worker` for per-tick reconciliation:
after a run starts, moving the issue to `aiops/done` or
`aiops/canceled` makes the next worker poll refresh that issue by ID
and cancel the active run. That part is current; only the
queue-writing poller is legacy.

## Common failure modes

### Worker fails to read the workflow

```
workflow: AIOPS_WORKFLOW_PATH refers to /…/WORKFLOW.md which does not exist
```

- `AIOPS_WORKFLOW_PATH` must point at the file, not the directory.
- A YAML front-matter block is optional. Files with no `---` fence
  load as `source: prompt_only` and pick up schema defaults for every
  config field; files with a front-matter block use those values
  instead. Either form is valid — front matter is only required when
  you need to override a default (e.g. a non-default
  `repo.clone_url`, `tracker.kind`, or `agent.default`).
- Run
  `go run ./cmd/worker --print-config $(dirname "$AIOPS_WORKFLOW_PATH")`
  to see how the loader resolves it and which `source` is reported.

### Missing tracker credentials

All tracker tokens flow through `tracker.api_key` in `WORKFLOW.md`,
regardless of `tracker.kind`. The worker does **not** read
`LINEAR_API_KEY` / `GITEA_TOKEN` / `GITHUB_TOKEN` directly at poll
time — the canonical pattern is to set `tracker.api_key: $VAR` in
the workflow and let the loader expand `$VAR` at startup. Only the
tracker's base URL has any direct env fallback (`GITEA_BASE_URL` /
`GITHUB_API_BASE_URL`, applied at runtime when the corresponding
config field is empty).

| `tracker.kind` | `WORKFLOW.md` token reference | Base-URL env fallback |
| --- | --- | --- |
| `linear` | `tracker.api_key: $LINEAR_API_KEY` (or any `$VAR`) | n/a |
| `gitea`  | `tracker.api_key: $GITEA_TOKEN`  (or any `$VAR`) | `GITEA_BASE_URL` when `tracker.project_slug` is empty |
| `github` | `tracker.api_key: $GITHUB_TOKEN` (or any `$VAR`) | `GITHUB_API_BASE_URL` when `tracker.base_url` is empty |

When the failure surfaces depends on how `tracker.api_key` is encoded
in `WORKFLOW.md`:

- If `tracker.api_key` is an explicit env reference (`$VAR` or
  `${VAR}`) and the env var is unset or empty, workflow loading fails
  at startup with `missing_tracker_api_key` before the first poll.
  This is the loud, fail-fast path — fix the env var and retry.
- If `tracker.api_key` is empty (or missing entirely), startup
  succeeds with no warning and the first tracker poll fails (e.g. the
  Gitea client errors with `GITEA_BASE_URL and Gitea tracker api_key
  are required`). Check the worker log for the first poll cycle, not
  just the startup line.
- A non-empty literal value (e.g. `tracker.api_key: ghp_…`) is passed
  through unchanged and used as the token — that is a valid
  configuration, just not the recommended one because the secret then
  sits in `WORKFLOW.md` rather than the environment. Operators who
  paste a raw token here are not hitting a loader bug; they are
  bypassing the env-expansion path.

### `/api/v1/state` returns nothing or refuses to bind

- The HTTP server is loopback-only (`127.0.0.1:server.port`). From
  inside a container, expose it explicitly or run the worker on the
  host.
- The server is disabled when `server.port: -1` is set in
  `WORKFLOW.md`. Use `--print-config` to confirm the effective port.

### `connection refused` against Postgres (legacy pollers only)

`cmd/worker` does **not** need Postgres — if you see this error from
the worker, you are running an out-of-date binary or an old shell with
stale env vars. For the legacy pollers:

- Check `docker compose ps` and ensure the `postgres` service is
  healthy.
- Confirm `DATABASE_URL` matches the host you are running from.
  Inside compose use `postgres:5432`, from your host use
  `localhost:5432`.

### `tasks` table does not exist (legacy pollers only)

The init script in `migrations/001_init.sql` only runs on the very
first start of the Postgres volume. Run `docker compose down -v` to
drop the volume and start again, or apply the SQL manually as shown
in [Section 4.1](#41-postgres-for-the-legacy-pollers).

### Agent fails to push or open PRs

PR creation lives **agent-side**, not in the worker. The worker no
longer holds Gitea credentials. If the agent (Codex, Claude, etc.)
needs to push:

- The agent invocation environment must carry credentials matching
  the configured remote (SSH key, deploy key, or
  `GITEA_TOKEN`/`GITHUB_TOKEN` exported to the agent process).
- For Docker Compose, the worker container mounts a **dedicated** SSH
  keypair at `/root/.ssh/id_ed25519` — not your entire `~/.ssh`. Set
  it up once:

  ```bash
  cd deploy
  ssh-keygen -t ed25519 -f ssh/id_ed25519 -C aiops-worker-deploy-key -N ''
  ssh-keyscan -H <your-gitea-host> >> ssh/known_hosts
  ```

  Then register `deploy/ssh/id_ed25519.pub` as a Gitea / GitHub deploy
  key on the target repository. The keypair lives outside version
  control thanks to the root `.gitignore`. Override the path with
  `AIOPS_SSH_KEY_PATH` / `AIOPS_SSH_KNOWN_HOSTS_PATH` in `.env` if
  you keep the key elsewhere. See `docs/security-posture.md` ("Docker
  Compose SSH key isolation") for the rationale — this scoping closed
  the broad credential exposure described in issue #221.

### Port `5432` already in use (legacy pollers only)

Stop the conflicting local service or change the published port in
`deploy/docker-compose.yml` and `DATABASE_URL` accordingly.

### `go: module lookup disabled` or `go.sum` mismatch

Run `go mod tidy`, then re-run the failing command. CI rejects changes
that leave `go.mod` or `go.sum` dirty; see [CI/CD runbook](ci.md) for
the local pre-push check list.

## Workspace cache and cleanup

The worker keeps a per-repo bare mirror under `AIOPS_MIRROR_ROOT`
(default `os.UserCacheDir()/aiops-platform/mirrors`) and creates a
per-task worktree under the selected workflow's `workspace.root` for
every claimed task. If `workspace.root` is omitted from `WORKFLOW.md`,
the worker falls back to `WORKSPACE_ROOT`. This avoids re-cloning on
every retry and lets two tasks run concurrently without sharing a
working tree. See the dedicated
[workspace cache runbook](workspace-cache.md) for the on-disk layout,
configuration knobs, and recommended cleanup cadence (the
`(*workspace.Manager).Cleanup` API or removal of the effective
workspace root once old tasks no longer matter).

## Running e2e tests locally

The e2e suite under `test/e2e/` validates the Gitea poller loop
against real Postgres and Gitea containers. It is gated by the `e2e`
build tag and does not run as part of `go test ./...`.

Requirements: a working Docker daemon. Cold first run pulls ~600MB of
images and takes 2–3 minutes. Warm runs take ~10 seconds for all four
tests.

```bash
go test -tags e2e -race -timeout 15m ./test/e2e/...
```

Common failure modes:

- `Cannot connect to the Docker daemon` — start Docker Desktop or
  `colima`.
- `go test` reports `build constraints exclude all Go files` — the
  `-tags e2e` flag is missing.
