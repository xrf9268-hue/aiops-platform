# DEVIATIONS

This document records every known difference between `aiops-platform` and the
upstream Symphony project. Three sources are jointly authoritative:

- [Symphony SPEC.md](https://github.com/openai/symphony/blob/main/SPEC.md) — the
  protocol contract.
- [`openai/symphony` Elixir reference implementation](https://github.com/openai/symphony/tree/main/elixir/lib/symphony_elixir) —
  the working reference. When SPEC text is ambiguous, the Elixir module's
  behavior is the tiebreaker.
- [OpenAI Symphony announcement blog post (2026-04-27)](docs/research/2026-04-27-openai-symphony-blog.md) —
  authoritative design rationale from the authors. Mirrored in this repo so it
  cannot drift.

Practitioner accounts (advisory; not authoritative on SPEC but useful for
operational expectations and prompt-engineering patterns):

- [George's Symphony Electron rewrite thread (2026-05)](docs/research/2026-05-george-symphony-electron-rewrite.md) —
  first-hand operator report (50 tickets → 30 merged PRs overnight), with
  the user-visible behavior model for Rework / Cancel / Backlog transitions.

`aiops-platform` is a Go port of that reference. Per the project posture in
[`AGENTS.md`](AGENTS.md#spec-alignment-is-a-hard-requirement), SPEC alignment
is a hard requirement and the project is pre-release, so the cost of closing
deviations is at its minimum right now.

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
| D7 | Gitea webhook ingress (vs tracker polling) | §triggers | Medium | Reverting | [#74](https://github.com/xrf9268-hue/aiops-platform/issues/74) |
| D8 | Orchestrator does PR creation / git push / Linear status writes (should be agent tools) | §1 boundary, §tools | **P0** | Reverting | [#76](https://github.com/xrf9268-hue/aiops-platform/issues/76) |
| D9 | No per-tick reconciliation; in-flight runs do not stop on tracker state change | §2.1 Goals (stop ineligible runs), §retry/reconciliation | P1 | Reverting | [#78](https://github.com/xrf9268-hue/aiops-platform/issues/78) |

Severity reflects the risk and the gap to SPEC, not the implementation effort.

## Deliberate extensions

There are no current deliberate extensions. The bar is high: SPEC alignment
is a hard requirement (see project posture in
[`AGENTS.md`](AGENTS.md#spec-alignment-is-a-hard-requirement)) and cosmetic
or marginal convenience does not qualify. Anything that fails the bar moves
to the deviations table above and gets a tracking issue for reversal.

> **Removed claims.** Earlier revisions of this file listed several
> "deliberate extensions" that did not survive review under the project's
> SPEC-alignment posture. They are recorded here so the correction is
> auditable:
>
> - **Gitea webhook trigger path** — value (latency, rate limit) was
>   marginal for self-hosted Gitea against minute-scale agent runs;
>   tracked as D7, being reverted under #74.
> - **PostgreSQL-backed queue** — value (concurrent-worker safety) is
>   unused while the codebase assumes single-worker (#68); tracked as
>   D6, being reverted under #73.
> - **Multi-path WORKFLOW.md discovery** — value was cosmetic only;
>   tracked as D4, being reverted under #72.
> - **Runner abstraction supporting `mock` / `codex` / `claude`** — SPEC
>   is explicitly agent-agnostic, so this is **not a deviation at all**.
>   It was a documentation error in #69.

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
