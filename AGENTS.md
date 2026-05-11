# AGENTS.md

Project guide for AI coding agents (Codex CLI, Claude Code, etc.) working in this repo.

## What this project is

`aiops-platform` is a Go-based, self-hostable AI coding orchestrator inspired by OpenAI Symphony. It receives Linear issues or Gitea webhook events, claims them through a Postgres-backed queue, runs them through a Symphony-style workflow (`mock` / `codex` / `claude` runners) in a deterministic workspace, and opens a draft PR.

The Go module path is `github.com/xrf9268-hue/aiops-platform` — keep it as-is even if the GitHub repo is temporarily mirrored elsewhere.

## Layout

| Path | What lives there |
|------|------------------|
| `cmd/trigger-api` | HTTP server: Gitea webhook ingress + manual task submission |
| `cmd/worker` | Claims queued tasks, runs the Symphony loop, opens PRs |
| `cmd/linear-poller` | Polls Linear active states and enqueues tasks |
| `internal/workflow` | Loads `WORKFLOW.md` (front matter + prompt body) |
| `internal/queue` | Postgres queue using `FOR UPDATE SKIP LOCKED` |
| `internal/runner` | Runner abstraction: `mock`, `codex`, `claude` |
| `internal/workspace` | Deterministic git workspace, verify, policy checks |
| `internal/tracker` | Tracker abstraction with Linear client |
| `internal/gitea` | Webhook parser, signature verification, PR client |
| `internal/triggerapi`, `internal/worker` | HTTP handlers and worker lifecycle |
| `internal/task`, `internal/policy` | Task event constants, policy helpers |
| `migrations/` | SQL migrations for the Postgres queue |
| `docs/adr/` | Architectural decisions (start here for "why") |
| `docs/runbooks/` | Operational guides (CI, local dev, secret scan, workspace cache) |
| `test/e2e/` | Build-tagged E2E suite (`-tags e2e`) using Postgres + Gitea containers |

## Build, test, lint

The CI gate is the authoritative checklist — match it locally before pushing:

```bash
gofmt -l $(git ls-files '*.go')         # must be empty
go mod tidy && git diff --exit-code -- go.mod go.sum
go test -race -covermode=atomic ./...
go build ./cmd/trigger-api ./cmd/worker ./cmd/linear-poller
```

E2E (requires Docker, pulls `postgres:16` and `gitea/gitea:1.26.1-rootless`):

```bash
go test -tags e2e -race -timeout 15m ./test/e2e/...
```

Inspect the effective `WORKFLOW.md` resolution for a workdir without consuming a task:

```bash
go run ./cmd/worker --print-config /path/to/repo/clone
```

Go toolchain: pinned via `go.mod` (Go 1.25). Don't edit `go.mod`'s `go` directive opportunistically.

## Conventions

- **gofmt is non-negotiable**: CI fails on any diff. Always run before committing.
- **`go mod tidy` must leave `go.mod`/`go.sum` clean**: don't add deps you don't use.
- **No `t.Parallel()` in tests that touch shared Postgres state** — the queue tests rely on serial execution to assert ordering.
- **Task events**: when adding a new lifecycle event, add the kind as a constant in `internal/task` rather than inlining the string at the call site.
- **Don't mock the database in integration tests** — hit real Postgres (testcontainers or the E2E harness).
- **Secrets**: never commit real credentials. `.env`, `.env.*`, `*.key`, `*.pem` are gitignored; `.env.example` is the only sanctioned env template.
- **PRs from the worker are draft + labeled by default**; respect `policy.max_changed_files` (12) and `policy.max_changed_loc` (300) defaults when shaping changes.

## WORKFLOW.md discovery (worker side)

The worker resolves `WORKFLOW.md` from a target clone in this priority order:

1. `<repo>/WORKFLOW.md`
2. `<repo>/.aiops/WORKFLOW.md`
3. `<repo>/.github/WORKFLOW.md`

Missing front matter is allowed — the body becomes the prompt template, all other settings fall back to defaults (see `README.md` table). The `workflow_resolved` task event captures the resolved source, path, and shadowed candidates.

## Where to read next

- `README.md` — user-facing quick start (Gitea webhook path, Linear polling path)
- `docs/runbooks/local-dev.md` — local dev loop
- `docs/runbooks/ci.md` — CI behavior, release flow, pre-push checks
- `docs/runbooks/secret-scanning.md` — opt-in pre-push leak scan
- `docs/runbooks/workspace-cache.md` — workspace lifecycle and cleanup
- `docs/adr/0001-symphony-style-personal-orchestrator.md` — the "why"

## Safety posture for agents

- The worker opens PRs; it never merges. Don't add auto-merge logic.
- Keep first-time real runs on `agent.default: mock` until the loop is trusted on the target repo.
- Use low-privilege bot accounts for Gitea / Linear / GitHub tokens.
- When in doubt about scope, prefer a narrower change and a clear PR description over speculative refactors.
