# Symphony integration

`aiops-platform` is a Go implementation of the OpenAI Symphony SPEC for a
personal productivity loop.

## Goal

Keep the Go implementation aligned with `SPEC.md` while supporting local
Gitea, Linear, and custom workflow needs. `DEVIATIONS.md` tracks the D1–D24
closure backlog.

## Architecture

```text
Linear or Gitea tracker poll
  -> in-memory scheduler/runtime
  -> deterministic workspace
  -> WORKFLOW.md
  -> runner
  -> verification
  -> agent-side branch push + pull request handoff
```

## Mapping

| Symphony concept | aiops-platform module |
|---|---|
| Issue tracker | `internal/tracker` |
| Workflow contract | `internal/workflow` and `WORKFLOW.md` |
| Workspace manager | `internal/workspace` |
| Agent runner | `internal/runner` |
| Runtime state | in-process orchestrator state with tracker/filesystem restart recovery |
| Git handoff | agent via dynamic tool / CLI (`internal/gitea` backs the tool implementation) |

## Usage model

Start with the mock runner:

```yaml
agent:
  default: mock
```

After the queue, workspace, and agent-side branch/PR handoff loop works, switch personal projects to:

```yaml
agent:
  default: codex-app-server
```

For company repositories, keep human review in the loop and prefer draft pull requests.

## Current scope

Implemented:

- Gitea label polling trigger
- Linear polling trigger
- GitHub issue polling trigger (`tracker.kind: github`, wired through
  `cmd/worker` and `internal/tracker/github.go`)
- repo-owned `WORKFLOW.md` in the service/repository root (single canonical path; see `DEVIATIONS.md` D4 closure)
- mock, codex-app-server, and claude runner abstraction
- deterministic local workspace keyed by sanitized source issue identifier (`source_type` + `source_event_id`), so reruns for the same issue reuse the same path while receiving a fresh checkout
- basic path policy
- verification commands
- `linear_graphql` dynamic tool advertisement and invocation for Codex app-server sessions, proxying Linear GraphQL through orchestrator-held auth without exposing the Linear token to the agent process

Not yet implemented:

- remaining app-server protocol gaps not covered by `linear_graphql`, as tracked in `DEVIATIONS.md`
- pull request labels and reviewers
- per-tick reconciliation (#78); startup reconciliation is already closed in #68
- dashboard
- robust event streaming
- sandbox backend coverage beyond the current optional runner enforcement

## Deviations from SPEC

Tracked centrally in [`DEVIATIONS.md`](../DEVIATIONS.md). Deliberate extensions
beyond the SPEC are scoped narrowly and listed there per deviation. Closed
historical deviations — for example multi-path `WORKFLOW.md` discovery (D4,
#72), Gitea webhook ingress (D7, #74), and orchestrator-side PR / push /
Linear writes (D8, #76) — are no longer in the codebase; legacy alternate
workflow paths are not searched or reported as shadow sources.

## Pointers

- Symphony's Codex integration uses the long-running `codex app-server`
  JSON-RPC protocol (`elixir/lib/symphony_elixir/codex/app_server.ex`) and
  exposes per-turn sandbox overrides via `Codex.changeset`
  (`elixir/lib/symphony_elixir/config/schema.ex`). This platform now has a
  Codex app-server runner path; remaining protocol/runtime gaps stay tracked in
  `DEVIATIONS.md` until their rows close.
