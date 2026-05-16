# DEVIATIONS

This document records every known difference between `aiops-platform` and the
upstream Symphony project. Three sources are jointly authoritative:

- [Symphony SPEC.md](https://github.com/openai/symphony/blob/main/SPEC.md) — the
  protocol contract.
- [`openai/symphony` Elixir reference implementation](https://github.com/openai/symphony/tree/main/elixir/lib/symphony_elixir) —
  the working reference. When SPEC text is ambiguous, the Elixir module's
  behavior is the tiebreaker. **Not** a porting target — see
  [`DECISION.md`](DECISION.md).
- [OpenAI Symphony announcement blog post (2026-04-27)](docs/research/2026-04-27-openai-symphony-blog.md) —
  authoritative design rationale from the authors. Mirrored in this repo so it
  cannot drift.

Practitioner accounts (advisory; not authoritative on SPEC but useful for
operational expectations and harness-engineering posture):

- [George's Symphony Electron rewrite thread (2026-05)](docs/research/2026-05-george-symphony-electron-rewrite.md) —
  first-hand operator report (50 tickets → 30 merged PRs overnight), with
  the user-visible behavior model for Rework / Cancel / Backlog transitions.
- [Addy Osmani's harness-engineering thread (2026-05)](docs/research/2026-05-addy-osmani-harness-engineering.md) —
  the framework we use to evaluate whether a component earns its keep.
  Every Reverting deviation below survives the "name the behavior it
  delivers" test described in
  [`AGENTS.md` §Harness engineering principles](AGENTS.md#harness-engineering-principles).

`aiops-platform` is a Go port of that reference. Per the project posture in
[`AGENTS.md`](AGENTS.md#spec-alignment-is-a-hard-requirement), SPEC alignment
is a hard requirement and the project is pre-release, so the cost of closing
deviations is at its minimum right now.

The umbrella tracking issue is [#67](https://github.com/xrf9268-hue/aiops-platform/issues/67).

The 2026-05-15 gap audit (PR [#82](https://github.com/xrf9268-hue/aiops-platform/pull/82), report at [`docs/audits/2026-05-15-spec-vs-go-gap-audit.md`](docs/audits/2026-05-15-spec-vs-go-gap-audit.md)) is the most recent full sweep against SPEC. It confirmed D1–D9, surfaced D10–D24, and documents 12 silent-area categories. When a row below says "see audit", treat the audit doc as the source of file:line evidence and severity reasoning.

## Deviations

| ID  | Area                                                                                              | SPEC reference                                            | Severity | Status                  | Tracking issue                                                                                                                       |
| --- | ------------------------------------------------------------------------------------------------- | --------------------------------------------------------- | -------- | ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| D1  | Agent runner protocol and session model (codex `exec` vs `app-server`; events, timeouts, tokens)  | §Agent Runner, §10, §7.1, §13.5, §5.3.6                   | High     | Open                    | [#64](https://github.com/xrf9268-hue/aiops-platform/issues/64)                                                                       |
| D2  | Orchestrator-side Linear status writes (overlaps D8; see audit)                                   | §1 boundary, §11.5                                        | Medium   | Open                    | [#14](https://github.com/xrf9268-hue/aiops-platform/issues/14)                                                                       |
| D3  | No multi-run reconciliation on restart (precondition: D13)                                        | §retry/reconciliation, §restart recovery                  | Medium   | Open                    | [#68](https://github.com/xrf9268-hue/aiops-platform/issues/68)                                                                       |
| D4  | WORKFLOW.md multi-path discovery (vs single source); subset of D10                                | §workflow file                                            | Low      | Reverting (not landed)  | [#72](https://github.com/xrf9268-hue/aiops-platform/issues/72) (supersedes [#69](https://github.com/xrf9268-hue/aiops-platform/issues/69)) |
| D5  | Sandbox posture relies solely on Codex CLI sandbox                                                | §safety, §harness hardening                               | Medium   | Open                    | [#70](https://github.com/xrf9268-hue/aiops-platform/issues/70)                                                                       |
| D6  | Postgres-backed queue (vs tracker+filesystem recovery); partial of D21                            | §restart recovery, §orchestrator runtime state            | High     | Reverting               | [#73](https://github.com/xrf9268-hue/aiops-platform/issues/73)                                                                       |
| D7  | Gitea webhook ingress (vs tracker polling)                                                        | §triggers                                                 | Medium   | Reverting               | [#74](https://github.com/xrf9268-hue/aiops-platform/issues/74)                                                                       |
| D8  | Orchestrator does PR creation / git push / Linear status writes (should be agent tools)           | §1 boundary, §tools                                       | **P0**   | Reverting               | [#76](https://github.com/xrf9268-hue/aiops-platform/issues/76)                                                                       |
| D9  | No per-tick reconciliation; in-flight runs do not stop on tracker state change (stall: see D14)   | §2.1 Goals, §8.5                                          | P1       | Reverting               | [#78](https://github.com/xrf9268-hue/aiops-platform/issues/78)                                                                       |
| D10 | Workflow file is per-service, not per-repo-per-task (supersedes D4)                               | §5.1, §17.7                                               | High     | Open                    | [#84](https://github.com/xrf9268-hue/aiops-platform/issues/84)                                                                       |
| D11 | Dynamic WORKFLOW.md watch/reload unimplemented                                                    | §6.2                                                      | High     | Open                    | [#85](https://github.com/xrf9268-hue/aiops-platform/issues/85)                                                                       |
| D12 | Workspace lifecycle hooks (after_create/before_run/after_run/before_remove) unimplemented         | §5.3.4, §9.4, §18.1                                       | High     | Open                    | [#86](https://github.com/xrf9268-hue/aiops-platform/issues/86)                                                                       |
| D13 | Workspace key by `<repo>_<task_id>` instead of sanitized issue identifier                         | §4.2, §9.1, §9.2                                          | High     | PR pending              | [#87](https://github.com/xrf9268-hue/aiops-platform/issues/87)                                                                       |
| D14 | Stall detection (`stall_timeout_ms` / `turn_timeout_ms` / `read_timeout_ms`) unimplemented        | §5.3.6, §8.5 Part A                                       | Medium   | Open                    | [#88](https://github.com/xrf9268-hue/aiops-platform/issues/88)                                                                       |
| D15 | Per-state concurrency (`max_concurrent_agents_by_state`) unimplemented (depends on D21)           | §5.3.5, §8.3                                              | Medium   | Open                    | [#89](https://github.com/xrf9268-hue/aiops-platform/issues/89)                                                                       |
| D16 | Fixed-delay retry vs exponential backoff + 1s continuation retries                                | §7.3, §8.4, §16.6                                         | High     | Open                    | [#90](https://github.com/xrf9268-hue/aiops-platform/issues/90)                                                                       |
| D17 | `strings.ReplaceAll` template vs Liquid-strict; `attempt` variable never threaded                 | §5.4, §12.2, §17.1                                        | Medium   | Open                    | [#91](https://github.com/xrf9268-hue/aiops-platform/issues/91)                                                                       |
| D18 | Issue domain model missing `priority` / `labels` / `blocked_by` / `branch_name` / timestamps      | §4.1.1                                                    | Medium   | Open                    | [#92](https://github.com/xrf9268-hue/aiops-platform/issues/92)                                                                       |
| D19 | Candidate selection, `Todo` blocker rule, dispatch sort unimplemented (depends on D18, D21)       | §8.2                                                      | High     | Open                    | [#93](https://github.com/xrf9268-hue/aiops-platform/issues/93)                                                                       |
| D20 | Linear GraphQL: missing `project.slugId` filter, pagination, `[ID!]` state-refresh query          | §11.2                                                     | High     | Open                    | [#94](https://github.com/xrf9268-hue/aiops-platform/issues/94)                                                                       |
| D21 | Single-source-of-truth orchestrator state absent (umbrella; D6 is partial)                        | §3.1, §4.1.8, §7.4                                        | High     | Open                    | [#95](https://github.com/xrf9268-hue/aiops-platform/issues/95)                                                                       |
| D22 | Run attempt phases (§7.2) and runtime events (§10.4) not modeled (depends on D1)                  | §7.2, §10.4                                               | Medium   | Open                    | [#96](https://github.com/xrf9268-hue/aiops-platform/issues/96)                                                                       |
| D23 | `linear_graphql` client-side tool absent; orchestrator embeds writes instead (related to D8)      | §10.5                                                     | Medium   | Open                    | [#97](https://github.com/xrf9268-hue/aiops-platform/issues/97)                                                                       |
| D24 | Workflow front-matter schema: aiops keys + missing core (`hooks`, `polling`, `workspace.root`)    | §5.3                                                      | Medium   | Open                    | [#98](https://github.com/xrf9268-hue/aiops-platform/issues/98)                                                                       |

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

The 2026-05-15 audit also enumerated nine existing orchestrator-side
behaviors that look like extensions but fail the SPEC-alignment test
(verify gate, secret scan, summary gate, policy enforcement, orchestrator
Linear writes, etc.). They are catalogued in
[`docs/audits/2026-05-15-spec-vs-go-gap-audit.md` §Deliberate extensions](docs/audits/2026-05-15-spec-vs-go-gap-audit.md#deliberate-extensions);
none move to this section, all are covered by existing deviations
(notably D8, D23) or will be re-expressed as workflow extensions /
agent tools as those deviations close.

## How to use this list

If you are auditing SPEC alignment, walk the table top to bottom and check the
linked issue for the latest status. A row stays here until either:

1. The deviation is fully closed (i.e. SPEC-aligned) and the tracking issue is
   marked resolved, OR
2. The deviation is explicitly accepted ("won't fix — accepted deviation") in
   which case it moves under [Deliberate extensions](#deliberate-extensions).

Closing all of D1–D24 (or moving each to accepted-deviation status) is what
would let the project legitimately describe itself as a Symphony Go port
rather than "inspired by Symphony".
