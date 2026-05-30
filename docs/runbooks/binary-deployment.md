# Binary (non-Docker) deployment runbook

This runbook deploys `cmd/worker` as a plain binary on a host, with no
container runtime. Docker is **optional** in this project (see
[`local-dev.md`](local-dev.md) "Prerequisites"); the worker is a
self-contained Go binary that launches the configured agent as a local
subprocess. Use this path when you want to run the worker directly under
an init system (systemd) or a process supervisor instead of Compose.

> This runbook is the operator companion to README's "Quick start:
> worker-owned tracker polling path" and to [`local-dev.md`](local-dev.md).
> If they ever diverge, **the README is canonical**; this runbook
> elaborates the same flow for a binary host deployment.

## What the binary does and does not depend on

- **No Postgres / queue.** `cmd/worker` never reads `DATABASE_URL`.
  Restart recovery follows SPEC §14.3: fresh runtime state, re-dispatch
  eligible active issues on the next poll.
- **No container runtime.** The agent runners (`mock`, `codex`,
  `codex-app-server`, `claude`) launch local subprocesses via `os/exec`;
  none shell out to `docker`. `testcontainers-go` is test-only.
- **No PR creation / tracker writes.** The worker prepares a git
  workspace, runs the agent, enforces policy, and stops. Push and
  draft-PR creation happen agent-side.

## 1. Host prerequisites

These are host-level tools (not an image). Install whatever your
configured `agent.default` and clone URLs actually exercise:

| Tool | Required? | Why |
| --- | --- | --- |
| `git` | Always | Prepares the deterministic per-issue workspace. |
| `ca-certificates` | Always | TLS to the tracker / git remote. |
| `ssh` (`openssh-client`) | When pushing over an SSH clone URL | The agent pushes branches; `worker --doctor` flags a missing `ssh` only when SSH clone URLs are configured. |
| `rg` (ripgrep) | Recommended | Faster agent code search; doctor warns if absent. |
| `codex` / `claude` CLI on `PATH` | Only when `agent.default` selects it | `mock` needs no external CLI. Keep first real runs on `mock`. |

> **Note on `worker --doctor` wording.** Several doctor remediation
> strings say "… in the worker image" (e.g. a missing `ssh` suggests
> "Install ssh in the worker image"), and the run includes Docker
> Compose checks. Those messages assume the container path; on a binary
> host read them as "install the tool on this host and ensure it is on
> `PATH`," and ignore the Compose checks. Making doctor
> deployment-mode-aware is tracked separately.

## 2. Obtain the binaries

### Option A — build from source (pin Go 1.25 per `go.mod`)

```bash
scripts/install.sh                 # builds dist/worker and dist/tui
sudo scripts/install.sh --prefix /usr/local   # build + install to /usr/local/bin
```

The script uses the same canonical flags as CI
(`-trimpath -ldflags="-s -w"`). Equivalent manual build:

```bash
mkdir -p dist
go build -trimpath -ldflags="-s -w" -o dist/worker ./cmd/worker
go build -trimpath -ldflags="-s -w" -o dist/tui    ./cmd/tui
```

### Option B — download a release archive

`.github/workflows/release.yml` publishes static archives
(`CGO_ENABLED=0`) for `linux/{amd64,arm64}` and `darwin/{amd64,arm64}`,
each with an SBOM and build-provenance attestation. Download the archive
matching your `GOOS/GOARCH`, verify the attestation with
`gh attestation verify`, then unpack `worker` (and optionally `tui`)
onto the host `PATH`.

## 3. Configure the workflow and environment

The worker reads its workflow from `AIOPS_WORKFLOW_PATH` and its fallback
workspace root from `AIOPS_WORKSPACE_ROOT`. Tracker tokens are **not**
read directly by the worker — they flow through `tracker.api_key: $VAR`
expansion in `WORKFLOW.md` (see [`local-dev.md`](local-dev.md)
"Missing tracker credentials"). Set `tracker.api_key` for your
`tracker.kind`:

| `tracker.kind` | `WORKFLOW.md` token reference |
| --- | --- |
| `linear` | `tracker.api_key: $LINEAR_API_KEY` |
| `gitea`  | `tracker.api_key: $GITEA_TOKEN` |
| `github` | `tracker.api_key: $GITHUB_TOKEN` |

Lay down a config directory and an env file (never commit a real env
file):

```bash
sudo install -d -o aiops -g aiops /etc/aiops-platform
sudo cp examples/WORKFLOW.md /etc/aiops-platform/WORKFLOW.md   # edit tracker.api_key/kind
sudo cp .env.example /etc/aiops-platform/worker.env           # fill in the token + paths
sudo chmod 600 /etc/aiops-platform/worker.env
```

Keep `agent.default: mock` in `WORKFLOW.md` until the loop is trusted on
the target repo.

## 4. Validate before running

Run the operator preflight; it should pass with no Docker present (the
Compose checks are advisory on this path — see §1):

```bash
export AIOPS_WORKFLOW_PATH=/etc/aiops-platform/WORKFLOW.md
worker --doctor --mode=mock "$AIOPS_WORKFLOW_PATH"
# then, when the workflow should validate live tracker auth + a real agent:
worker --doctor --mode=real "$AIOPS_WORKFLOW_PATH"
```

Inspect the effective config (secrets are masked with `***`):

```bash
worker --print-config "$(dirname "$AIOPS_WORKFLOW_PATH")"
```

## 5. Run

### Foreground (development)

```bash
export AIOPS_WORKFLOW_PATH=/etc/aiops-platform/WORKFLOW.md
export AIOPS_WORKSPACE_ROOT=/var/lib/aiops-platform/workspaces
export LINEAR_API_KEY=...        # only the var your WORKFLOW.md tracker.api_key references
worker
```

The worker exposes a loopback-only HTTP server on
`127.0.0.1:${server.port}` (default `4000`): `/livez`, `/readyz`
(`503` until startup reconciliation finishes), and `/api/v1/state`.
Override the bind port with `-port` (`-1` disables the server) and the
host with `AIOPS_SERVER_HOST`. The dashboard/state API is unauthenticated
on pure loopback; if you bind anything other than loopback, require a
token via `AIOPS_STATE_API_TOKEN` — see README "Operator surfaces".

### Under systemd (production)

A hardened sample unit ships at
[`deploy/systemd/aiops-worker.service`](../../deploy/systemd/aiops-worker.service).
It runs as a dedicated non-root `aiops` user (mirroring the Docker
hardening in #365), loads secrets from an `EnvironmentFile`, and keeps
mutable state under `/var/lib/aiops-platform` via `StateDirectory`.

```bash
# one-time: create the service user and state dir
sudo useradd --system --create-home --home-dir /var/lib/aiops-platform \
  --shell /usr/sbin/nologin aiops

# install the binary and unit
sudo scripts/install.sh --prefix /usr/local
sudo cp deploy/systemd/aiops-worker.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now aiops-worker.service

# observe
systemctl status aiops-worker.service
journalctl -u aiops-worker.service -f
curl http://127.0.0.1:4000/api/v1/state
```

Edit the unit's `EnvironmentFile=` / `ExecStart=` paths if you installed
the binary or config elsewhere. If you set `server.port: -1` to disable
the HTTP server, drop any external health probe that targets it.

## Common failure modes

- **`missing_tracker_api_key` at startup** — `tracker.api_key` references
  a `$VAR` that is unset. Confirm the env var is exported in the same
  environment as the worker (for systemd, that it is present in
  `EnvironmentFile`).
- **Agent cannot push** — the worker does not hold git credentials;
  provide a file-backed credential (e.g. a dedicated SSH deploy key) the
  agent subprocess can read. See [`local-dev.md`](local-dev.md)
  "Agent fails to push or open PRs".
- **`/api/v1/state` refuses to bind** — the server is loopback-only and
  disabled when `server.port: -1`. Confirm with `worker --print-config`.
- **Workspace permission errors under systemd** — ensure
  `AIOPS_WORKSPACE_ROOT` and `AIOPS_MIRROR_ROOT` point at directories the
  `aiops` user owns (the sample unit uses `StateDirectory`).

## See also

- [`local-dev.md`](local-dev.md) — the canonical local loop and the
  full failure-mode catalog.
- [`codex-app-server-docker.md`](codex-app-server-docker.md) — the
  container path for `agent.default: codex-app-server`.
- README "Quick start" and "Operator surfaces".
