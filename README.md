# aiops-platform

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

## Architecture notes

- [Symphony integration guide](docs/symphony-integration.md)
- [Research: Symphony-style personal productivity](docs/research/symphony-personal-productivity.md)
- [ADR 0001: Adopt a Symphony-style personal orchestrator](docs/adr/0001-symphony-style-personal-orchestrator.md)
- [Local development runbook](docs/runbooks/local-dev.md)
- [CI/CD runbook](docs/runbooks/ci.md)
- [Task debugging API](docs/runbooks/task-api.md)
- [Workspace cache and cleanup](docs/runbooks/workspace-cache.md)

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
