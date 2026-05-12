# Symphony integration

`aiops-platform` follows a Symphony-style personal productivity loop.

## Goal

Use two tracks together:

1. Run OpenAI Symphony directly for fast Linear plus Codex experiments.
2. Keep `aiops-platform` as a Go-based local orchestrator for Gitea, Linear, and custom workflow needs.

## Architecture

```text
Linear or Gitea task
  -> task queue
  -> deterministic workspace
  -> WORKFLOW.md
  -> runner
  -> verification
  -> pull request handoff
```

## Mapping

| Symphony concept | aiops-platform module |
|---|---|
| Issue tracker | `internal/tracker` and `cmd/linear-poller` |
| Workflow contract | `internal/workflow` and `WORKFLOW.md` |
| Workspace manager | `internal/workspace` |
| Agent runner | `internal/runner` |
| Status history | `tasks` and `task_events` tables |
| Git handoff | `internal/gitea` |

## Usage model

Start with the mock runner:

```yaml
agent:
  default: mock
```

After the queue, workspace, branch, and pull request loop works, switch personal projects to:

```yaml
agent:
  default: codex
```

For company repositories, keep human review in the loop and prefer draft pull requests.

## Current scope

Implemented:

- Gitea issue comment trigger
- Linear polling trigger
- Postgres task queue
- repo-owned `WORKFLOW.md` (discovered at three paths — see Deviations below)
- mock, codex, and claude runner abstraction
- deterministic local workspace
- basic path policy
- verification commands
- Gitea pull request creation

Not yet implemented:

- Linear status update after completion
- pull request labels and reviewers
- multi-run reconciliation
- dashboard
- robust event streaming
- OS-level sandboxing (sandbox-exec, firejail, container isolation). Codex CLI's own sandbox is wired via `codex.profile`.

## Deviations from SPEC

Tracked centrally in [`DEVIATIONS.md`](../DEVIATIONS.md). One item is worth
calling out here because it touches workflow discovery directly:

- **Multi-path WORKFLOW.md discovery (D4).** SPEC treats `WORKFLOW.md` as a
  single, repository-owned source. We extend lookup to three paths
  (`<repo>/WORKFLOW.md`, `<repo>/.aiops/WORKFLOW.md`, `<repo>/.github/WORKFLOW.md`)
  with a documented precedence. Lower-priority files that exist but lose
  precedence are recorded as `shadowed_by` on the `workflow_resolved` event,
  echoed on the worker's per-task `workflow resolved:` log line, and surfaced
  at the top level of `worker --print-config` output. This is a deliberate
  extension to make it convenient to park the workflow under `.aiops/` while
  keeping the repo root clean; the observability hooks ensure operators can
  always answer "which file is in effect, and what is being shadowed?"
  without running tooling.

## Pointers

- Symphony's richer codex integration uses the long-running `codex app-server` JSON-RPC protocol (`elixir/lib/symphony_elixir/codex/app_server.ex`) and exposes per-turn sandbox overrides via `Codex.changeset` (`elixir/lib/symphony_elixir/config/schema.ex`). This platform's M4 stays on one-shot `codex exec`; an app-server-style integration is a candidate for M5+.
