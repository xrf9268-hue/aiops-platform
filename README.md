# aiops-platform

> **Direction update (2026-05-15).** `aiops-platform` continues as a
> Go implementation of OpenAI Symphony in this repository. `SPEC.md` is
> the contract; the Elixir reference is the tie-breaker when SPEC text is
> ambiguous. See [`DECISION.md`](DECISION.md) and the D1–D24 tracker in
> [`DEVIATIONS.md`](DEVIATIONS.md).

A personal-productivity AI coding orchestrator implementing OpenAI Symphony.

The goal is not to build a heavy enterprise platform first. The goal is to run a practical loop:

```text
Linear or Gitea task
  -> aiops-platform
  -> deterministic workspace
  -> WORKFLOW.md policy + prompt
  -> mock / Codex / Claude runner
  -> verification
  -> agent-side branch push + PR handoff
```

## Workflow

Use `aiops-platform` as the Go-based, Gitea-friendly, locally customizable
Symphony implementation while D1–D24 are closed systematically.

## Components

- `cmd/trigger-api`: receives Gitea webhooks and manual task submissions.
- `cmd/linear-poller`: polls Linear issues in configured active states and enqueues tasks.
- `cmd/gitea-poller`: polls Gitea issues whose mutually-exclusive `aiops/*` labels map to configured active states and enqueues tasks.
- `cmd/worker`: claims/dispatches tasks and runs the Symphony-style workflow.
- `internal/workflow`: loads repo-owned `WORKFLOW.md` configuration and prompt body.
- `internal/tracker`: tracker abstraction with a Linear client.
- `internal/workspace`: deterministic Git workspace management, verification, and simple policy checks.
- `internal/runner`: runner abstraction for `mock`, `codex`, and `claude`.
- `internal/queue`: PostgreSQL-backed task queue using `FOR UPDATE SKIP LOCKED`.
- `internal/gitea`: transitional webhook parser/signature verification plus the Gitea PR-tool implementation consumed through the agent/tool surface (not a worker-side PR handoff).

## WORKFLOW.md discovery

The worker looks for `WORKFLOW.md` in three locations and uses the first one it finds:

1. `<repo>/WORKFLOW.md`
2. `<repo>/.aiops/WORKFLOW.md`
3. `<repo>/.github/WORKFLOW.md`

When multiple files exist, lower-priority files are recorded as `shadowed_by` on the `workflow_resolved` event but are otherwise ignored. The worker does not warn or fail.

If none of the three exist, the worker proceeds with built-in defaults:

| Setting | Default |
|---------|---------|
| `agent.default` | `mock` |
| `agent.timeout` | `30m` |
| `agent.max_concurrent_agents` | `1` |
| `pr.draft` | `false` |
| `pr.labels` | `[ai-generated, needs-review]` |
| `policy.mode` | `draft_pr` |
| `policy.max_changed_files` | `12` |
| `policy.max_changed_loc` | `300` |
| `verify.commands` | none |

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

For post-hoc inspection, the `workflow_resolved` task event records the source, path, and shadowed list of every run.

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

## Continuous integration

Every push to `main` and every pull request targeting `main` runs
[`.github/workflows/ci.yml`](.github/workflows/ci.yml). The CI job is the
safety net for all changes; PRs should not merge while it is red.

CI expectations on each run:

- `gofmt` check on all tracked Go files (no diff).
- `go mod tidy` check (`go.mod` and `go.sum` clean).
- `go test -race -covermode=atomic ./...`.
- `go build` of `cmd/trigger-api`, `cmd/worker`, and `cmd/linear-poller`.
- Docker image build of the repository `Dockerfile`.

See the [CI/CD runbook](docs/runbooks/ci.md) for triggers, security posture,
release flow, and local pre-push checks.

## Quick start: Gitea webhook path

```bash
cp .env.example .env
# edit GITEA_BASE_URL, GITEA_TOKEN, GITEA_WEBHOOK_SECRET
docker compose --env-file .env -f deploy/docker-compose.yml up --build
```

Configure a Gitea issue-comment webhook pointing at:

```text
http://<trigger-host>:8080/v1/events/gitea
```

Comment on a Gitea issue:

```text
/ai-run
```

## Quick start: Linear polling path

Edit `examples/WORKFLOW.md` with your repo and Linear settings, then run:

```bash
export LINEAR_API_KEY=your-linear-personal-key
docker compose --env-file .env -f deploy/docker-compose.yml --profile linear up --build
```

The poller watches active Linear states such as `AI Ready`, `In Progress`, and `Rework`, then enqueues tasks for the worker.

## Quick start: Gitea polling path

For SPEC-aligned Gitea polling, encode issue state as exactly one `aiops/*` label:

| Workflow state | Gitea label |
| --- | --- |
| `AI Ready` | `aiops/todo` |
| `In Progress` | `aiops/in-progress` |
| `Rework` | `aiops/rework` |
| `Done` | `aiops/done` |
| `Canceled` | `aiops/canceled` |

Then run the poller from source:

```bash
export GITEA_BASE_URL=https://gitea.example.com
export GITEA_TOKEN=your-gitea-bot-token
go run ./cmd/gitea-poller examples/WORKFLOW.md
```

The poller reads issues whose labels map to configured active states and enqueues them for the worker. State changes are still performed by the agent through the advertised tool surface, not by the poller.

## First safe mode

Start with:

```yaml
agent:
  default: mock
```

After the mock loop produces a branch and PR, change the workflow to:

```yaml
agent:
  default: codex
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
