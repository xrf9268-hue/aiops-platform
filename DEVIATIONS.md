# DEVIATIONS

This document records every known difference between `aiops-platform` and the
upstream [Symphony specification](https://github.com/openai/symphony/blob/main/SPEC.md).

`aiops-platform` is positioned as "inspired by Symphony", not a 1:1 port. Some
deviations are deliberate scope reductions or extensions for our Gitea-friendly,
locally customizable workflow; others are gaps we intend to close. Either way
they should be visible to anyone evaluating SPEC alignment.

The umbrella tracking issue is [#67](https://github.com/xrf9268-hue/aiops-platform/issues/67).

## Deviations

| ID | Area | SPEC reference | Severity | Status | Tracking issue |
|----|------|----------------|----------|--------|----------------|
| D1 | Agent protocol: `codex exec` vs `codex app-server` | §Agent Runner, §tools, §max_turns | High | Open | [#64](https://github.com/xrf9268-hue/aiops-platform/issues/64) |
| D2 | Linear status not written back after handoff | §state machine, §terminal_states | Medium | Open | [#14](https://github.com/xrf9268-hue/aiops-platform/issues/14) |
| D3 | No multi-run reconciliation on restart | §retry/reconciliation, §restart recovery | Medium | Open | [#68](https://github.com/xrf9268-hue/aiops-platform/issues/68) |
| D4 | WORKFLOW.md multi-path discovery (vs single source) | §workflow file | Low | Open | [#69](https://github.com/xrf9268-hue/aiops-platform/issues/69) |
| D5 | Sandbox posture relies solely on Codex CLI sandbox | §safety, §harness hardening | Medium | Open | [#70](https://github.com/xrf9268-hue/aiops-platform/issues/70) |

Severity reflects the risk and the gap to SPEC, not the implementation effort.

## Deliberate extensions

These are NOT bugs and we do not plan to remove them, but they are differences
from the reference implementation and worth documenting:

- **Gitea webhook trigger path.** SPEC describes pull-only via tracker polling.
  We accept Gitea issue-comment webhooks as an additional task source so the
  same orchestrator covers both Linear-driven and Gitea-driven flows.
- **PostgreSQL-backed queue.** SPEC describes restart recovery as
  "tracker-driven and filesystem-driven (without a durable orchestrator DB)".
  Our queue is a durable Postgres table using `FOR UPDATE SKIP LOCKED`. This
  is what lets multiple workers coexist safely; a true SPEC-aligned recovery
  pass on top of this queue is tracked separately under D3 (#68).
- **Runner abstraction supporting `mock` / `codex` / `claude`.** SPEC is
  agent-agnostic but the reference is Codex-specific. The mock runner is what
  makes end-to-end smoke tests cheap; the Claude runner is for personal use.
- **WORKFLOW.md discovered at 3 paths instead of 1.** Lookup order is
  `<repo>/WORKFLOW.md`, then `<repo>/.aiops/WORKFLOW.md`, then
  `<repo>/.github/WORKFLOW.md`. Lower-priority files are recorded as
  `shadowed_by` on the `workflow_resolved` event and logged at info level
  during resolution (see D4 above for the observability work).

## How to use this list

If you are auditing SPEC alignment, walk the table top to bottom and check the
linked issue for the latest status. A row stays here until either:

1. The deviation is fully closed (i.e. SPEC-aligned) and the tracking issue is
   marked resolved, OR
2. The deviation is explicitly accepted ("won't fix — accepted deviation") in
   which case it moves under [Deliberate extensions](#deliberate-extensions).

Closing all of D1–D5 (or moving them to accepted-deviation status) is what
would let the project legitimately describe itself as a Symphony Go port
rather than "inspired by Symphony".
