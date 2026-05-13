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
| D4 | WORKFLOW.md multi-path discovery (vs single source) | §workflow file | Low | Reverting | [#72](https://github.com/xrf9268-hue/aiops-platform/issues/72) (supersedes [#69](https://github.com/xrf9268-hue/aiops-platform/issues/69)) |
| D5 | Sandbox posture relies solely on Codex CLI sandbox | §safety, §harness hardening | Medium | Open | [#70](https://github.com/xrf9268-hue/aiops-platform/issues/70) |
| D6 | Postgres-backed queue (vs tracker+filesystem recovery) | §restart recovery, §orchestrator runtime state | High | Reverting | [#73](https://github.com/xrf9268-hue/aiops-platform/issues/73) |

Severity reflects the risk and the gap to SPEC, not the implementation effort.

## Deliberate extensions

These are differences from the reference implementation that we have decided
to keep because they offer functional value beyond what SPEC provides. The
bar is high: cosmetic convenience does not qualify (see project posture in
[`AGENTS.md`](AGENTS.md#spec-alignment-is-a-hard-requirement)).

- **Gitea webhook trigger path.** SPEC describes pull-only via tracker
  polling. We accept Gitea issue-comment webhooks as an additional task
  source. Justification: lower latency than poll-and-rate-limit, and
  webhook signature verification is the natural Gitea-side integration
  point. (Under review — file an issue if you disagree.)

> **Removed claims.** Earlier revisions of this file listed
> "PostgreSQL-backed queue", "Runner abstraction supporting
> mock/codex/claude", and "WORKFLOW.md discovered at 3 paths" as
> deliberate extensions. None of those clear the bar:
>
> - Postgres queue: tracked as D6, being reverted under #73.
> - Runner abstraction (mock/codex/claude): SPEC is explicitly
>   agent-agnostic, so this is **not a deviation at all** — it was a
>   documentation error.
> - Multi-path WORKFLOW.md: tracked as D4, being reverted under #72.

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
