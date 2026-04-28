# Roadmap Progress - 2026-04-28

This log records local implementation progress before commit or PR publication.

## 2026-04-28 17:40 CST

### Issue #3 - Local Secret Hygiene

Status: implemented in this branch.

Changed:
- Added `.gitignore` coverage for local env files, workspace folders, caches, logs, temporary files, build output, Go test artifacts, and macOS metadata.
- Kept `.env.example` tracked.
- Replaced the example webhook secret with a placeholder value.

Verified:
- `git check-ignore` confirms `.env`, workspace folders, logs, temporary files, and build outputs are ignored.
- `git check-ignore --no-index -q .env.example` confirms `.env.example` is not ignored.
- `git check-ignore --no-index -q .aiops/TASK.md` confirms intended `.aiops` task artifacts are not ignored.
- Secret-like keys in `.env.example` use `replace-with-...` placeholder values.
- `git diff --check` passed.
- `go test ./...` passed.

### Issue #6 - Manual Enqueue Smoke Test Script

Status: implemented in this branch.

Changed:
- Added `scripts/enqueue-manual-task.sh`.
- Added `scripts/test-enqueue-manual-task.sh`.

Verified:
- First test run failed because `scripts/enqueue-manual-task.sh` did not exist.
- After implementation, `scripts/test-enqueue-manual-task.sh` passed.
- `bash -n scripts/enqueue-manual-task.sh scripts/test-enqueue-manual-task.sh` passed.
- `shellcheck scripts/enqueue-manual-task.sh scripts/test-enqueue-manual-task.sh` passed.
- `git diff --check` passed.
- `go test ./...` passed.

## 2026-04-28 17:46 CST

### Issue #8 - Task Read APIs

Status: implemented in this branch.

Changed:
- Added snake_case JSON tags for task request and response payloads.
- Added task event JSON type.
- Added `GET /v1/tasks/{id}`.
- Added `GET /v1/tasks/{id}/events`.
- Added `GET /v1/tasks?status=queued`.
- Added Postgres store read methods behind the trigger API store interface.
- Added task API documentation and linked it from the README.

Verified:
- First JSON contract test failed because `repo_owner` did not decode into `Task.RepoOwner`.
- First handler test failed because task event type, GET routes, store interface, and not-found behavior were missing.
- After implementation, `go test ./cmd/trigger-api ./internal/task` passed.
- Final batch verification passed: `go test ./...`, `scripts/test-enqueue-manual-task.sh`, `bash -n scripts/enqueue-manual-task.sh scripts/test-enqueue-manual-task.sh`, `shellcheck scripts/enqueue-manual-task.sh scripts/test-enqueue-manual-task.sh`, `gofmt -l cmd internal`, `git diff --check`, and `go mod tidy && git diff --exit-code -- go.mod go.sum`.
