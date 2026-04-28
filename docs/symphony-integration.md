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
- repo-owned `WORKFLOW.md`
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
- advanced sandboxing
