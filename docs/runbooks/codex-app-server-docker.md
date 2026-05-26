# Codex app-server in Docker

This runbook documents the production-style worker image path for workflows
that set `agent.default: codex-app-server` and run `codex app-server` inside the
worker container.

For the full new-operator path, including support matrix, Docker Compose
secrets, `worker --doctor`, and the todo smoke test, start with
[`first-run-docker-linear-codex.md`](first-run-docker-linear-codex.md).

## Current installer status

On 2026-05-26, the online installer failed inside the Linux ARM64 worker image:

```bash
curl -fsSL https://chatgpt.com/codex/install.sh | sh
```

Observed:

```text
==> Installing Codex CLI
==> Detected platform: Linux (ARM64)
==> Resolved version: 0.133.0
==> Downloading Codex CLI
Could not find SHA-256 digest for codex-package-aarch64-unknown-linux-musl.tar.gz in codex-package_SHA256SUMS.
```

Until the installer works for this platform, use a deterministic release-package
install in the worker image.

The repository Dockerfile now exposes this as the `codex-worker` target:

```bash
docker build --target codex-worker -t aiops-platform:codex-worker .
```

The target supports Linux `amd64` and `arm64`, pins Codex CLI `0.133.0`, checks
the release artifact SHA-256, and runs `codex --version` during the build.

## Linux ARM64 fallback

The 2026-05-26 validation used Codex CLI `0.133.0`:

```bash
curl -fL -o /tmp/codex.tar.gz \
  https://github.com/openai/codex/releases/download/rust-v0.133.0/codex-package-aarch64-unknown-linux-musl.tar.gz
echo '7a77d416f9ce16f18e09fdc57622a15aab6ad131c34e078ab9d55a13bb3d9b05  /tmp/codex.tar.gz' | sha256sum -c -
mkdir -p /opt/codex
tar -xzf /tmp/codex.tar.gz -C /opt/codex
ln -sf /opt/codex/codex-aarch64-unknown-linux-musl /usr/local/bin/codex
codex --version
```

Expected version output:

```text
codex-cli 0.133.0
```

Keep the version and checksum together in any derived Dockerfile so future
upgrades are reviewable.

## Protocol schema upgrades

`codex app-server` has its own versioned wire protocol. The Symphony Elixir
config schema is useful for organizing workflow settings, but it is not the
authority for the Codex `turn/start` JSON shape. For each Codex CLI upgrade,
refresh the local minimal schema snapshot from the matching upstream Codex
app-server protocol schema before running live Linear validation.

The runner-side contract currently covers the fields aiops-platform emits:

- `TurnStartParams.sandboxPolicy`
- `UserInput.Text` with `text_elements`
- `SandboxPolicy` variants such as `workspaceWrite`

Run the schema contract tests before any real issue run:

```bash
go test ./internal/workflow -run 'Codex.*Sandbox|Schema' -count=1
go test ./internal/runner -run 'CodexAppServer.*TurnStart|Schema' -count=1
```

If either test fails after a Codex upgrade, update the typed Go structs and the
schema snapshot first. Do not add fallback translation for old sandbox fields
such as `mode`, `access`, or `readOnlyAccess`; this project is pre-release and
should fail fast on stale workflow shape.

## Authentication mount

`codex app-server` must be authenticated before the worker starts it. For a
local validation image, mount a temporary Codex home copied from a host session:

```bash
mkdir -p /tmp/aiops-codex-home/.codex
cp ~/.codex/auth.json /tmp/aiops-codex-home/.codex/auth.json
cp ~/.codex/config.toml /tmp/aiops-codex-home/.codex/config.toml
```

Mount that directory read-only or from a secret store at `/home/aiops/.codex`
for the default non-root worker image, and set:

```text
CODEX_HOME=/home/aiops/.codex
```

Validate inside the container before processing issues:

```bash
codex login status
codex app-server
```

`codex login status` should report a logged-in account, and `codex app-server`
should remain running on stdio until the worker speaks JSON-RPC to it.
`worker --doctor --mode=real` performs that stdio probe by keeping stdin open,
sending `initialize`, and waiting for a JSON-RPC response. A probe that only
starts the process and closes stdin is not a valid app-server check.

## Sandbox note

In the 2026-05-26 Docker Desktop ARM64 validation, Codex
`thread_sandbox: workspace-write` failed inside the already-restricted worker
container with:

```text
bwrap: No permissions to create a new namespace
```

For that validation, the workflow used `thread_sandbox: danger-full-access`
inside the Docker-isolated worker container. Do not copy that setting onto a
shared host. If the worker runs directly on a host instead of inside a container,
prefer Codex's normal sandbox and approval policy.

## Validation reference

See `docs/validation/2026-05-26-docker-linear-e2e.md` for the live Linear todo
run that completed `AIS-18` and `AIS-19` through real `codex app-server` and
agent-side `linear_graphql` writes.
