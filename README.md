# aiops-platform

> **⚠️ Direction change in progress (2026-05-13).** This repo's Go
> implementation accumulated nine SPEC deviations from upstream Symphony.
> Rather than rewrite the orchestrator core in Go, we are switching to
> fork an existing Symphony implementation as the new base. See
> [`DECISION.md`](DECISION.md) for the reasoning and
> [`docs/research/symphony-fork-evaluation.md`](docs/research/symphony-fork-evaluation.md)
> for the candidate comparison.
>
> The content below describes the current Go implementation as it stood
> before this decision. New architectural work happens on the new repo
> once the fork is chosen; this repo will be archived with a pointer.

A personal-productivity AI coding orchestrator inspired by OpenAI Symphony.

The goal is not to build a heavy enterprise platform first. The goal is to run a practical loop:

```text
Linear or Gitea task
  -> aiops-platform
  -> deterministic workspace
  -> WORKFLOW.md policy + prompt
  -> mock / Codex / Claude runner
  -> verification
  -> draft PR handoff
```

## Two-track workflow

Use both tracks:

1. **OpenAI Symphony directly** for quick Linear + Codex experiments.
2. **aiops-platform** for your own Go-based, Gitea-friendly, locally customizable workflow.

This repository implements the second track.

## Components

- `cmd/trigger-api`: receives Gitea webhooks and manual task submissions.
- `cmd/linear-poller`: polls Linear issues in configured active states and enqueues tasks.
- `cmd/worker`: claims queued tasks and runs the Symphony-style workflow.
- `internal/workflow`: loads repo-owned `WORKFLOW.md` configuration and prompt body.
- `internal/tracker`: tracker abstraction with a Linear client.
- `internal/workspace`: deterministic Git workspace management, verification, and simple policy checks.
- `internal/runner`: runner abstraction for `mock`, `codex`, and `claude`.
- `internal/queue`: PostgreSQL-backed task queue using `FOR UPDATE SKIP LOCKED`.
- `internal/gitea`: webhook parser, signature verification, and PR client.

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
verify fails the worker still opens a draft PR (regardless of `pr.draft`),
emits a `verify_end` event with `status: failed_allowed`, and prepends a
warning banner to the PR body pointing to `.aiops/VERIFICATION.txt`. Use this
when you want to inspect what the agent produced even though the checks
flagged it. Default is `false`; failed verification blocks PR creation.

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

- Keep branch protection enabled.
- The worker opens PRs; it does not merge them.
- Use a low-privilege bot account for Gitea.
- Keep company repositories in draft-PR or analysis-only mode until the workflow is trusted.
- Do not commit real credentials to `.env`, `.env.example`, or `WORKFLOW.md`.
