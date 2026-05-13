# AGENTS.md

Project guide for AI coding agents (Codex CLI, Claude Code, etc.) working in this repo.

## What this project is

`aiops-platform` is a Go-based, self-hostable AI coding orchestrator that
implements the [OpenAI Symphony SPEC](https://github.com/openai/symphony/blob/main/SPEC.md).
The orchestrator polls a tracker (Linear, soon Gitea), prepares a deterministic
per-issue workspace, runs a coding agent in that workspace, and watches the
agent's lifecycle. Per SPEC §1, the **agent** is what writes tickets, opens
PRs, and pushes branches — through tools the orchestrator advertises
(`linear_graphql` and equivalent for Gitea). The orchestrator is the
scheduler/runner and tracker *reader*, not a tracker writer.

The Go module path is `github.com/xrf9268-hue/aiops-platform` — keep it as-is even if the GitHub repo is temporarily mirrored elsewhere.

> **Transitional notes** — several pieces of the current implementation deviate
> from this SPEC-aligned picture and are being reverted. Do not design new
> code that depends on the legacy behavior:
>
> - **Postgres queue** (`internal/queue`, `migrations/`): not in SPEC; being
>   removed under #73 in favor of in-memory + tracker + filesystem recovery
>   (#68).
> - **Gitea webhook ingress** (`cmd/trigger-api`, `internal/triggerapi`,
>   `internal/gitea/webhook*.go`): not in SPEC; being replaced with a Gitea
>   poller under #74.
> - **Orchestrator-driven PR creation, git push, and Linear status writes**
>   (`internal/worker/runtask.go` calls to `CommitAndPush`, `CreatePR`,
>   `OnClaim`, `OnPRCreated`): SPEC §1 says these are agent responsibilities;
>   being moved to agent-side dynamic tools under #76 (depends on app-server
>   protocol, #64).
> - **No per-tick reconciliation; in-flight runs ignore tracker state**:
>   SPEC §2.1 Goal "Stop active runs when issue state changes make them
>   ineligible" is unimplemented. #68 covers startup reconciliation only;
>   the per-tick reconcile + agent-cancel propagation lives under #78.
> - **Multi-path WORKFLOW.md discovery** (`internal/workflow/resolver.go`):
>   SPEC says single source; being reverted under #72.

## SPEC alignment is a hard requirement

This project is positioned as a Symphony port. **Three upstream sources are
jointly authoritative**:

1. The protocol contract: [Symphony SPEC.md](https://github.com/openai/symphony/blob/main/SPEC.md).
2. The reference implementation: [`openai/symphony` Elixir tree](https://github.com/openai/symphony/tree/main/elixir).
   When SPEC text is ambiguous, the reference's behavior is the tiebreaker.
   Pay particular attention to:
   - [`elixir/lib/symphony_elixir/orchestrator.ex`](https://github.com/openai/symphony/blob/main/elixir/lib/symphony_elixir/orchestrator.ex) — in-process GenServer state; no DB; reconcile-on-startup via tracker fetch.
   - [`elixir/lib/symphony_elixir/codex/app_server.ex`](https://github.com/openai/symphony/blob/main/elixir/lib/symphony_elixir/codex/app_server.ex) — long-running JSON-RPC 2.0 over stdio; not one-shot exec.
   - [`elixir/lib/symphony_elixir/tracker.ex`](https://github.com/openai/symphony/blob/main/elixir/lib/symphony_elixir/tracker.ex) and adapters — polling model with `:poll_interval_ms`; no webhook ingress.
   - [`elixir/lib/symphony_elixir/config/schema.ex`](https://github.com/openai/symphony/blob/main/elixir/lib/symphony_elixir/config/schema.ex) — canonical config keys, defaults, and types.
3. The authors' announcement post, mirrored locally as
   [`docs/research/2026-04-27-openai-symphony-blog.md`](docs/research/2026-04-27-openai-symphony-blog.md).
   Provides the design rationale and the SPEC §1 problem statement. Direct
   quotes to anchor on:
   - "Symphony is a scheduler/runner and tracker reader." (defines our boundary)
   - "Ticket writes (state transitions, comments, PR links) are typically
     performed by the coding agent using tools available in the workflow/runtime
     environment." (where #76 comes from)
   - "Support restart recovery without requiring a persistent database."
     (where #73 comes from)
   - "Poll the issue tracker on a fixed cadence" (where #74 comes from)
   - "Symphony continuously watches the task board and ensures that every
     active task has an agent running in the loop until it's done."
     (where #78 comes from)
   - "We use [dynamic tool calls] to expose the raw `linear_graphql` function...
     without relying on MCP or exposing the access token to containers."
     (where the token-isolation requirement in #76 comes from)

**Practitioner accounts** (advisory; not authoritative on SPEC, but useful
for matching observed Symphony behavior in real deployments and for
calibrating harness-engineering decisions):

- [`docs/research/2026-05-george-symphony-electron-rewrite.md`](docs/research/2026-05-george-symphony-electron-rewrite.md)
  — first-hand operator report of running 50 Linear tickets to 30 merged
  PRs overnight. Pins the user-visible state-machine semantics
  ("Cancel a ticket — the agent stops on the next poll") and reinforces
  that the WORKFLOW.md prompt is the leverage point, not the orchestrator
  ("Symphony is plumbing; the prompt teaches the agent how to plan,
  test, handle review feedback, and constrain scope").
- [`docs/research/2026-05-addy-osmani-harness-engineering.md`](docs/research/2026-05-addy-osmani-harness-engineering.md)
  — Addy Osmani's harness-engineering thread. Provides the
  vocabulary and principles for evaluating components (see "Harness
  engineering principles" below).

The project is **pre-release** — there are no users to migrate, so the cost of
aligning with SPEC and the reference is at its minimum **right now**. Treat
alignment as a non-negotiable goal, not a future cleanup.

## Harness engineering principles

Adopted from
[Addy Osmani's harness-engineering thread](docs/research/2026-05-addy-osmani-harness-engineering.md).
These complement (not replace) the SPEC-alignment rules above; they
govern *how* we evaluate components inside the SPEC-aligned envelope.

1. **Behavior first.** Every component must name the specific behavior
   it delivers. If you cannot state that behavior in one sentence, the
   component does not belong in the harness — remove it. This is
   exactly the test that retired the Postgres queue (#73),
   Gitea webhook (#74), and multi-path WORKFLOW.md (#72) as
   "deliberate extensions": none of them had a nameable behavior that
   SPEC alignment didn't already cover.
2. **Earned rules.** Every rule in `AGENTS.md`, `WORKFLOW.md`,
   `DEVIATIONS.md`, and the prompt template should trace back to a
   specific, observed failure. Treat the files like a pilot's checklist,
   not a style guide. When in doubt, leave a rule out until you have a
   failure that demands it. (Pre-release exception: rules derived
   directly from SPEC or the Elixir reference are earned by the
   protocol contract itself.)
3. **Failures are configuration problems.** When the agent does the
   wrong thing, the default response is to tighten the harness — add
   a hook, sharpen a tool description, tighten the prompt, narrow a
   permission — not to wait for a smarter model. The harness is a
   living artifact: every observed failure should produce a permanent
   constraint that prevents the same failure next time.
4. **Constraints have a lifecycle.** Rules added to fix a failure may
   become redundant when a later model handles the case natively.
   Periodically audit `AGENTS.md` and the prompt template; remove
   scaffolding that is no longer earning its keep, and use the
   freed-up surface to reach the next horizon.
5. **Few sharp tools beat many overlapping ones.** When the
   agent's tool surface lands (#76), aim for the smallest set of
   focused tools (`linear_graphql`, one Gitea PR tool, etc.); resist
   the temptation to wrap every Gitea / Linear endpoint as a separate
   tool.

Rules for agents working on this repo:

1. **Read SPEC.md and the relevant Elixir module before designing any
   architectural change.** When SPEC describes the behavior of a subsystem you
   are touching (workflow file, agent runner, tracker, state machine, recovery,
   sandboxing, tools), the SPEC text is the default and the Elixir reference
   resolves any ambiguity. Deviations require a written justification.
2. **Every accepted deviation lives in [`DEVIATIONS.md`](DEVIATIONS.md).** If you
   find behavior that violates SPEC or contradicts the reference and is not
   already listed there, **do not add a new "deliberate extension" to make the
   discrepancy disappear**. File an issue with the `area:spec-alignment` label
   so the deviation is visible and tracked. The umbrella tracker is
   [#67](https://github.com/xrf9268-hue/aiops-platform/issues/67).
3. **"Has better value than SPEC" is a high bar.** Cosmetic convenience (e.g.
   "let users park a config file in a hidden directory") does not clear it.
   Things that initially look like better value but match neither SPEC nor the
   Elixir reference are usually wrong on closer inspection — see #74 (Gitea
   webhook ingress, sold as "lower latency" but ultimately reverted) for a
   recent worked example. When in doubt, default to SPEC and open an issue.
4. **Do not introduce new deviations to fix bugs.** If a SPEC-aligned design
   would make the bug easier to fix, prefer that over patching around a deviation.
5. **Observability is not a substitute for alignment during pre-release.** The
   project briefly chose to *document* a deviation rather than fix it (#69 for
   D4); that approach has since been reversed (#72) and should not be the
   default playbook again. If a deviation is wrong, fix it; do not log it.
6. **When in doubt, port from the Elixir tree.** This project is a Go port of a
   working Elixir reference. If you are unsure how a subsystem should behave,
   read the corresponding Elixir module first; the answer is usually there.

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
