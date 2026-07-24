# Symphony integration

`aiops-platform` is a Go implementation of the OpenAI Symphony SPEC for a
personal-productivity coding loop.

## Stable ownership boundary

The worker is a scheduler/runner and tracker reader. It polls and reconciles
tracker state, dispatches eligible issues, prepares deterministic workspaces,
resolves the repository's `WORKFLOW.md`, and invokes the configured runner.

```text
tracker issue
  -> worker poll + reconcile + dispatch
  -> deterministic workspace + WORKFLOW.md
  -> runner
  -> coding agent
  -> agent-owned verify + branch push + PR + tracker write-back
```

The coding agent owns source changes and the irreversible handoff steps. The
worker may expose authenticated runtime tools without exposing their tokens, but
it does not decide or perform agent-side tracker mutations, pushes, or pull
requests. Verification instructions belong in the workflow prompt and are run
by the agent before handoff; there is no worker-owned post-turn verification
phase.

## Current authorities

This page intentionally does not copy feature inventories or deviation status.
Use the maintained sources instead:

- [README](../README.md) — current product surface, usage, and configuration.
- [Symphony SPEC mirror](research/SPEC.md) — authoritative protocol contract;
  the upstream Elixir implementation resolves ambiguity.
- [Architecture overview](architecture.md) — current runtime flow and package
  boundaries.
- [ADR 0001](adr/0001-symphony-style-personal-orchestrator.md) and the
  [engineering-rules rationale](engineering-rules-rationale.md) — design and
  harness decisions with provenance.
- [Deviation ledger](../DEVIATIONS.md) — current SPEC differences and their
  tracked status.
