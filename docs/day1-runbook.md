# Day 1 Runbook: `/ai-run` to Gitea PR

This runbook proves the first vertical slice of the AI coding platform:

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

## 1. Configure environment

Copy `.env.example` to `.env` and set:

```bash
GITEA_WEBHOOK_SECRET=change-me
GITEA_BASE_URL=http://your-gitea-host
GITEA_TOKEN=<token for ai-bot with repo write permission>
```

Do not commit real tokens.

## 2. Start services

From repository root:

```bash
docker compose --env-file .env -f deploy/docker-compose.yml up --build
```

Health check:

```bash
curl http://localhost:8080/healthz
```

Expected:

```json
{"ok":true}
```

## 3. Configure Gitea webhook

In the target demo repository:

- URL: `http://<trigger-host>:8080/v1/events/gitea`
- Secret: same as `GITEA_WEBHOOK_SECRET`
- Event: Issue comment

## 4. Trigger a task

Create an issue in the demo repository and comment:

```text
/ai-run
```

Expected webhook response:

```json
{
  "accepted": true,
  "task_id": "tsk_...",
  "deduped": false
}
```

## 5. Verify task state

```bash
docker compose --env-file .env -f deploy/docker-compose.yml exec postgres \
  psql -U aiops -d aiops -c "select id,status,repo_owner,repo_name,work_branch from tasks order by created_at desc limit 5;"
```

Expected final status: `succeeded`.

## 6. Verify Gitea result

The worker should create a branch named:

```text
ai/tsk_...
```

and open a pull request containing:

```text
.aiops/TASK.md
.aiops/<task-id>.md
```

## 7. Current Day 1 limitations

- Only `issue_comment` webhook is handled.
- Only `/ai-run` command is recognized.
- The runner is `mock`; Codex/Claude are not wired yet.
- No deny-path diff enforcement yet.
- No automatic merge; human review is required.
