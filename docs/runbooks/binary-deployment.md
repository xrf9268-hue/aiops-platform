# Binary (non-Docker) deployment runbook

This runbook deploys `cmd/worker` as a plain binary on a host, with no
container runtime. Docker is **optional** in this project (see
[`local-dev.md`](local-dev.md) "Prerequisites"); the worker is a
self-contained Go binary that launches the configured agent as a local
subprocess. Use this path when you want to run the worker directly under
a service manager (systemd on Linux, launchd on macOS) instead of Compose.

> This runbook is the operator companion to README's "Quick start:
> worker-owned tracker polling path" and to [`local-dev.md`](local-dev.md).
> If they ever diverge, **the README is canonical**; this runbook
> elaborates the same flow for a binary host deployment.

## What the binary does and does not depend on

- **No Postgres / queue.** `cmd/worker` never reads `DATABASE_URL`.
  Restart recovery follows SPEC §14.3: fresh runtime state, re-dispatch
  eligible active issues on the next poll.
- **No container runtime.** The agent runners (`mock`, `codex-app-server`,
  `claude`) launch local subprocesses via `os/exec`;
  none shell out to `docker`. `testcontainers-go` is test-only.
- **No PR creation / tracker writes.** The worker prepares a git
  workspace, runs the agent, and stops. Push and draft-PR creation
  happen agent-side; the worker enforces no post-run policy gate
  (DEVIATIONS D33, #561 — `policy.mode` only selects the analysis-only
  vs draft-PR prompt directive).

## 1. Host prerequisites

This runbook covers both **Linux** (systemd) and **macOS** (launchd)
hosts. The binary, build, config, and `--doctor` steps are identical on
both; only the service-manager integration (§5) and a few default paths
differ, called out inline.

These are host-level tools (not an image). Install whatever your
configured `agent.default` and clone URLs actually exercise:

| Tool | Required? | Why |
| --- | --- | --- |
| `git` | Always | Prepares the deterministic per-issue workspace. |
| `ca-certificates` | Always | TLS to the tracker / git remote. |
| `ssh` (`openssh-client`) | When pushing over an SSH clone URL | The agent pushes branches; `worker --doctor` flags a missing `ssh` only when SSH clone URLs are configured. |
| `rg` (ripgrep) | Recommended | Faster agent code search; doctor warns if absent. |
| `codex` / `claude` CLI on `PATH` | Only when `agent.default` selects it | `mock` needs no external CLI. Keep first real runs on `mock`. |

Install hints by platform:

- **Debian/Ubuntu:** `sudo apt-get install -y git ca-certificates openssh-client ripgrep`
- **macOS:** `git` and `ssh` ship with the Xcode Command Line Tools
  (`xcode-select --install`); add ripgrep with `brew install ripgrep`.

> **Run `worker --doctor` with `--deploy=binary`** on a binary host. That
> skips the container-only Docker Compose checks (irrelevant without a
> container, and they would otherwise FAIL in `--mode=real`) and phrases
> install hints for a host `PATH` rather than a worker image. The default
> `--deploy=docker` is for the container path (`first-run-docker-linear-codex.md`).

### Unprivileged user namespaces (codex-app-server only)

With `agent.default: codex-app-server`, Codex runs the agent's shell commands
inside a bubblewrap (`bwrap`) sandbox that needs **unprivileged user
namespaces**. On modern Ubuntu (24.04+, and observed on kernel 6.17) these are
**off by default** — `kernel.apparmor_restrict_unprivileged_userns=1` makes
`bwrap` fail with `bwrap: setting up uid map: Permission denied`, which breaks
every agent command (edit / test / push). The run still dispatches, burns a full
turn, and is recorded as failed. This is **not** required for `agent.default:
mock`, for the `claude` runner, or when `codex.thread_sandbox: danger-full-access`.

**Verify (run as the worker's service user):**

```bash
# 1 = restricted (Ubuntu 24.04+ default); 0 = allowed
sysctl kernel.apparmor_restrict_unprivileged_userns
# Most authoritative: exercise Codex's own sandbox. Must print "ok".
codex sandbox -- /bin/echo ok
# Or the raw capability:
bwrap --ro-bind / / --unshare-user --uid 0 -- /bin/echo ok
```

Run `worker --doctor --deploy=binary --mode=real` before a real run: its codex
sandbox preflight FAILs here (with the remediation below) instead of letting a
dispatched run burn a turn.

**Remediation (Ubuntu 24.04+).** Prefer the least-privileged option that works
for your host:

- **Run the worker in a container** that allows user namespaces (the container's
  sandbox model differs; see `first-run-docker-linear-codex.md`). This is the
  most practical fix when you already deploy with a container runtime.
- **Scoped AppArmor profile.** Keeps the host-wide restriction in place for every
  other binary, so it is the safest fix on a shared host. Ubuntu 24.04+ keys
  unprivileged-userns permission to the **executable path**, and Codex runs its
  own vendored `bwrap` (not the system one), so the profile must point at that
  binary. Locate it and grant `userns`:

  ```bash
  CODEX_BWRAP=$(find "$(dirname "$(readlink -f "$(command -v codex)")")/.." \
    -name bwrap -path '*codex-resources*' 2>/dev/null | head -1)
  sudo tee /etc/apparmor.d/codex-bwrap >/dev/null <<EOF
  abi <abi/4.0>,
  include <tunables/global>
  profile codex-bwrap "$CODEX_BWRAP" flags=(unconfined) {
    userns,
    include if exists <local/codex-bwrap>
  }
  EOF
  sudo apparmor_parser -r /etc/apparmor.d/codex-bwrap
  ```

  Re-run the locate step after a Codex upgrade — the vendored path is
  version-specific.
- **Relax the host-wide sysctl (last resort).** This re-enables unprivileged
  user namespaces for **every** process on the host, weakening its security
  posture — only do this on a single-purpose host:

  ```bash
  # runtime (resets on reboot)
  sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
  # persistent (apply now with --system, otherwise it waits for reboot)
  echo 'kernel.apparmor_restrict_unprivileged_userns=0' | \
    sudo tee /etc/sysctl.d/99-userns.conf
  sudo sysctl --system
  ```

- **Or skip the sandbox** with `codex.thread_sandbox: danger-full-access` —
  **only** in an already-isolated environment (a dedicated VM/container), since
  it runs agent commands without a user-namespace sandbox.

## 2. Obtain the binaries

### Option A — build from source (pin Go 1.25 per `go.mod`)

```bash
scripts/install.sh                 # builds dist/worker and dist/tui
sudo scripts/install.sh --prefix /usr/local   # build + install to /usr/local/bin
```

The script uses the same canonical flags as the CI build
(`-trimpath -ldflags="-s -w"`). It does not set `CGO_ENABLED=0`, so a
source build may be dynamically linked against the host libc, unlike the
fully static release archives below. Equivalent manual build:

```bash
mkdir -p dist
go build -trimpath -ldflags="-s -w" -o dist/worker ./cmd/worker
go build -trimpath -ldflags="-s -w" -o dist/tui    ./cmd/tui
```

### Option B — download a release archive

`.github/workflows/release.yml` publishes one fully static
(`CGO_ENABLED=0`) archive per platform —
`aiops-platform_<tag>_<goos>_<goarch>.tar.gz` for `linux/{amd64,arm64}`
and `darwin/{amd64,arm64}`, each containing both `worker` and `tui` —
plus a single aggregate CycloneDX SBOM
(`aiops-platform_<tag>_sbom.cdx.json`) and a build-provenance attestation
covering all release assets. Download the archive matching your
`GOOS/GOARCH`, verify its provenance with
`gh attestation verify <archive> --repo xrf9268-hue/aiops-platform`,
then unpack `worker` (and optionally `tui`) onto the host `PATH`.

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
# Pick the example matching your tracker.kind: examples/WORKFLOW.md (linear),
# examples/gitea-WORKFLOW.md, or examples/github-local-WORKFLOW.md.
sudo cp examples/WORKFLOW.md /etc/aiops-platform/WORKFLOW.md   # edit tracker.api_key/kind
sudo cp .env.example /etc/aiops-platform/worker.env           # fill in the token + paths
sudo chmod 600 /etc/aiops-platform/worker.env
```

`examples/WORKFLOW.md` and `.env.example` ship in the source checkout
(Option A). The release archive (Option B) is self-contained: it bundles
every `examples/` WORKFLOW template, `.env.example`, `README.md`, and
`LICENSE` alongside the binaries, so `cp` the example matching your
`tracker.kind` straight from the extracted directory.

The paths above use the Linux FHS layout (`/etc`, `/var/lib`). On macOS
the conventional equivalents are `/usr/local/etc/aiops-platform` and
`/usr/local/var/aiops-platform` (Homebrew prefix), or a per-user
`~/Library/Application Support/aiops-platform`. The §5 launchd example
uses the `/usr/local` paths; substitute consistently.

Keep `agent.default: mock` in `WORKFLOW.md` until the loop is trusted on
the target repo.

## 4. Validate before running

Run the operator preflight with `--deploy=binary` (see §1) so it skips the
container-only Docker Compose checks and gives host-oriented install hints:

```bash
export AIOPS_WORKFLOW_PATH=/etc/aiops-platform/WORKFLOW.md
worker --doctor --deploy=binary --mode=mock "$AIOPS_WORKFLOW_PATH"
# then, when the workflow should validate live tracker auth + a real agent:
worker --doctor --deploy=binary --mode=real "$AIOPS_WORKFLOW_PATH"
```

With `--deploy=binary` there is no spurious Docker Compose failure to
discount, so any `FAIL` in `--mode=real` reflects a real problem on this
path (e.g. a missing `gh`/deploy-key credential at the agent `git push`
preflight).

Inspect the effective config (secrets are masked with `***`):

```bash
worker --print-config "$(dirname "$AIOPS_WORKFLOW_PATH")"
```

## 5. Run

### Foreground (development)

```bash
export AIOPS_WORKFLOW_PATH=$PWD/WORKFLOW.md
export AIOPS_WORKSPACE_ROOT=$PWD/.aiops/workspaces   # any path the user can write
export LINEAR_API_KEY=...        # only the var your WORKFLOW.md tracker.api_key references
worker
```

This works the same on Linux and macOS. Use a user-writable workspace
root here; the system paths in §3 are owned by the service user and are
for the supervised setups below.

The worker exposes a loopback-only HTTP server on
`127.0.0.1:${server.port}` (default `4000`): `/livez`, `/readyz`
(`503` until startup reconciliation finishes), and `/api/v1/state`.
Override the bind port with `--port` (`-1` disables the server) and the
host with `AIOPS_SERVER_HOST`. The dashboard/state API is unauthenticated
on pure loopback; if you bind anything other than loopback, require a
token via `AIOPS_STATE_API_TOKEN` — see README "Operator surfaces".

### Linux: under systemd (production)

A hardened sample unit ships at
[`deploy/systemd/aiops-worker.service`](../../deploy/systemd/aiops-worker.service).
It runs as a dedicated non-root `aiops` user (mirroring the Docker
hardening in #365), loads secrets from an `EnvironmentFile`, and keeps
mutable state under `/var/lib/aiops-platform` via `StateDirectory`.

```bash
# one-time: create the service user. StateDirectory= creates and chowns
# /var/lib/aiops-platform on first start, so --create-home is not needed.
sudo useradd --system --home-dir /var/lib/aiops-platform \
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

**SSH deploy key under hardening.** The unit sets `ProtectHome=true`,
which makes `/home`, `/root`, and `/run/user` inaccessible — but **not**
`/var/lib`. Because the unit sets the `aiops` user's `$HOME` to its state
dir (`/var/lib/aiops-platform`), place the agent's git deploy key at
`/var/lib/aiops-platform/.ssh/id_ed25519` (owned by `aiops`, mode `0600`)
so `ssh` resolves it via `$HOME/.ssh` and `ReadWritePaths=` already
covers it:

```bash
sudo -u aiops install -d -m 700 /var/lib/aiops-platform/.ssh
sudo -u aiops ssh-keygen -t ed25519 -N '' \
  -f /var/lib/aiops-platform/.ssh/id_ed25519 -C aiops-worker-deploy-key
# register /var/lib/aiops-platform/.ssh/id_ed25519.pub as a deploy key, then:
sudo -u aiops bash -c 'ssh-keyscan -H <git-host> >> /var/lib/aiops-platform/.ssh/known_hosts'
```

A key left under a conventional home such as `/home/aiops/.ssh` is hidden
by `ProtectHome=true` and the agent's `git push` over SSH then fails with
a confusing missing-key/auth error. For the same reason the unit pins
`AIOPS_MIRROR_ROOT` inside the state dir: the worker's default mirror root
is `os.UserCacheDir()/aiops-platform/mirrors`, which would otherwise
resolve under `$HOME` and must stay within `ReadWritePaths=`.

### macOS: under launchd

A sample per-user LaunchAgent ships at
[`deploy/launchd/com.aiops-platform.worker.plist`](../../deploy/launchd/com.aiops-platform.worker.plist).
launchd has no sandboxing directives equivalent to systemd's, so the
hardening here is "run as an ordinary (non-admin) user with least
filesystem footprint" plus keeping the secret out of the plist (below).
It uses the `/usr/local` paths from §3.

```bash
# install the binaries onto PATH
sudo scripts/install.sh --prefix /usr/local

# config + state/log dirs owned by the running user (the plist logs here).
# /usr/local is root-owned on a stock Mac, so create with sudo + -o "$USER".
sudo install -d -o "$(id -un)" /usr/local/etc/aiops-platform
sudo install -d -o "$(id -un)" /usr/local/var/aiops-platform
cp examples/WORKFLOW.md /usr/local/etc/aiops-platform/WORKFLOW.md   # edit tracker.api_key/kind

# install and load the agent (per-user; runs only while you are logged in)
cp deploy/launchd/com.aiops-platform.worker.plist ~/Library/LaunchAgents/
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/com.aiops-platform.worker.plist
launchctl enable "gui/$(id -u)/com.aiops-platform.worker"

# observe
launchctl print "gui/$(id -u)/com.aiops-platform.worker" | head -n 40
tail -f /usr/local/var/aiops-platform/worker.err.log
curl http://127.0.0.1:4000/api/v1/state
```

For a machine-wide daemon that runs without a login session, install the
plist into `/Library/LaunchDaemons/` instead, add a `UserName` key, and
bootstrap into the `system` domain (`sudo launchctl bootstrap system …`).

**Secrets on macOS.** launchd has no `EnvironmentFile=` equivalent, and a
token pasted into the plist's `EnvironmentVariables` is readable by the
user and lands in backups. Prefer the login Keychain plus a thin wrapper
that exports the token before exec, keeping the plist secret-free:

```bash
# 1. store the token once, keyed to the login account
security add-generic-password -a "$(id -un)" -s aiops-linear-api-key -w 'your-token'

# 2. install the wrapper into a PATH dir and make it executable. The 'EOF'
#    is quoted, so id -un / the security lookup run at launch, not now.
sudo tee /usr/local/bin/aiops-worker-launch >/dev/null <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
LINEAR_API_KEY="$(security find-generic-password -a "$(id -un)" -s aiops-linear-api-key -w)"
export LINEAR_API_KEY
exec /usr/local/bin/worker
EOF
sudo chmod 755 /usr/local/bin/aiops-worker-launch

# 3. point the agent at the wrapper instead of the bare binary, then reload
plutil -replace ProgramArguments.0 -string /usr/local/bin/aiops-worker-launch \
  ~/Library/LaunchAgents/com.aiops-platform.worker.plist
launchctl bootout "gui/$(id -u)" ~/Library/LaunchAgents/com.aiops-platform.worker.plist
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/com.aiops-platform.worker.plist
```

SSH keys on macOS need no special placement — `ProtectHome` does not
exist, so a standard `~/.ssh/id_ed25519` for the running user works.

> The existing [`scripts/install-local-launchagents.sh`](../../scripts/install-local-launchagents.sh)
> is a different, higher-level macOS flow that bundles a GitHub worker
> wrapper and a PR follow-through agent
> (see [`github-local-automation.md`](github-local-automation.md)); use
> the plist above when you just want the bare worker binary as a service.

## Common failure modes

- **`missing_tracker_api_key` at startup** — `tracker.api_key` references
  a `$VAR` that is unset. Confirm the env var is exported in the same
  environment as the worker (for systemd, that it is present in
  `EnvironmentFile`; for launchd, that the Keychain wrapper exported it).
- **Agent cannot push** — the worker does not hold git credentials;
  provide a file-backed credential (e.g. a dedicated SSH deploy key) the
  agent subprocess can read. Under the systemd unit the key must live
  inside the state dir (see §5 "SSH deploy key under hardening"), because
  `ProtectHome=true` hides keys under `/home`. See also
  [`local-dev.md`](local-dev.md) "Agent fails to push or open PRs".
- **`bwrap: setting up uid map: Permission denied` on every agent command**
  (codex-app-server) — the host restricts unprivileged user namespaces. See
  §1 "Unprivileged user namespaces"; `worker --doctor --deploy=binary
  --mode=real` catches this at preflight.
- **`/api/v1/state` refuses to bind** — the server is loopback-only and
  disabled when `server.port: -1`. Confirm with `worker --print-config`.
- **Workspace permission errors under a service manager** — ensure
  `AIOPS_WORKSPACE_ROOT` and `AIOPS_MIRROR_ROOT` point at directories the
  running user owns (the systemd unit uses `StateDirectory`; the launchd
  agent runs as your user and writes under `/usr/local/var`).

## See also

- [`local-dev.md`](local-dev.md) — the canonical local loop and the
  full failure-mode catalog.
- [`codex-app-server-docker.md`](codex-app-server-docker.md) — the
  container path for `agent.default: codex-app-server`.
- README "Quick start" and "Operator surfaces".
