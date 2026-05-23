# aiops-platform

> **Direction update (2026-05-15).** `aiops-platform` continues as a
> Go implementation of OpenAI Symphony in this repository. `SPEC.md` is
> the contract; the Elixir reference is the tie-breaker when SPEC text is
> ambiguous. See [`DECISION.md`](DECISION.md) and the D1–D24 tracker in
> [`DEVIATIONS.md`](DEVIATIONS.md).

A personal-productivity AI coding orchestrator implementing OpenAI Symphony.

The goal is not to build a heavy enterprise platform first. The goal is to run a practical loop:

```text
Linear, Gitea, or GitHub issue
  -> aiops-platform
  -> deterministic workspace
  -> WORKFLOW.md policy + prompt
  -> mock / Codex / Claude runner
  -> verification
  -> agent-side branch push + PR handoff
```

## Workflow

Use `aiops-platform` as the Go-based, Gitea-friendly, locally customizable
Symphony implementation while the remaining open/partial D1–D24 items in
[`DEVIATIONS.md`](DEVIATIONS.md) are closed systematically.

## Components

- `cmd/worker`: polls the configured tracker, reconciles startup workspaces, owns the in-memory orchestrator runtime state, dispatches eligible issues, and runs the Symphony-style workflow without Postgres.
- `cmd/linear-poller`: transitional Linear-to-queue poller retained under the legacy queue profile; the worker now owns the SPEC-aligned poll tick.
- `cmd/gitea-poller`: transitional Gitea-to-queue poller for `aiops/*` label state; the worker can read Gitea issues directly through `tracker.kind: gitea`.
- `internal/workflow`: loads repo-owned `WORKFLOW.md` configuration and prompt body.
- `internal/tracker`: tracker abstraction with Linear and GitHub clients.
- `internal/workspace`: deterministic Git workspace management, verification, and simple policy checks.
- `internal/runner`: runner abstraction for `mock`, `codex`, and `claude`.
- `internal/orchestrator`: single in-memory runtime state, serialized dispatch authority, retry bookkeeping, and worker spawn bridge.
- `internal/queue`: legacy PostgreSQL-backed task queue still used by transitional poller binaries, not by `cmd/worker`.
- `internal/gitea`: Gitea tracker client support plus the Gitea PR-tool implementation consumed through the agent/tool surface (not a worker-side PR handoff).

## WORKFLOW.md discovery

The worker resolves one canonical workflow source: `WORKFLOW.md` in the service/repository root, or an explicit startup workflow path where supported. Legacy fallback files such as `.aiops/WORKFLOW.md` and `.github/WORKFLOW.md` are not searched and are not reported as shadowed workflow sources.

If the canonical file does not exist, the worker proceeds with built-in defaults. The table below mirrors SPEC §6.4's cheat-sheet so a SPEC reader's mental model lines up with `worker --print-config` output; defaults that diverge from SPEC are called out explicitly and tracked in [`DEVIATIONS.md`](DEVIATIONS.md):

| Setting | Default | Source |
|---------|---------|--------|
| `agent.default` | `mock` | implementation (SPEC defers to operator) |
| `agent.max_concurrent_agents` | `10` | SPEC §6.4 |
| `agent.max_turns` | `20` clean turns per issue before continuation stops | SPEC §6.4 |
| `agent.timeout` | `30m` | implementation (#215) |
| `agent.max_retry_attempts` | `1` failure retry after the first run (`0` disables) | implementation (#215) |
| `agent.max_timeout_retries` | `1` timeout retry after the first timeout (`0` disables) | implementation (#215) |
| `agent.policy_violation_budget` | `2` policy-violation feedback entries per issue before non-retryable fail (`0` disables suppression) | implementation (#230) |
| `codex.command` | `codex app-server` | SPEC §6.4 |
| `pr.draft` | `false` | implementation |
| `pr.labels` | `[ai-generated, needs-review]` | implementation |
| `server.port` | `4000` (`-1` disables the HTTP state server) | implementation |
| `policy.mode` | `draft_pr` | implementation |
| `policy.max_changed_files` | `12` | implementation |
| `policy.max_changed_loc` | `300` | implementation |
| `tracker.kind` | `gitea` (SPEC §6.4 marks this REQUIRED; see DEVIATIONS D28) | partial deviation |
| `tracker.project_slug` | required for `tracker.kind: linear` unless `services[]` routes define per-service project slugs | SPEC §6.4 |
| `tracker.active_states` | `[Todo, In Progress]` | SPEC §6.4 |
| `tracker.terminal_states` | `[Closed, Cancelled, Canceled, Duplicate, Done]` | SPEC §6.4 |
| `workspace.root` | `/symphony_workspaces` (per-boot) — set explicitly to a long-lived path for persistence | SPEC §6.4 |
| `verify.commands` | none | implementation |

Operators who want the historical personal-profile values — `agent.max_concurrent_agents: 1`, `codex.command: codex exec`, `workspace.root: ~/aiops-workspaces/personal`, the Linear-vocabulary state lists — copy [`examples/WORKFLOW.md`](examples/WORKFLOW.md) and declare them explicitly. The example file pins every divergent value so a SPEC reader can see the personal-profile envelope without reading source.

When present, `hooks.timeout_ms` must be a positive integer. Omit it to use the
default `60000` ms timeout.

The worker binds a private-loopback HTTP server at `127.0.0.1:<server.port>` and publishes the SPEC §13.7 state snapshot at `GET /api/v1/state`. Set `server.port: -1` to disable the endpoint in environments that provide their own state bridge. The endpoint accepts only `localhost` and `127.0.0.1` Host headers and assumes local host users and processes are trusted; on shared hosts, disable it or isolate the worker behind host-level access controls. If the configured listener cannot start, the worker logs the failure, continues without the HTTP state endpoint, and retries on later workflow reload checks until the bind succeeds or `server.port` changes.

The status surface should be treated as containing live agent text even though it binds to loopback. Per SPEC §15.3, the `last_message` field on each running row is a passthrough of the most recent Codex notification message and may include echoed issue body text, `linear_graphql` tool responses, or tool output. The worker truncates the field to 256 runes and pattern-scrubs common token shapes (Authorization headers, bearer tokens, `sk-`/`ghp_`-prefixed keys, embedded basic-auth URLs) before serializing it, but loopback is not a trust boundary on multi-tenant hosts — co-tenant containers, sidecars, and any local process can read the field. Treat screenshots / dashboard caches / chat pastes accordingly.

A `WORKFLOW.md` with no YAML front matter (just a prompt body) is supported: the body becomes the prompt template, all other settings fall through to the defaults above. The `workflow_resolved` event records this as `source: prompt_only` so an operator can tell apart "ran with full Symphony config" from "ran with body-only template".

### `verify.timeout` and `verify.allow_failure`

`verify.timeout` (Go duration string, e.g. `5m`) caps the entire verify phase.
The default `0` means unbounded, preserving the previous behavior. When the
deadline elapses, the in-flight command is killed via context cancellation and
the remaining commands are skipped; the task fails through the normal verify
path unless `verify.allow_failure` is set.

`verify.allow_failure: true` opts the worker into "investigation mode": when
verify fails the worker emits a `verify_end` event with
`status: failed_allowed` and still requires the agent-produced summary. The
agent remains responsible for any branch push or PR handoff it performs from
the workflow/tool surface. Use this when you want to inspect what the agent
produced even though the checks flagged it. Default is `false`; failed
verification blocks the worker from marking the run successful.

To inspect the effective configuration for a workdir without consuming a task:

```bash
worker --print-config /path/to/repo/clone
```

The output is JSON. `tracker.api_key` is masked as `***`; the prompt template is summarized (length + first line) rather than printed verbatim — `cat <resolution.path>` to see the full body.

For post-hoc inspection, the `workflow_resolved` task event records the source and path of every run; `shadowed_by` is omitted unless future non-legacy resolution metadata is added.

## Architecture notes

- [SPEC.md deviations](DEVIATIONS.md)
- [Symphony integration guide](docs/symphony-integration.md)
- [Research: Symphony-style personal productivity](docs/research/symphony-personal-productivity.md)
- [ADR 0001: Adopt a Symphony-style personal orchestrator](docs/adr/0001-symphony-style-personal-orchestrator.md)
- [Local development runbook](docs/runbooks/local-dev.md)
- [CI/CD runbook](docs/runbooks/ci.md)
- [Task debugging API](docs/runbooks/task-api.md)
- [Workspace cache and cleanup](docs/runbooks/workspace-cache.md)
- [Pre-push secret scanning](docs/runbooks/secret-scanning.md)
- [GitHub local automation](docs/runbooks/github-local-automation.md)

## Continuous integration

Every push to `main` and every pull request targeting `main` runs
[`.github/workflows/ci.yml`](.github/workflows/ci.yml). The CI job is the
safety net for all changes; PRs should not merge while it is red.

CI expectations on each run:

- `gofmt` check on all tracked Go files (no diff).
- `go mod tidy` check (`go.mod` and `go.sum` clean).
- `go test -race -covermode=atomic ./...`.
- `go build` of `cmd/worker`, `cmd/linear-poller`, and `cmd/gitea-poller`.
- Docker image build of the repository `Dockerfile`.

See the [CI/CD runbook](docs/runbooks/ci.md) for triggers, security posture,
release flow, and local pre-push checks.

## Quick start: worker-owned tracker polling path

Edit `examples/WORKFLOW.md` with your repository and tracker settings, then run the worker. The worker performs startup reconciliation before the first poll tick, then repeatedly fetches active issues and dispatches them through the in-memory orchestrator state:

```bash
export AIOPS_WORKFLOW_PATH=$PWD/examples/WORKFLOW.md
export WORKSPACE_ROOT=$PWD/.aiops/workspaces

# For tracker.kind: linear
export LINEAR_API_KEY=your-linear-personal-key
# Set tracker.project_slug in WORKFLOW.md to the Linear project slugId
# unless you use services[] routes with per-service tracker.project_slug values.
# Example: a Linear project URL ending in /project/aiops-platform-abc123
# uses project_slug: aiops-platform-abc123.

# For tracker.kind: gitea
export GITEA_BASE_URL=https://gitea.example.com
export GITEA_TOKEN=your-gitea-bot-token

# For tracker.kind: github
export GITHUB_TOKEN=$(gh auth token -h github.com)

go run ./cmd/worker
```

`WORKFLOW.md` front matter is the source of truth for runtime workspace placement: when `workspace.root` is set in the selected workflow, the worker creates and reconciles task workspaces under that path. `WORKSPACE_ROOT` is only the fallback for workflows that omit `workspace.root`.

No `DATABASE_URL` or Postgres service is required for `cmd/worker`. Restart recovery follows SPEC §14.3: the worker starts with fresh runtime state, cleans terminal workspaces from tracker state, and re-dispatches eligible active issues on the next poll rather than restoring queue rows, retry timers, or running sessions from a database.

The default Compose service now starts only `worker` unless a legacy profile is requested:

```bash
docker compose --env-file .env -f deploy/docker-compose.yml up --build worker
```

For an operator-side walkthrough — workflow file layout, the `/api/v1/state` and `--print-config` smoke checks, and the legacy queue-driven pollers — see the [local development runbook](docs/runbooks/local-dev.md). If the runbook and this README ever diverge, **this README is canonical**.

## Legacy Linear queue polling path

`cmd/linear-poller` is retained only as transitional queue-ingress code until D7 cleanup. The SPEC-aligned `worker` no longer drains Postgres queue rows, so do not start `linear-poller worker` as a working legacy stack. For active Linear issue execution, configure `tracker.kind: linear` in `WORKFLOW.md` and run the worker-owned tracker polling path above.

## Legacy quick start: Gitea queue polling path

For SPEC-aligned Gitea polling, encode issue state as exactly one `aiops/*` label:

| Workflow state | Gitea label |
| --- | --- |
| `AI Ready` | `aiops/todo` |
| `In Progress` | `aiops/in-progress` |
| `Rework` | `aiops/rework` |
| `Done` | `aiops/done` |
| `Canceled` | `aiops/canceled` |

The worker-owned Gitea tracker path uses these labels for both active issue
polling and per-tick reconciliation. If a running issue is moved to
`aiops/done` or `aiops/canceled`, the next poll refreshes that issue by ID and
stops the in-flight run.

Then run the poller from source:

```bash
export GITEA_BASE_URL=https://gitea.example.com
export GITEA_TOKEN=your-gitea-bot-token
go run ./cmd/gitea-poller examples/gitea-WORKFLOW.md
```

The legacy poller reads issues whose labels map to configured active states and enqueues them for the transitional queue path. For SPEC-aligned operation, prefer running `cmd/worker` directly so the worker owns tracker polling and orchestrator runtime state.

## First safe mode

Start with the mock runner:

```yaml
agent:
  default: mock
```

For ambiguous or high-risk work, keep the workflow in analysis-only mode until
operators have reviewed the plan:

```yaml
agent:
  default: mock # or codex/claude after the workflow is trusted
policy:
  mode: analysis_only
```

Analysis-only mode asks the agent to produce an assessment artifact such as
`.aiops/PLAN.md` and the required `.aiops/RUN_SUMMARY.md` without relying on the
worker to commit, push, open PRs, or post tracker comments. If the plan needs to
be posted back to Linear or another tracker, that handoff belongs to the
agent-side tool surface (for example `linear_graphql` when configured), not to
worker-side tracker writes. Use normal implementation mode such as
`policy.mode: draft_pr` only when the agent should make code changes and manage
PR handoff through its workflow/tools.

After the mock loop is trusted, change the workflow to:

```yaml
agent:
  default: codex
policy:
  mode: draft_pr
```

## Safety notes

See [`docs/security-posture.md`](docs/security-posture.md) for the current
sandbox model, threat model, and operator checklist. In short: this platform
always relies on the coding agent's own sandbox/approval behavior, such as Codex
CLI's sandbox selected by `codex.profile`, and can optionally wrap agent
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
- Keep company repositories in draft-PR or analysis-only mode until the workflow is trusted.
- Do not commit real credentials to `.env`, `.env.example`, or `WORKFLOW.md`.
