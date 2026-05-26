# Codex app-server in Docker

This runbook documents the production-style worker image path for workflows
that set `agent.default: codex-app-server` and run `codex app-server` inside the
worker container.

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
