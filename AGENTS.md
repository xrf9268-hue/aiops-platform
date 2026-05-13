# AGENTS.md

Project guide for AI coding agents (Codex CLI, Claude Code, etc.) working in this repo.

## What this project is

`aiops-platform` is a Go-based, self-hostable AI coding orchestrator that
implements the [OpenAI Symphony SPEC](https://github.com/openai/symphony/blob/main/SPEC.md).
It receives Linear issues or Gitea webhook events, dispatches them through a
Symphony-style workflow (`mock` / `codex` / `claude` runners) in a deterministic
workspace, and opens a draft PR.

The Go module path is `github.com/xrf9268-hue/aiops-platform` — keep it as-is even if the GitHub repo is temporarily mirrored elsewhere.

> **Transitional note:** the current implementation routes scheduling through a
> Postgres-backed queue (`internal/queue`, `migrations/`). This is deviation
> not in SPEC and is being removed under #73 in favor of SPEC's
> tracker-driven, filesystem-driven recovery model (see also #68). Do not
> design new code that assumes the queue is permanent; in particular, do not
> add new tables, indexes, or migration steps without first checking #73.

## SPEC alignment is a hard requirement

This project is positioned as a Symphony port. The upstream
[Symphony SPEC.md](https://github.com/openai/symphony/blob/main/SPEC.md) is the
authoritative contract. The project is **pre-release** — there are no users to
migrate, so the cost of aligning with SPEC is at its minimum **right now**. Treat
alignment as a non-negotiable goal, not a future cleanup.

Rules for agents working on this repo:

1. **Read SPEC.md before designing any architectural change.** When SPEC describes
   the behavior of a subsystem you are touching (workflow file, agent runner,
   tracker, state machine, recovery, sandboxing, tools), the SPEC text is the
   default; deviations require a written justification.
2. **Every accepted deviation lives in [`DEVIATIONS.md`](DEVIATIONS.md).** If you
   find behavior that violates SPEC and is not already listed there, **do not
   add a new "deliberate extension" to make the discrepancy disappear**. File
   an issue with the `area:spec-alignment` label so the deviation is visible and
   tracked. The umbrella tracker is [#67](https://github.com/xrf9268-hue/aiops-platform/issues/67).
3. **"Has better value than SPEC" is a high bar.** Cosmetic convenience (e.g.
   "let users park a config file in a hidden directory") does not clear it.
   Genuine functional capability (e.g. low-latency Gitea webhook ingress vs. poll)
   may. When in doubt, default to SPEC and open an issue to discuss.
4. **Do not introduce new deviations to fix bugs.** If a SPEC-aligned design
   would make the bug easier to fix, prefer that over patching around a deviation.
5. **Observability is not a substitute for alignment during pre-release.** The
   project briefly chose to *document* a deviation rather than fix it (#69 for
   D4); that approach has since been reversed (#72) and should not be the
   default playbook again. If a deviation is wrong, fix it; do not log it.

The current set of open deviations is in `DEVIATIONS.md` (D1–D5 reference SPEC
sections). Any new SPEC-violating change you make must either (a) close an
existing deviation, (b) be tracked as a new deviation with an issue, or
(c) be reverted.

## Layout

| Path | What lives there |
|------|------------------|
| `cmd/trigger-api` | HTTP server: Gitea webhook ingress (transitional — being removed per #74) |
| `cmd/worker` | Claims queued tasks, runs the Symphony loop, opens PRs |
| `cmd/linear-poller` | Polls Linear active states and enqueues tasks |
| `internal/workflow` | Loads `WORKFLOW.md` (front matter + prompt body) |
| `internal/queue` | Postgres queue (transitional — being removed per #73) |
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

Per SPEC §workflow file, `WORKFLOW.md` is a single repository-owned source at
the repo root:

```
<repo>/WORKFLOW.md
```

Missing front matter is allowed — the body becomes the prompt template, all
other settings fall back to defaults (see `README.md` table). The
`workflow_resolved` task event captures the resolved source and path.

> **Note (transitional):** the current implementation still searches three
> paths (`<repo>/WORKFLOW.md`, `<repo>/.aiops/WORKFLOW.md`,
> `<repo>/.github/WORKFLOW.md`) — this is deviation D4 (#69) and is being
> reverted to single-source under #72. Treat single-source as the canonical
> answer when designing new code; do not add features that depend on the
> alternate paths.

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
