# aiops-platform

A personal-productivity AI coding orchestrator: a single Go worker that watches
a tracker (Linear, Gitea, or GitHub), turns eligible issues into deterministic
Git workspaces, runs a coding agent in each, and lets the agent hand the
branch/PR back through its own tools.

```text
Linear, Gitea, or GitHub issue
  -> cmd/worker  (poll + reconcile + dispatch)
  -> deterministic Git workspace
  -> WORKFLOW.md config front matter + prompt
  -> mock / Codex / Claude runner
  -> agent-owned verify + branch push + PR + tracker write-back
```

It is a Go implementation of [OpenAI Symphony](https://github.com/openai/symphony).
The [`SPEC.md`](docs/research/SPEC.md) contract — mirrored verbatim into this
repo from
[upstream](https://github.com/openai/symphony/blob/main/SPEC.md) so it cannot
drift (upstream is an unmaintained demo repo) — is authoritative; the Elixir
reference implementation is the tie-breaker when the SPEC text is ambiguous. Why
we continue the Go port here rather than forking is recorded in
[`DECISION.md`](DECISION.md); the current SPEC deviation ledger lives in
[`DEVIATIONS.md`](DEVIATIONS.md).

The goal is a practical loop, not a heavy enterprise platform: run `cmd/worker`
as a Go-based, Gitea-friendly, locally customizable Symphony while the open
items in [`DEVIATIONS.md`](DEVIATIONS.md) are closed systematically.

## License

aiops-platform is licensed under the [Apache License 2.0](LICENSE).

## Components

### Binaries (`cmd/`)

- **`cmd/worker`** — the primary process. Polls the configured tracker,
  reconciles startup workspaces, owns the single in-memory orchestrator runtime
  state, dispatches eligible issues, runs the Symphony workflow, and serves the
  loopback HTTP state API plus the embedded web dashboard. No Postgres required.
- **`cmd/tui`** — terminal status dashboard. Polls the worker's
  `/api/v1/state` and redraws each tick, mirroring the upstream Elixir
  `SymphonyElixir.StatusDashboard`.

### Web dashboard (`cmd/worker/dashboard`)

A React 19 + Vite single-page app with self-contained, embedded CSS (no
Tailwind, no external font/CDN), served by the worker at `/`
(and assets under `/assets/`) on the same loopback listener as the state API.
It is a read-only operator client for `/api/v1/state` (SPEC §13.7.1) with
light/dark themes and Anthropic brand styling. The built output is generated in
`cmd/worker/dashboard/dist` before compiling the worker and is embedded via
`//go:embed` — so the shipped worker binary needs no Node toolchain. See
[the dashboard design note](docs/design/dashboard-brand-redesign.md) for the
brand/UX rationale.

### Packages (`internal/`)

- `internal/orchestrator` — single in-memory runtime state, serialized dispatch
  authority, retry bookkeeping, restart recovery, and the worker spawn bridge.
- `internal/worker` — per-task run loop wiring workflow resolution, workspace
  prep, prompt directives, and runner invocation.
- `internal/workflow` — loads the repo-owned `WORKFLOW.md` configuration and
  prompt body, with defaults and normalization.
- `internal/tracker` — tracker abstraction with Linear and GitHub clients.
- `internal/gitea` — Gitea tracker client plus the label-state helpers behind
  the agent-side `gitea_issue_labels` tool.
- `internal/runner` — runner abstraction for `mock`, `codex-app-server`, and `claude`.
- `internal/workspace` — deterministic Git workspace management, task files, and
  cached artifacts.
- `internal/task` — task model and shared types.

## Quick start: worker-owned tracker polling path

Start from the example workflow for your tracker —
[`examples/WORKFLOW.md`](examples/WORKFLOW.md) (Linear),
[`examples/gitea-WORKFLOW.md`](examples/gitea-WORKFLOW.md) (Gitea), or
[`examples/github-local-WORKFLOW.md`](examples/github-local-WORKFLOW.md)
(GitHub) — edit your repository and tracker settings, then run
the worker. It reconciles workspaces on startup before the first poll tick, then
repeatedly fetches active issues and dispatches them through the in-memory
orchestrator state:

```bash
# Point AIOPS_WORKFLOW_PATH at the template you picked above:
export AIOPS_WORKFLOW_PATH=$PWD/examples/WORKFLOW.md                 # Linear
# export AIOPS_WORKFLOW_PATH=$PWD/examples/gitea-WORKFLOW.md         # Gitea
# export AIOPS_WORKFLOW_PATH=$PWD/examples/github-local-WORKFLOW.md  # GitHub
export AIOPS_WORKSPACE_ROOT=$PWD/.aiops/workspaces

# For tracker.kind: linear  (consumed via WORKFLOW.md "api_key: $LINEAR_API_KEY")
export LINEAR_API_KEY=your-linear-personal-key
# Set tracker.project_slug in WORKFLOW.md to the Linear project slugId.
# Example: a Linear project URL ending in /project/aiops-platform-abc123
# uses project_slug: aiops-platform-abc123.

# For tracker.kind: gitea  (consumed via WORKFLOW.md "api_key: $GITEA_TOKEN")
# Set tracker.endpoint in WORKFLOW.md to the Gitea base URL — a literal URL
# or "endpoint: $GITEA_BASE_URL"; the bare export below is only the runtime
# fallback when tracker.endpoint is empty.
export GITEA_BASE_URL=https://gitea.example.com
export GITEA_TOKEN=your-gitea-bot-token

# For tracker.kind: github  (consumed via WORKFLOW.md "api_key: $GITHUB_TOKEN")
export GITHUB_TOKEN=$(gh auth token -h github.com)

go run ./cmd/worker
```

The worker never reads these tracker tokens directly: a token reaches it only
when `tracker.api_key` in the selected `WORKFLOW.md` references the variable as
the entire field value (`$VAR` / `${VAR}`). The shipped examples already wire
the right variable per tracker kind — `examples/WORKFLOW.md`
(`api_key: $LINEAR_API_KEY`), `examples/gitea-WORKFLOW.md`
(`api_key: $GITEA_TOKEN`), `examples/github-local-WORKFLOW.md`
(`api_key: $GITHUB_TOKEN`). Exporting a token without that `api_key` line
leaves the worker unauthenticated against the tracker.

`WORKFLOW.md` front matter is the source of truth for runtime workspace
placement: when `workspace.root` is set in the selected workflow, the worker
creates and reconciles task workspaces under that path. `AIOPS_WORKSPACE_ROOT`
(legacy alias: `WORKSPACE_ROOT`) is only the fallback for workflows that omit
`workspace.root`.

No `DATABASE_URL` or Postgres service is required. Restart recovery follows
SPEC §14.3: the worker starts with fresh runtime state, cleans terminal
workspaces from tracker state, and re-dispatches eligible active issues on the
next poll rather than restoring queue rows, retry timers, or running sessions
from a database.

The worker is a self-contained binary with no container runtime dependency.
To run it directly under a service manager instead of Compose — build/install,
`worker --doctor` preflight, and Linux (systemd) / macOS (launchd) samples —
see the [binary deployment runbook](docs/runbooks/binary-deployment.md).

Prebuilt Linux/macOS (amd64/arm64) archives are attached to each
[GitHub Release](https://github.com/xrf9268-hue/aiops-platform/releases) as
`aiops-platform_<tag>_<os>_<arch>.tar.gz`, bundling the `worker` and `tui`
binaries plus `examples/WORKFLOW.md` and `.env.example`. Verify a download with
either `gh attestation verify <archive> --repo xrf9268-hue/aiops-platform`
(build provenance) or `sha256sum --ignore-missing -c
aiops-platform_<tag>_SHA256SUMS` (plain checksums; `--ignore-missing` checks
only the archives you downloaded, since the file lists every artifact) — both
are published with the release.

Each tagged release publishes prebuilt **linux/amd64** images to GHCR, so you can
run without a source checkout (arm64 hosts run them under emulation, or
`--build` a native image):

```bash
docker pull ghcr.io/xrf9268-hue/aiops-platform-worker:latest
# Codex-enabled agent runtime: ghcr.io/xrf9268-hue/aiops-platform-codex-worker:latest
```

Public packages pull without auth; if a package is still private, run
`docker login ghcr.io` first (or `--build` from source instead). The published
image runs as UID 1000 — if your host `id -u` differs and you use the Compose
0600 SSH-key / Codex-home bind mounts, prefer `--build` (it aligns the container
user via `AIOPS_UID`/`AIOPS_GID`), which the pulled image can't do.

The default Compose service starts `worker` and references that image, falling
back to a local build with `--build`:

```bash
docker compose --env-file .env -f deploy/docker-compose.yml pull worker        # use the published image
docker compose --env-file .env -f deploy/docker-compose.yml up --build worker   # or build locally
```

The image defaults to the worker (`CMD ["worker"]`), so a plain `docker run`
starts it. Mount your workflow file in (as Compose does):

```bash
docker build -t aiops-platform .

# Worker (default CMD); mount a workflow and point the worker at it:
docker run --rm --env-file .env \
  -v "$PWD/examples:/app/examples:ro" \
  -e AIOPS_WORKFLOW_PATH=/app/examples/WORKFLOW.md \
  aiops-platform

```

For an operator walkthrough — workflow file layout, the `/api/v1/state` and
`--print-config` smoke checks — see the [local development
runbook](docs/runbooks/local-dev.md). If the runbook and this README ever
diverge, **this README is canonical**.

For workflow-authoring patterns — including the repo-owned `LEARNINGS.md`
cross-run memory convention (read-before-plan, verified-facts-only entries
reviewed inside PRs) — see the [workflow authoring
runbook](docs/runbooks/workflow-authoring.md).

## Operator surfaces

The worker binds an HTTP server at `<server.host>:<server.port>`, defaulting to
the private loopback `127.0.0.1:4000`. Override the bind host with `server.host`
in `WORKFLOW.md` or the `AIOPS_SERVER_HOST` environment variable (a blank value
keeps the loopback default). The same listener serves the JSON state API and the
web dashboard:

| Path | Purpose |
|------|---------|
| `GET /api/v1/state` | SPEC §13.7 state snapshot (the canonical data source). |
| `POST /api/v1/refresh` | Forces a state refresh where the runtime supports it. Requires the `X-AIOPS-Refresh: true` header; non-POST methods get `405`. |
| `GET /api/v1/{issue}` | Per-issue debug snapshot — see the [runtime debugging API runbook](docs/runbooks/task-api.md). |
| `GET /` | The embedded web dashboard (HTML). |
| `GET /assets/…` | Static dashboard assets. |
| `GET /livez` | Unauthenticated liveness probe. Returns `ok` when the HTTP listener can serve requests. |
| `GET /readyz` | Unauthenticated readiness probe. Returns `ok` once the worker has loaded workflow config, constructed the tracker client, and completed startup reconciliation. |

When `AIOPS_STATE_API_TOKEN` is set, every request must authenticate with either
`Authorization: Bearer <token>` or HTTP Basic auth user `aiops` and the token as
the password. Without that token, the server accepts unauthenticated requests
only when both the Host header and TCP peer are loopback; non-loopback peers
fail closed. Set `server.port: -1` to disable the listener entirely (e.g. when
you provide your own state bridge). If the configured listener cannot start, the
worker logs the failure, continues without the HTTP surface, and retries on
later workflow-reload checks until the bind succeeds or `server.port` changes.
The `/livez` and `/readyz` probes intentionally bypass this auth guard and
return no runtime state or agent text, so Docker and Compose can use them
without a dashboard token. `/readyz` returns `503` until startup readiness has
been marked after workflow load, tracker client construction, and startup
reconciliation. The default container health check probes `/livez`; use
`/readyz` for orchestrators that distinguish startup/readiness from liveness.
If you change the worker port in Compose, set `AIOPS_HEALTHCHECK_PORT` to match;
if you set `server.port: -1`, disable the container health check as well.

Under Docker Compose the default loopback bind is unreachable from the host
(Docker publishes ports to the container interface, not its loopback). Merge the
opt-in overlay to reach the dashboard from the host:

```bash
mkdir -p .aiops/secrets
# Create the token with 0600 so co-tenant users/processes cannot read it; the
# subshell scopes umask 077 to this write and never exposes a 0644 window.
( umask 077; openssl rand -hex 24 > .aiops/secrets/state_api_token )
AIOPS_STATE_API_TOKEN_FILE=$PWD/.aiops/secrets/state_api_token \
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.dashboard.yml up worker
```

The overlay sets `AIOPS_SERVER_HOST=0.0.0.0` (bind all interfaces *inside* the
container) but publishes only to host loopback (`127.0.0.1:4000:4000`), so the
host trust boundary stays the loopback. Docker port publishing reaches the
container from a bridge peer rather than container loopback, so the overlay
requires `AIOPS_STATE_API_TOKEN_FILE`; browsers will receive a Basic-auth challenge
and should use username `aiops` plus that token. Open the plain dashboard URL
(`http://127.0.0.1:4000/`) and let the browser prompt for credentials; avoid
sharing or bookmarking URLs with embedded credentials. The dashboard strips URL
credentials from its own state API fetches, but the plain challenge flow avoids
leaking the token through browser history or screenshots. The overlay also
moves the worker onto a dedicated `dashboard` bridge network. Do **not** publish
on a routable host interface or attach untrusted containers to the dashboard
network unless they should be able to use the token-protected status surface.

**Treat the status surface as live agent text even though it binds to loopback.**
Per SPEC §15.3, each running row's `last_message` is a passthrough of the most
recent Codex notification and may include echoed issue body text,
`linear_graphql` tool responses, or tool output. The worker truncates the field
to 256 runes and pattern-scrubs common token shapes (Authorization headers,
bearer tokens, `sk-`/`ghp_`-prefixed keys, embedded basic-auth URLs) before
serializing, but loopback is **not** a trust boundary on multi-tenant hosts:
co-tenant containers, sidecars, and any local process can read the field. Treat
screenshots, dashboard caches, and chat pastes accordingly.

### Web dashboard

Open `http://127.0.0.1:4000/` (or your `server.port`) while the worker is
running. It renders the orchestrator state — running/blocked/retrying tasks,
metrics, and rate limits — as a read-only client of `/api/v1/state`.

To rebuild the dashboard after changing its source (requires Node):

```bash
cd cmd/worker/dashboard
npm install
npm run build     # writes ignored dashboard/dist for Go embed
npm test          # vitest
```

### Terminal UI

`cmd/tui` renders the same state in a terminal, polling the worker over HTTP:

```bash
go run ./cmd/tui                          # defaults to http://127.0.0.1:4000/, 5s interval
go run ./cmd/tui --url http://127.0.0.1:4000/ --interval 5s
go run ./cmd/tui --raw                    # disable alt-screen/cursor mgmt (upstream parity)
```

When the worker requires state API auth (for example the Docker dashboard
overlay), set `AIOPS_STATE_API_TOKEN` in the TUI environment; the client sends
it as a bearer token.

## WORKFLOW.md configuration

The worker resolves one canonical workflow source: `WORKFLOW.md` in the
service/repository root, or an explicit startup workflow path where supported.
Legacy fallback files such as `.aiops/WORKFLOW.md` and `.github/WORKFLOW.md` are
not searched and are not reported as shadowed workflow sources.

If the canonical file does not exist, the worker proceeds with built-in
defaults. The table below mirrors SPEC §6.4's cheat-sheet so a SPEC reader's
mental model lines up with `worker --print-config` output; defaults that diverge
from SPEC are called out and tracked in [`DEVIATIONS.md`](DEVIATIONS.md). It is
deliberately partial — the exhaustive key-by-key reference (every front-matter
key with type, default, behavior, and validation rule) is
[`docs/runbooks/workflow-frontmatter-reference.md`](docs/runbooks/workflow-frontmatter-reference.md):

| Setting | Default | Source |
|---------|---------|--------|
| `agent.default` | `mock` | implementation (SPEC defers to operator) |
| `agent.max_concurrent_agents` | `10` | SPEC §6.4 |
| `agent.max_turns` | `20` per-session turn budget — the codex app-server runner's in-session loop (SPEC §5.3.5) | SPEC §6.4 |
| `agent.max_continuation_turns` | `agent.max_turns` (default `20`) issue-level clean-turn budget across fresh and continuation dispatches. Each dispatch receives the remaining clean-turn budget, capped again by `agent.max_turns`; reaching the budget parks the issue in local `blocked` state (`continuation_budget`) instead of looping forever. Raising the value later does not automatically redrive existing blocked claims. | implementation (accepted deviation D34 / #621) |
| `agent.timeout` | `30m` | implementation (#215) |
| `codex.command` | `codex app-server` | SPEC §6.4 |
| `codex.env_passthrough` / `claude.env_passthrough` | none beyond runner baseline (`PATH`, `HOME`, `USER`, locale, `TZ`, `TERM`); use for model CLI auth/proxy/CA vars, not tracker/repo API tokens | implementation (#384) |
| `server.port` | `4000` (`-1` disables the HTTP state server + dashboard) | implementation |
| `policy.mode` | `draft_pr` (or `analysis_only`) | implementation |
| `tracker.kind` | none — REQUIRED per SPEC §6.4; the loader rejects an empty value with an error that names the field and the allowed set (`gitea`, `github`, `linear`) | SPEC §6.4 |
| `tracker.endpoint` | Linear defaults to `https://api.linear.app/graphql`; Gitea/GitHub use this as the REST API base URL, with env fallbacks only when omitted | SPEC §6.4 / implementation |
| `tracker.project_slug` | required for `tracker.kind: linear` | SPEC §6.4 |
| `tracker.active_states` | `[Todo, In Progress]` | SPEC §6.4 |
| `tracker.terminal_states` | `[Closed, Cancelled, Canceled, Duplicate, Done]` | SPEC §6.4 |
| `tracker.required_labels` | `[]` (gate off) — opt-in dispatch filter: an issue must carry every listed label (matched case-insensitively after trimming) to dispatch or keep running. Removing a required label makes a running agent self-stop after its current turn (per-turn refresh) and releases retry/blocked work on the next poll. A blank entry matches no issue. Labels are projected up to the Linear API's 250-per-issue page maximum; a required label beyond that window is outside the gate's evidence (an issue carrying 250+ labels is pathological — keep the marker set small). | SPEC §4.1.1 / §6.4 |
| `tracker.pagination_max_pages` | adapter default (`github`: 10 pages; `gitea`: 20 pages; `linear`: 200 pages) | implementation |
| `workspace.root` | `<system-temp>/symphony_workspaces` (resolved via `os.TempDir()` at startup, typically `/tmp/symphony_workspaces` on Linux; per-boot — set explicitly to a long-lived path for persistence) | SPEC §6.4 |
| `verify.commands` | none — surfaced to the agent's prompt as its own pre-handoff responsibility; the worker does not run them (SPEC §1 agent boundary) | implementation |

Operators who want the historical personal-profile values —
`agent.max_concurrent_agents: 1`, `codex.command: codex app-server`,
`workspace.root: ~/aiops-workspaces/personal`, the Linear-vocabulary state
lists — copy [`examples/WORKFLOW.md`](examples/WORKFLOW.md) and declare them
explicitly. That example pins every divergent value so a SPEC reader can see the
personal-profile envelope without reading source.

When present, `hooks.timeout_ms` must be a positive integer. Omit it to use the
default `60000` ms timeout.

A `WORKFLOW.md` with no YAML front matter (just a prompt body) is supported: the
body becomes the prompt template and all other settings fall through to the
defaults above. The `workflow_resolved` event records this as
`source: prompt_only` so an operator can tell apart "ran with full Symphony
config" from "ran with body-only template".

### `verify.commands`

Per SPEC §1 the orchestrator is a scheduler/runner, not a verifier: running the
checks is the coding agent's responsibility. `verify.commands` are no longer run
by the worker. Instead they are appended to the rendered prompt as the agent's
own pre-handoff contract — the agent runs them in the workspace and fixes the
code until they pass before opening a PR or moving the issue to a review state.
PR CI is the backstop. There is no `verify_end`/`verify_start` task event.

### Inspecting effective config

To inspect the effective configuration for a workdir without consuming a task:

```bash
worker --print-config /path/to/repo/clone
# pass --port to see how a CLI override is attributed:
worker --print-config /path/to/repo/clone --port=4001
```

The output is JSON. `tracker.api_key` is masked as `***`; the prompt template is
summarized (length + first line) rather than printed verbatim — `cat
<resolution.path>` to see the full body. For post-hoc inspection, the
`workflow_resolved` task event records the source and path of every run;
`shadowed_by` is omitted unless future non-legacy resolution metadata is added.

A `provenance` block reports where each multi-layer value resolved from —
`default`, `env` (with the env var name and whether it was a deprecated
unprefixed alias), `workflow`, or `cli`. It currently covers the workspace root
(`AIOPS_WORKSPACE_ROOT` / legacy `WORKSPACE_ROOT` / `workspace.root`), the mirror
root (`AIOPS_MIRROR_ROOT`), `server.port` (including the `--port` override), and
the workflow path source. The `provenance` values are the effective ones the
worker would actually use, so they reflect env/CLI overrides that the masked
`config` block (the raw WORKFLOW.md/default values) does not.

## Runner modes and first safe mode

Start with the mock runner:

```yaml
agent:
  default: mock
```

For ambiguous or high-risk work, keep the workflow in analysis-only mode until
operators have reviewed the plan:

```yaml
agent:
  default: mock # or codex-app-server/claude after the workflow is trusted
policy:
  mode: analysis_only
```

Analysis-only mode asks the agent to produce an assessment artifact such as
`.aiops/PLAN.md` without relying on the
worker to commit, push, open PRs, or post tracker comments. If the plan needs to
be posted back to a tracker, that handoff belongs to the agent-side tool surface
(for example `linear_graphql` when configured), not worker-side tracker writes.
Use a normal implementation mode such as `policy.mode: draft_pr` only when the
agent should make code changes and manage PR handoff through its workflow/tools.

After the mock loop is trusted, switch to a real runner:

```yaml
agent:
  default: codex-app-server
policy:
  mode: draft_pr
```

## Gitea issue-state labels

For SPEC-aligned Gitea polling, encode issue state as exactly one `aiops/*`
label:

| Workflow state | Gitea label |
| --- | --- |
| `Todo` | `aiops/todo` |
| `In Progress` | `aiops/in-progress` |
| `Human Review` | `aiops/human-review` |
| `Rework` | `aiops/rework` |
| `Done` | `aiops/done` |
| `Canceled` | `aiops/canceled` |

The worker-owned Gitea tracker path uses these labels for both active issue
polling and per-tick reconciliation. If a running issue is moved to `aiops/done`
or `aiops/canceled`, the next poll refreshes that issue by ID and stops the
in-flight run.

For setting up a low-privilege bot account with the minimum token scopes
(including the scopes the `gitea_issue_labels` state tool needs) and branch
protection on a Gitea instance, follow the
[Gitea bot and branch-protection runbook](docs/runbooks/gitea-bot-and-branch-protection.md).

## GitHub issue-state labels

For `tracker.kind: github`, a configured state named `open`, `closed`, or
`all` (case-insensitive) maps directly to the GitHub issue-state filter with
no label involved. **Any other state name is treated as an issue label**: the
worker polls open issues carrying that label, so a workflow state such as
`aiops:ready` means "open and labeled `aiops:ready`".

| Configured state | What the worker polls |
| --- | --- |
| `open` / `closed` / `all` | GitHub issue state, no label filter |
| anything else (e.g. `aiops:ready`) | open issues carrying that label |

The dogfood convention ([ADR 0002](docs/adr/0002-ready-gated-binary-self-hosting.md)):
use a dedicated `aiops:ready` label as the unattended queue entry and do
**not** include `open` in `active_states`, so the worker never sweeps
arbitrary open issues into execution. Priority labels are triage metadata,
not permission to run. (The colon form `aiops:ready` is just this repo's
GitHub-side naming, distinct from the Gitea path's `aiops/*` labels above —
any label name works as long as the configured state matches it exactly.)

Two behaviors keep the label-as-state loop safe:

- **Open-PR claim skip.** Whenever an active state can include open issues,
  the worker lists open pull requests and parses closing keywords
  (`Closes #N`, `Fixes #N`, …) from each PR's title and body; an issue
  claimed by an open PR is skipped while both stay open, so a poll never
  double-dispatches an issue whose agent PR is already in flight.
- **Reconciliation.** Closing the issue (a configured terminal state) stops
  an in-flight run on a following poll. Removing the state label keeps the
  issue out of *future* dispatch but does **not** cancel an already-running
  agent under this configuration: the refreshed issue falls back to plain
  `open`, which is neither active nor a configured inactive/terminal state.
  Close the issue to stop work that is already running.

`tracker.api_key` needs only read access — a fine-grained PAT with read-only
**Issues**, **Pull requests**, and **Metadata** permissions on the target
repository (classic-PAT equivalent: `public_repo`, or `repo` for private
repositories).

[`examples/github-local-WORKFLOW.md`](examples/github-local-WORKFLOW.md) is a
working template wiring all of the above.

## Continuous integration

Every push to `main` and every pull request targeting `main` runs
[`.github/workflows/ci.yml`](.github/workflows/ci.yml). CI is the safety net for
all changes; PRs should not merge while it is red. It runs four jobs:

- **`go`** — format and lint gates (`gofmt`, the blocking golangci-lint gate),
  repo hygiene (`go mod tidy`, Dockerfile/`go.mod` Go-version drift, the Go
  file-size budget), the test suite (`go test -race`, a short fuzz smoke, and the
  dashboard / Trellis / GitHub-script tests), and the build (dashboard bundle
  plus the `worker` and `tui` binaries, uploaded as artifacts).
- **`security`** — supply-chain checks: standalone `go vet ./...` plus
  `govulncheck ./...` built against the `go.mod` toolchain floor.
- **`e2e`** — the end-to-end Gitea mock loop (`go test -tags e2e ./test/e2e/...`)
  against a real `gitea` container.
- **`docker`** — a Docker image build of the repository `Dockerfile` (depends on
  `go`), a blocking Trivy scan for fixed CRITICAL/HIGH image vulnerabilities, and
  a CycloneDX SBOM generated and uploaded as a build artifact.

See the [CI/CD runbook](docs/runbooks/ci.md) for triggers, security posture,
release flow, and local pre-push checks.

For production-style Docker runs that execute real `codex app-server` inside
the worker image, see the
[Codex app-server Docker runbook](docs/runbooks/codex-app-server-docker.md).
For a clean first install, start with the
[Docker + Linear + Codex first-run runbook](docs/runbooks/first-run-docker-linear-codex.md);
it covers the support matrix, Docker secret mounts, `worker --doctor`, and the
todo smoke script.

## AI agent rules

[`AGENTS.md`](AGENTS.md) is the canonical source for all engineering rules
(SPEC alignment, clean code, harness engineering principles, Go runtime
hardening). [`CLAUDE.md`](CLAUDE.md) is a thin bridge that imports it via
`@AGENTS.md` so Claude Code sessions load the same rules automatically. If
you add a coding agent that reads a different file (e.g. `.cursorrules`),
add a bridge that imports `AGENTS.md` rather than duplicating content.

## Architecture notes

- [Architecture overview & diagrams](docs/architecture.md)
- [SPEC deviation ledger](DEVIATIONS.md)
- [Decision: continue the Go port here](DECISION.md)
- [Symphony integration guide](docs/symphony-integration.md)
- [Dashboard brand redesign & UX](docs/design/dashboard-brand-redesign.md)
- [Research: Symphony-style personal productivity](docs/research/symphony-personal-productivity.md)
- [ADR 0001: Adopt a Symphony-style personal orchestrator](docs/adr/0001-symphony-style-personal-orchestrator.md)
- [ADR 0002: Ready-gated binary self-hosted development](docs/adr/0002-ready-gated-binary-self-hosting.md)
- [Local development runbook](docs/runbooks/local-dev.md)
- [Binary (non-Docker) deployment runbook](docs/runbooks/binary-deployment.md)
- [CI/CD runbook](docs/runbooks/ci.md)
- [Runtime debugging API](docs/runbooks/task-api.md)
- [Workspace cache and cleanup](docs/runbooks/workspace-cache.md)
- [GitHub local automation](docs/runbooks/github-local-automation.md)
- [Gitea bot and branch protection](docs/runbooks/gitea-bot-and-branch-protection.md)

## Safety notes

See [`docs/security-posture.md`](docs/security-posture.md) for the current
sandbox model, threat model, and operator checklist. In short: this platform
always relies on the coding agent's own sandbox/approval behavior, such as the
Codex app-server sandbox selected by `codex.thread_sandbox` /
`codex.turn_sandbox_policy`, and can optionally wrap agent
invocation with a Linux `bubblewrap` or `firejail` sandbox configured by the
workflow `sandbox:` block. That wrapper is disabled by default and is not a
container/VM isolation layer.

- Do not use this platform against untrusted issue authors, untrusted
  repositories, or shared production secrets until external sandboxing and
  per-run credential scoping are enabled and validated for your worker host.
- Keep branch protection enabled.
- The agent opens PRs through its workflow/tool surface; the worker does not
  push, open, or merge PRs.
- Use a low-privilege bot account for Git hosting and tracker access.
- Keep company repositories in draft-PR or analysis-only mode until the workflow
  is trusted.
- Do not commit real credentials to `.env`, `.env.example`, or `WORKFLOW.md`.
