# Local development runbook

This is the first document to open after cloning. It walks through
bootstrapping the local loop: the SPEC-aligned `cmd/worker` against a
real or mock tracker and smoke checks for the tracker poll.

> This runbook is the operator companion to README's "Quick start:
> worker-owned tracker polling path". If this runbook and the README
> ever diverge, **the README is canonical** — the runbook is meant to
> elaborate on the same flow, not contradict it.

## What `cmd/worker` does (and doesn't)

The worker is the SPEC-aligned entry point. Per D6 (#73/#407), D7 (#74), and
D8 (#76), it has stopped owning behaviors that earlier prototypes included:

- **No Postgres / queue.** `cmd/worker` does not read `DATABASE_URL`
  and does not enqueue or drain queue rows. Restart recovery follows
  SPEC §14.3: the worker starts with fresh runtime state, cleans
  terminal workspaces from tracker state, and re-dispatches eligible
  active issues on the next poll.
- **No PR creation, no tracker writes.** The worker prepares a
  deterministic Git workspace, runs the configured agent (`mock`,
  `codex-app-server`, or `claude`), and stops — it enforces no post-run
  policy gate (DEVIATIONS D33, #561; `policy.mode` only selects the
  analysis-only vs draft-PR prompt directive). Push, draft-PR
  creation, and tracker label transitions happen agent-side via the
  configured tool surface (`linear_graphql`, the workflow prompt's
  push step, etc.), not in `cmd/worker`.

If a doc, log line, or env var hints that the worker creates PRs or
needs Postgres, it is stale — file an issue.

## Prerequisites

- Go 1.25.11 or newer (the exact patch floor pinned in `go.mod` and the
  Dockerfile).
- `git` and `curl`.
- Tracker credentials matching the workflow you're running:
  - `tracker.kind: linear` → a Linear personal API key.
  - `tracker.kind: gitea` → a Gitea base URL in `tracker.endpoint`
    and a bot token.
  - `tracker.kind: github` → a GitHub token (`gh auth token` works).
- A scratch workspace root. `workspace.root` in `WORKFLOW.md` is the
  source of truth; `AIOPS_WORKSPACE_ROOT` (legacy alias `WORKSPACE_ROOT`)
  is the fallback when omitted.
- Optional: Docker and Docker Compose v2, only if you want the
  containerized worker or e2e tests. To deploy the worker as a plain
  binary under an init system instead, see the
  [binary deployment runbook](binary-deployment.md).
- Optional: the Codex CLI installed and on `PATH`, only if you set
  `agent.default: codex-app-server` and want a real model loop.

`cmd/worker` does **not** need Postgres.

## Quick local validation

From a fresh checkout, run the lightweight non-container validation shim before
starting a worker loop or opening a PR:

```bash
./scripts/dev-test.sh
```

The script reads the pinned Go version from `go.mod`, checks the effective
`go env GOVERSION`, then runs the credential-free Go path: `gofmt -l`,
`go mod tidy` with a clean `go.mod` / `go.sum` diff, and `go test ./...`.
It does not require tracker credentials, dashboard dependencies, or an e2e
container runtime.

Keep `GOTOOLCHAIN=auto` enabled unless you have already installed the pinned Go
toolchain locally. With `GOTOOLCHAIN=auto`, modern Go can download the required
toolchain when the checkout asks for Go 1.25.11. If the machine is offline or
the download is blocked, install Go 1.25.11 first (or pre-seed the toolchain)
and re-run `./scripts/dev-test.sh`.

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

The worker resolves its workflow source from `AIOPS_WORKFLOW_PATH` and
its fallback workspace root from `AIOPS_WORKSPACE_ROOT`. Worker env vars
share the `AIOPS_` prefix; the legacy unprefixed forms (`WORKSPACE_ROOT`,
`MIRROR_ROOT`, `WORKFLOW_PATH`) are still honored as deprecated aliases
but log a warning at startup. See `.env.example`.

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

The shipped examples — `examples/WORKFLOW.md`,
`examples/gitea-WORKFLOW.md`, and `examples/github-local-WORKFLOW.md` —
already use this pattern.

Option A: from source.

```bash
export AIOPS_WORKFLOW_PATH=$PWD/.aiops/WORKFLOW.md
export AIOPS_WORKSPACE_ROOT=$PWD/.aiops/workspaces

# For tracker.kind: linear  (consumed by WORKFLOW.md "api_key: $LINEAR_API_KEY")
export LINEAR_API_KEY=your-linear-personal-key

# For tracker.kind: gitea
# Prefer tracker.endpoint in WORKFLOW.md. GITEA_BASE_URL is only a runtime
# base-URL fallback when tracker.endpoint is empty; GITEA_TOKEN must be wired
# through "api_key: $GITEA_TOKEN" in WORKFLOW.md to actually authenticate.
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
as a fixed literal (not `${AIOPS_WORKFLOW_PATH}`) and mounts
`../examples:/app/examples:ro`, so this command always runs against
the bundled Linear example workflow — edits to your local
`.aiops/WORKFLOW.md` are **not** picked up by the container, and
setting `AIOPS_WORKFLOW_PATH` in `.env` has no effect. To run the
container against your own workflow:

- Bind-mount your file over the example path (simplest):

  ```bash
  docker compose --env-file .env -f deploy/docker-compose.yml run \
    -v "$PWD/.aiops/WORKFLOW.md:/app/examples/WORKFLOW.md:ro" worker
  ```

- Or override `AIOPS_WORKFLOW_PATH` per invocation with
  `docker compose run -e AIOPS_WORKFLOW_PATH=/app/your/path worker`
  (combined with whatever mount makes that path readable).

- Or maintain a `deploy/docker-compose.override.yml` that re-declares
  `environment.AIOPS_WORKFLOW_PATH` for the `worker` service.

The default Compose service starts only `worker`.
The worker image and Compose service include a health check against
`http://127.0.0.1:${AIOPS_HEALTHCHECK_PORT:-4000}/livez` from inside the
container. Use `/readyz` when a local orchestrator needs the startup-readiness
signal instead of the liveness signal; both probes are unauthenticated, and
`/readyz` returns `503` until startup reconciliation finishes. If you change the
worker HTTP port, set `AIOPS_HEALTHCHECK_PORT` to match; if you disable the HTTP
listener with `server.port: -1`, disable the container health check too.

The worker's loopback dashboard is not reachable from the host under the
base Compose file. To reach it, merge the opt-in overlay, which binds
`0.0.0.0` inside the container and publishes only to host loopback
(`127.0.0.1:4000:4000`). Docker forwards from a bridge peer rather than
container loopback, so the overlay requires a state API token:

```bash
mkdir -p .aiops/secrets
# Scope umask 077 to the write so the token lands as 0600 with no 0644 window.
( umask 077; openssl rand -hex 24 > .aiops/secrets/state_api_token )
AIOPS_STATE_API_TOKEN_FILE=$PWD/.aiops/secrets/state_api_token \
docker compose -f deploy/docker-compose.yml \
  -f deploy/docker-compose.dashboard.yml up worker
```

See README "Operator surfaces" for the trust-boundary caveats; the
overlay requires auth on every request. The browser Basic-auth username is
`aiops` and the password is the contents of
`.aiops/secrets/state_api_token`. Open
`http://127.0.0.1:4000/` and let the browser show its Basic-auth prompt instead
of embedding credentials in the URL. For `cmd/tui`, set
`AIOPS_STATE_API_TOKEN` from the same secret file; the client sends it as a
bearer token when polling the overlay.

> **Upgrading from a root-running worker image.** The worker now runs as the
> unprivileged `aiops` user (#365). A `workspaces` named volume created by an
> older root-running image stays root-owned and the non-root worker cannot
> write to it, surfacing as permission errors. Workspace contents are
> disposable per-issue git checkouts, so drop and recreate the volume once
> after upgrading:
>
> ```bash
> docker compose --env-file .env -f deploy/docker-compose.yml down
> docker volume rm deploy_workspaces   # name may be <project>_workspaces
> ```

## 3. Smoke test

For a first install, run the operator preflight before starting a worker:

```bash
go run ./cmd/worker --doctor --mode=mock "$AIOPS_WORKFLOW_PATH"
```

Use `--mode=real` only when the selected workflow should validate live Linear
auth and a real Codex app-server setup.

The fastest way to inspect the effective local configuration is to print the
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

## 4. Gitea label mapping

The worker treats Gitea as a tracker reader; label writes go through the agent
tool surface, not the worker. Default state labels:

| Workflow state | Gitea label |
| --- | --- |
| `Todo` | `aiops/todo` |
| `In Progress` | `aiops/in-progress` |
| `Human Review` | `aiops/human-review` |
| `Rework` | `aiops/rework` |
| `Done` | `aiops/done` |
| `Canceled` | `aiops/canceled` |

Gitea issue listing is capped at 20 pages of 50 issues per state label by
default (1000 issues). Override the page budget with
`tracker.pagination_max_pages` when a repository legitimately needs more. The
same label mapping is used for per-tick reconciliation: after a run starts,
moving the issue to `aiops/done` or `aiops/canceled` makes the next worker poll
refresh that issue by ID and cancel the active run.

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
| `gitea`  | `tracker.api_key: $GITEA_TOKEN`  (or any `$VAR`) | `GITEA_BASE_URL` when `tracker.endpoint` is empty |
| `github` | `tracker.api_key: $GITHUB_TOKEN` (or any `$VAR`) | `GITHUB_API_BASE_URL` when `tracker.endpoint` is empty |

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

### Agent fails to push or open PRs

PR creation lives **agent-side**, not in the worker. The worker no
longer holds Gitea credentials. If the agent (Codex, Claude, etc.)
needs to push:

- Prefer file-backed Git credentials such as a dedicated SSH deploy key. Agent
  subprocesses do not inherit the worker's full environment, and
  `codex.env_passthrough` / `claude.env_passthrough` reject tracker/repo token
  names such as `LINEAR_API_KEY`, `GITEA_TOKEN`, and `GITHUB_TOKEN`.
- For Docker Compose, the worker container mounts a **dedicated** SSH
  keypair at `/home/aiops/.ssh/id_ed25519` — not your entire `~/.ssh`. Set
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

### `go: module lookup disabled` or `go.sum` mismatch

Run `go mod tidy`, then re-run the failing command. CI rejects changes
that leave `go.mod` or `go.sum` dirty; see [CI/CD runbook](ci.md) for
the local pre-push check list.

## Workspace cache and cleanup

The worker keeps a per-repo bare mirror under `AIOPS_MIRROR_ROOT`
(default `os.UserCacheDir()/aiops-platform/mirrors`) and creates a
per-task worktree under the selected workflow's `workspace.root` for
every claimed task. If `workspace.root` is omitted from `WORKFLOW.md`,
the worker falls back to `AIOPS_WORKSPACE_ROOT` (legacy alias
`WORKSPACE_ROOT`). This avoids re-cloning on
every retry and lets two tasks run concurrently without sharing a
working tree. See the dedicated
[workspace cache runbook](workspace-cache.md) for the on-disk layout,
configuration knobs, and the recommended manual cleanup cadence
(removing the effective workspace root once old tasks no longer matter).

## Running e2e tests locally

The e2e suite under `test/e2e/` validates the Gitea worker loop
against a real Gitea container. It is gated by the `e2e`
build tag and does not run as part of `go test ./...`.

Requirements: a working Docker daemon. Cold first run pulls ~600MB of
images and takes 2–3 minutes. Warm runs take ~10 seconds for all
tests.

```bash
go test -tags e2e -race -timeout 15m ./test/e2e/...
```

Common failure modes:

- `Cannot connect to the Docker daemon` — start Docker Desktop or
  `colima`.
- `go test` reports `build constraints exclude all Go files` — the
  `-tags e2e` flag is missing.
