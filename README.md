# aiops-platform

Enterprise-internal AI coding automation platform skeleton.

Day 1 goal:

```text
Gitea issue comment `/ai-run`
  -> trigger-api webhook
  -> Postgres queued task
  -> worker claim
  -> git clone + branch
  -> mock runner writes `.aiops/<task>.md`
  -> git push
  -> Gitea pull request
```

## Components

- `cmd/trigger-api`: receives Gitea webhooks and manual task submissions.
- `cmd/worker`: claims queued tasks and executes the Day 1 mock pipeline.
- `internal/queue`: PostgreSQL-backed task queue using `FOR UPDATE SKIP LOCKED`.
- `internal/gitea`: webhook parser, signature verification, and PR client.
- `internal/runner`: execution engines. Day 1 includes `mock` only.
- `migrations`: database schema.
- `deploy`: local docker compose deployment.

## Quick start

```bash
cp .env.example .env
# edit GITEA_BASE_URL, GITEA_TOKEN, GITEA_WEBHOOK_SECRET
docker compose --env-file .env -f deploy/docker-compose.yml up --build
```

Health check:

```bash
curl http://localhost:8080/healthz
```

Full Day 1 runbook: [`docs/day1-runbook.md`](docs/day1-runbook.md)

## Security notes

- Do not commit real Gitea tokens.
- Use a dedicated `ai-bot` account with repo write permission only on demo repositories.
- Keep branch protection enabled. The worker opens PRs; it does not merge them.
- Start with `model=mock`; wire Codex/Claude only after the pipeline is proven.
