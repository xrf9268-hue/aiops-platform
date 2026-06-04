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

The target supports Linux `amd64` and `arm64`, pins Codex CLI `0.136.0`, checks
the release artifact SHA-256, and runs `codex --version` during the build.

## Linux ARM64 fallback

Install the pinned Codex CLI (`0.136.0`) directly when the official installer
cannot resolve the ARM64 package:

```bash
curl -fL -o /tmp/codex.tar.gz \
  https://github.com/openai/codex/releases/download/rust-v0.136.0/codex-package-aarch64-unknown-linux-musl.tar.gz
echo '2f332f07b4019bef87a844d4a8c3f4fae268912c5a6e50fd8a0388b61d125d15  /tmp/codex.tar.gz' | sha256sum -c -
mkdir -p /opt/codex
tar -xzf /tmp/codex.tar.gz -C /opt/codex
ln -sf /opt/codex/codex-aarch64-unknown-linux-musl /usr/local/bin/codex
codex --version
```

Expected version output:

```text
codex-cli 0.136.0
```

Keep the version and checksum together in any derived Dockerfile so future
upgrades are reviewable.

## Protocol schema upgrades

`codex app-server` has its own versioned wire protocol. The Symphony Elixir
config schema is useful for organizing workflow settings, but it is not the
authority for the Codex request JSON shapes. The authority is the schema the
matching Codex binary generates: it is vendored under `internal/runner/testdata/`
and validated against the exact request payloads the runner sends
(`internal/runner/codex_app_server_schema_test.go`).

For each Codex CLI upgrade, **regenerate** the vendored schema — never hand-edit
it (hand-curation is what rotted the previous snapshot into a placebo):

```bash
scripts/refresh-codex-schema.sh
```

The script regenerates the bundle with `codex app-server generate-json-schema
--out <dir> --experimental`, removes the prior vendored bundle, and prints the
new release SHA-256 plus the exact pins to update. The `--experimental` flag is
**mandatory**: the runner enables the experimental API
(`initialize` `capabilities.experimentalApi`) and sends experimental request
fields such as `thread/start` `dynamicTools`, which the default export strips —
validating against a non-experimental bundle would falsely reject them.

`CodexProtocolVersion` (in `internal/runner/codex_version.go`) is the single
source of truth; the Dockerfile `ARG CODEX_CLI_VERSION` + per-arch `codex_sha`,
the e2e Dockerfile assertion, and the vendored schema filename are pinned to it.
After updating the pins, both tests must pass:

```bash
go test ./internal/runner -run 'CodexAppServer.*Schema|CodexVersionPinParity' -count=1
```

`TestCodexVersionPinParity` fails if the constant, the Dockerfile pin, and the
vendored schema filename disagree. The schema contract tests fail if any request
payload the runner sends violates the generated schema — a missing required
field, a bad enum, or a stale/unknown field (the removed `turn/start` `title` was
one such field). Do not add fallback translation for old shapes such as `mode`,
`access`, or `readOnlyAccess`; this project is pre-release and should fail fast
on stale wire shape.

## Production auth and model configuration

`codex app-server` must be authenticated before the worker starts it. This
section is the production operator contract for **identity** and **model
configuration** (#465). The separate skills/MCP/toolchain bundle is #464; see
"Composition with #464" below for the boundary.

### Supported auth modes

Two modes are supported. Pick one per deployment; do not mix them.

1. **ChatGPT/Codex login (default).** Codex stores the login in
   `$CODEX_HOME/auth.json`. This file is a secret and is **writable at runtime**
   because Codex 0.136 refreshes tokens in place. Set it up once with
   `codex --login` in the same container user / `CODEX_HOME` context, then keep
   `CODEX_HOME` on a restricted writable volume so refreshed tokens persist
   across restarts. A read-only `CODEX_HOME` silently breaks long-lived workers
   the first time a token needs refreshing.

2. **Model API key.** Codex authenticates with `OPENAI_API_KEY` (or the
   `env_key` named by a custom `[model_providers.*]` block). The worker does
   **not** pass model-runtime credentials to the agent subprocess unless the
   workflow opts in: add `OPENAI_API_KEY` to `codex.env_passthrough` in
   `WORKFLOW.md`. Source the value from a Docker secret / secret file and let the
   entrypoint export it; never put it on a command line or in a git-tracked file.
   Tracker/repo tokens (`LINEAR_API_KEY`, `GITHUB_TOKEN`, …) stay denied from
   passthrough by policy — only the model key is opt-in.

`worker --doctor --mode=real` reports the active mode as `Codex auth mode`:
`API key via OPENAI_API_KEY passthrough` when the passthrough is configured (and
fails if the key resolves empty), otherwise `ChatGPT/Codex login` plus a
`Codex auth refresh` warning if `CODEX_HOME` is not writable.

### CODEX_HOME volume layout

Treat `CODEX_HOME` as three concerns with different trust levels:

| Path | Trust | Mount |
| --- | --- | --- |
| `auth.json` | secret, mutable | inside the **writable** `CODEX_HOME` volume; never commit |
| `config.toml` | non-secret, declarative | **read-only** bind from a version-controlled file (see below) |
| `sessions/`, `log/`, cache | ephemeral | writable; safe to discard between runs |

For Codex CLI 0.136, mount the writable home at `/home/aiops/.codex` for the
default non-root worker image, owned by the worker UID/GID, and set:

```text
CODEX_HOME=/home/aiops/.codex
```

Codex resolves its home from `CODEX_HOME` if set, otherwise `$HOME/.codex`. The
worker passes `HOME` to the agent subprocess but **not** `CODEX_HOME`; if you
point `CODEX_HOME` somewhere other than `$HOME/.codex`, also add `CODEX_HOME` to
`codex.env_passthrough` so the agent sees the same home the preflight checked.

### Declarative model configuration

Do not copy an opaque host `config.toml` into the image or the writable home.
Instead keep a small, non-secret, version-controlled config and mount it
read-only over `$CODEX_HOME/config.toml`. The repo ships
[`deploy/codex/config.toml`](../../deploy/codex/config.toml) as the example:

```toml
model = "gpt-5-codex"
model_provider = "openai"
model_reasoning_effort = "high"
```

Enable it by uncommenting the optional bind in
`deploy/docker-compose.codex.yml` and pointing `AIOPS_CODEX_CONFIG_FILE` at an
absolute path:

```text
AIOPS_CODEX_CONFIG_FILE=$PWD/deploy/codex/config.toml
```

`worker --doctor --mode=real` reads the three top-level model keys (and nothing
from `[model_providers.*]` tables, which may name secret env keys) and reports
them as `Codex model config: model=…, provider=…, reasoning_effort=…`. If no
`model` is declared it warns rather than silently inheriting Codex's built-in
default, so model selection is always explicit and auditable in review.

Provider, profile, service tier, reasoning effort, and feature flags all belong
in this tracked file or in `WORKFLOW.md` front matter (`codex.thread_sandbox`,
`codex.turn_sandbox_policy`, `codex.approval_policy`, the `codex.*_timeout_ms`
fields), never in operator-local state baked into the image.

### Rotation, refresh, and revocation

- **ChatGPT login refresh** happens automatically as long as `CODEX_HOME` is
  writable; no operator action is needed mid-deployment.
- **Re-login / rotation:** run `codex --login` again in the container user /
  `CODEX_HOME` context (or re-provision the `auth.json` on the writable volume),
  then re-run `worker --doctor --mode=real`. No image rebuild is required.
- **API-key rotation:** replace the Docker secret file contents and recreate the
  worker so the entrypoint re-exports the new value; nothing is committed.
- **Revocation:** revoke the ChatGPT session or API key at the provider, then
  delete `auth.json` (or clear the secret) on the volume. `codex login status`
  and `worker --doctor --mode=real` will then fail closed with an actionable
  `Codex auth` failure instead of running with stale credentials.

### Validate before processing issues

```bash
codex login status
codex app-server
```

`codex login status` should report a logged-in account, and `codex app-server`
should remain running on stdio until the worker speaks JSON-RPC to it.
`worker --doctor --mode=real` performs that stdio probe by keeping stdin open,
sending `initialize`, and waiting for a JSON-RPC response. A probe that only
starts the process and closes stdin is not a valid app-server check.

### Doctor remediation reference

| Doctor line | Meaning | Remediation |
| --- | --- | --- |
| `FAIL Codex auth` | `codex login status` failed or returned not-logged-in | Run `codex --login` (or re-provision `auth.json`) in the same `CODEX_HOME`/container user context. |
| `FAIL Codex auth mode` | `OPENAI_API_KEY` is in `codex.env_passthrough` but resolves empty | Mount the model API key from a Docker secret into `OPENAI_API_KEY`; never pass it on a command line. |
| `WARN Codex auth refresh` | ChatGPT-login mode but `CODEX_HOME` is not writable | Mount `CODEX_HOME` as a restricted writable volume so token refresh can persist. |
| `WARN Codex model config` | no `model` declared in `$CODEX_HOME/config.toml` | Declare `model` (and optional `model_provider`/`model_reasoning_effort`) in the tracked `config.toml`. |
| `PASS Codex model config` | resolved model selection | none; verify the reported `model`/`provider`/`reasoning_effort` match intent. |
| `FAIL Codex app-server` | app-server did not answer the JSON-RPC `initialize` probe | Check `CODEX_HOME`, `codex.command`, and app-server support in the installed Codex version. |

### Composition with #464

This issue covers credentials and model configuration only. The agent's
**toolchain** — repo/Codex skills, plugins, MCP servers, and the `gh`/Go
command surface — is the separate #464 contract. Both share the same principle:
secrets stay in Docker secrets / secret files and writable volumes, while
reproducible, non-secret assets (this `config.toml`, the #464 skills/MCP
manifest) are version-controlled and verified by `worker --doctor`. The auth
modes here do not install skills or MCP servers; do not rely on a host `~/.codex`
to supply them.

## GitHub agent credentials

Dogfood workflows that expect the agent shell to run `gh` or push GitHub
branches need credentials visible from the agent environment. Worker process
variables such as `GH_TOKEN` and `GITHUB_TOKEN` are stripped before Codex
subprocesses start, so they are not a supported credential contract.

The Docker overlay supports file-backed `gh` auth for the unprivileged `aiops`
user:

```bash
printf '%s' 'replace-with-least-privilege-github-token' > .aiops/secrets/github_token
chmod 600 .aiops/secrets/github_token
```

```text
GITHUB_TOKEN_FILE=$PWD/.aiops/secrets/github_token
```

The entrypoint writes `/home/aiops/.config/gh/hosts.yml` with `0600`
permissions, clears plain GitHub token env, and runs `gh auth setup-git`. The
`codex-worker` Dockerfile target installs the GitHub CLI alongside Codex so
`worker --doctor --github-issue` and runtime `git push` can both use the
file-backed credential; under that image the `command -v gh` guard in the
entrypoint always passes. The plain `worker` target does not ship `gh` and
that guard stays a no-op there, by design. Keep the token least-privilege
and never commit `.aiops/secrets`, `hosts.yml`, or copied credential files.
For deploy-key installs, mount the dedicated key and `known_hosts` from
`deploy/ssh/README.md` and use an SSH `repo.clone_url`.

Validate the exact agent environment before moving real issues into an active
state:

```bash
docker compose --env-file .env \
  -f deploy/docker-compose.yml \
  -f deploy/docker-compose.codex.yml \
  run --rm -v "$PWD:/repo:ro" worker \
    --doctor --mode=real --go-test-dir=/repo --github-issue 451 \
    --github-repo xrf9268-hue/aiops-platform
```

With `--go-test-dir`, doctor verifies this repo's Go toolchain by running
`go version`, `gofmt`, and a targeted `go test` from the mounted module root.
With `--github-issue`, doctor runs `gh issue view` and `git push --dry-run`
using the Codex agent environment. Omit `--github-repo` only when the workflow
configures one GitHub repo. A failure is a deployment blocker; fix the
toolchain or credential path before starting the worker.

The `CODEX_HOME` volume trust layout (writable home, read-only declarative
`config.toml`, ephemeral cache) is covered under "Production auth and model
configuration" above.

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

When `codex.turn_sandbox_policy` is left unset, the per-turn `sandboxPolicy` is
derived from `thread_sandbox` (`danger-full-access` → `dangerFullAccess`,
`read-only` → `readOnly`, otherwise `workspaceWrite`), so the container setting
above governs every turn — you no longer have to restate it under
`turn_sandbox_policy`. Setting `turn_sandbox_policy` explicitly still overrides
the derived value for callers that need a different per-turn policy (#472).

## Validation reference

See `docs/validation/2026-05-26-docker-linear-e2e.md` for the live Linear todo
run that completed `AIS-18` and `AIS-19` through real `codex app-server` and
agent-side `linear_graphql` writes.
