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
| D1  | Agent runner protocol and session model (Codex app-server, events, timeouts, tokens)                 | §Agent Runner, §10, §7.1, §13.5, §5.3.6                   | High     | Closed                  | [#64](https://github.com/xrf9268-hue/aiops-platform/issues/64) / PR [#112](https://github.com/xrf9268-hue/aiops-platform/pull/112) |
| D2  | Orchestrator-side Linear status writes (overlaps D8; see audit)                                   | §1 boundary, §11.5                                        | Medium   | Closed                  | [#14](https://github.com/xrf9268-hue/aiops-platform/issues/14) / PR [#116](https://github.com/xrf9268-hue/aiops-platform/pull/116) |
| D3  | No multi-run reconciliation on restart (precondition: D13)                                        | §retry/reconciliation, §restart recovery                  | Medium   | Closed                  | [#68](https://github.com/xrf9268-hue/aiops-platform/issues/68)                                                                       |
| D4  | WORKFLOW.md multi-path discovery (vs single source); subset of D10                                | §workflow file                                            | Low      | Closed                  | [#72](https://github.com/xrf9268-hue/aiops-platform/issues/72) (supersedes [#69](https://github.com/xrf9268-hue/aiops-platform/issues/69)) |
| D5  | Sandbox posture relies solely on Codex CLI sandbox                                                | §safety, §harness hardening                               | Medium   | Closed                  | [#70](https://github.com/xrf9268-hue/aiops-platform/issues/70) / [#114](https://github.com/xrf9268-hue/aiops-platform/issues/114) / PR [#115](https://github.com/xrf9268-hue/aiops-platform/pull/115) |
| D6  | Postgres-backed queue (vs tracker+filesystem recovery); partial of D21                            | §restart recovery, §orchestrator runtime state            | High     | Reverting               | [#73](https://github.com/xrf9268-hue/aiops-platform/issues/73)                                                                       |
| D7  | Gitea webhook ingress (vs tracker polling)                                                        | §triggers                                                 | Medium   | Closed                  | [#74](https://github.com/xrf9268-hue/aiops-platform/issues/74)                                                                       |
| D8  | Orchestrator does PR creation / git push / Linear status writes (should be agent tools)           | §1 boundary, §tools                                       | **P0**   | Closed                  | [#76](https://github.com/xrf9268-hue/aiops-platform/issues/76)                                                                       |
| D9  | No per-tick reconciliation; in-flight runs did not stop on tracker state change (stall: see D14)  | §2.1 Goals, §8.5                                          | P1       | Closed                  | [#78](https://github.com/xrf9268-hue/aiops-platform/issues/78) / PR [#131](https://github.com/xrf9268-hue/aiops-platform/pull/131) |
| D10 | Workflow file is per-service, not per-repo-per-task (supersedes D4)                               | §5.1, §17.7                                               | High     | Closed                  | [#84](https://github.com/xrf9268-hue/aiops-platform/issues/84)                                                                       |
| D11 | Dynamic WORKFLOW.md watch/reload was unimplemented                                                | §6.2                                                      | High     | Closed                  | [#85](https://github.com/xrf9268-hue/aiops-platform/issues/85) / PR [#133](https://github.com/xrf9268-hue/aiops-platform/pull/133) |
| D12 | Workspace lifecycle hooks lacked complete ordering/cleanup semantics                              | §5.3.4, §9.4, §18.1                                       | High     | Closed                  | [#86](https://github.com/xrf9268-hue/aiops-platform/issues/86) / PR [#134](https://github.com/xrf9268-hue/aiops-platform/pull/134) / [#146](https://github.com/xrf9268-hue/aiops-platform/issues/146) |
| D13 | Workspace key by `<repo>_<task_id>` instead of sanitized issue identifier; sanitizer realigned to SPEC §4.2 in [#229](https://github.com/xrf9268-hue/aiops-platform/issues/229); reuse + `after_create` realignment in [#245](https://github.com/xrf9268-hue/aiops-platform/issues/245) | §4.2, §9.1, §9.2, §9.4, §17.2                              | High     | Closed                  | [#87](https://github.com/xrf9268-hue/aiops-platform/issues/87), [#229](https://github.com/xrf9268-hue/aiops-platform/issues/229), [#245](https://github.com/xrf9268-hue/aiops-platform/issues/245), follow-ups [#308](https://github.com/xrf9268-hue/aiops-platform/issues/308) / [#310](https://github.com/xrf9268-hue/aiops-platform/issues/310) |
| D14 | Stall detection (`stall_timeout_ms` / `turn_timeout_ms` / `read_timeout_ms`) was missing on the Codex app-server runner path | §5.3.6, §8.5 Part A                                       | Medium   | Closed                  | [#88](https://github.com/xrf9268-hue/aiops-platform/issues/88)                                                                       |
| D15 | Per-state concurrency (`max_concurrent_agents_by_state`)                                         | §5.3.5, §8.3                                              | Medium   | Closed                  | [#89](https://github.com/xrf9268-hue/aiops-platform/issues/89) / closed follow-up [#159](https://github.com/xrf9268-hue/aiops-platform/issues/159) |
| D16 | Retry used fixed delays instead of exponential backoff + 1s continuation retries                  | §7.3, §8.4, §16.6                                         | High     | Closed                  | [#90](https://github.com/xrf9268-hue/aiops-platform/issues/90) / PR [#140](https://github.com/xrf9268-hue/aiops-platform/pull/140) |
| D17 | `strings.ReplaceAll` template vs Liquid-strict; `attempt` variable never threaded                 | §5.4, §12.2, §17.1                                        | Medium   | Closing via PR #161     | [#91](https://github.com/xrf9268-hue/aiops-platform/issues/91) / [#161](https://github.com/xrf9268-hue/aiops-platform/pull/161)      |
| D18 | Issue domain model missing `priority` / `labels` / `blocked_by` / `branch_name` / timestamps      | §4.1.1                                                    | Medium   | Closing via PR          | [#92](https://github.com/xrf9268-hue/aiops-platform/issues/92)                                                                       |
| D19 | Candidate selection, `Todo` blocker rule, and dispatch sorting were incomplete                    | §8.2                                                      | High     | Closed                  | [#93](https://github.com/xrf9268-hue/aiops-platform/issues/93) / PR [#141](https://github.com/xrf9268-hue/aiops-platform/pull/141) |
| D20 | Linear GraphQL lacked `project.slugId` filtering, pagination, and `[ID!]` state-refresh query; narrow state refresh now feeds poll-tick reconcile | §11.2                                                     | High     | Closing via PR          | [#94](https://github.com/xrf9268-hue/aiops-platform/issues/94) / PR [#142](https://github.com/xrf9268-hue/aiops-platform/pull/142) / [#148](https://github.com/xrf9268-hue/aiops-platform/issues/148) |
| D21 | Single-source-of-truth orchestrator state absent (umbrella; D6 is partial)                        | §3.1, §4.1.8, §7.4, §13.7                                | High     | Closed                  | [#95](https://github.com/xrf9268-hue/aiops-platform/issues/95) / PR [#117](https://github.com/xrf9268-hue/aiops-platform/pull/117) / [#150](https://github.com/xrf9268-hue/aiops-platform/issues/150) |
| D22 | Run attempt phases (§7.2) and runtime events (§10.4) not modeled (depends on D1)                  | §7.2, §10.4                                               | Medium   | Closing via PR          | [#96](https://github.com/xrf9268-hue/aiops-platform/issues/96)                                                                       |
| D23 | `linear_graphql` client-side tool surface is wired through Codex app-server dynamic tools; SPEC §15.5 harness narrowing (mutation allow-list + audit event) implemented in [#298](https://github.com/xrf9268-hue/aiops-platform/issues/298) | §10.5, §15.5                                              | Medium   | Closing via PR          | [#97](https://github.com/xrf9268-hue/aiops-platform/issues/97) / PR [#116](https://github.com/xrf9268-hue/aiops-platform/pull/116) / [#298](https://github.com/xrf9268-hue/aiops-platform/issues/298) |
| D24 | Workflow front-matter schema: aiops keys + missing core (`hooks`, `polling`, `workspace.root`)    | §5.3                                                      | Medium   | Closing via PR; note relative `workspace.root` now resolves from the `WORKFLOW.md` directory | [#98](https://github.com/xrf9268-hue/aiops-platform/issues/98)                                                                       |
| D25 | Multi-service `WORKFLOW.md` routing schema is an implementation-defined extension documented in [`docs/workflows/services-routing.md`](docs/workflows/services-routing.md) | §5.3 extension note, §8.2, §11.2                           | Low      | Closing via PR          | [#143](https://github.com/xrf9268-hue/aiops-platform/issues/143) / [#16](https://github.com/xrf9268-hue/aiops-platform/issues/16) |
| D26 | Codex input-required / MCP elicitation sessions retried instead of being held as operator-visible blocked claims | §10.5, §13.7; Elixir PR [#66](https://github.com/openai/symphony/pull/66) | P1       | Closing via PR          | [#183](https://github.com/xrf9268-hue/aiops-platform/issues/183)                                                                      |
| D27 | Capacity-full failure-retry armed a 100 ms re-fire timer instead of rescheduling through the configured backoff with a typed "no available orchestrator slots" error | §8.4, §16.6; upstream `handle_active_retry` (orchestrator.ex:1142-1161) | Medium   | Closed                  | [#306](https://github.com/xrf9268-hue/aiops-platform/issues/306)                                                                      |
| D28 | `DefaultConfig` ships `tracker.kind: gitea` instead of enforcing SPEC §6.4's REQUIRED semantics; the other five §6.4 defaults (`codex.command`, `agent.max_concurrent_agents`, `workspace.root`, `tracker.active_states`, `tracker.terminal_states`) are now SPEC-aligned. Follow-up: a coordinated test-fixture sweep across ~60 minimal-front-matter `internal/workflow` tests is needed before `tracker.kind` can become REQUIRED at the loader. | §6.4 | Low | Partial — see [`DECISION.md` §6.4 default-value alignment](DECISION.md) | [#244](https://github.com/xrf9268-hue/aiops-platform/issues/244) |
| D29 | `agent.max_retry_attempts` / `agent.max_timeout_retries` caps and the `OrchestratorState.Failed` map (+ `ReleaseFailedIfIssueChanged`) are non-SPEC harness extensions. Defaults are now SPEC-aligned: absent caps yield unbounded retries with the SPEC §8.4 exponential-backoff ceiling, and `Failed` only collects entries that are non-retryable by some other contract (verify gate, secret scan, continuation budget) or that exceeded an explicitly opt-in cap. Explicit positive `max_retry_attempts` / `max_timeout_retries` opt into the SPEC §15.5 harness-hardening cap; explicit `0` disables the corresponding retry path entirely. | §4.1.8, §8.4, §15.5, §16.6 | Low | Partial — opt-in caps retained as documented SPEC §15.5 harness extension | [#215](https://github.com/xrf9268-hue/aiops-platform/issues/215) |
| D30 | Orchestrator continuation-spawn cap survives for legacy one-shot runners (`codex` exec, shell-based `claude`, mocks) where the runner cannot enforce `agent.max_turns` inside a session. SPEC §7.1 leaves continuation worker spawns unbounded; the cap is a harness-engineering safety net for runners that don't honor SPEC §5.3.5 in-session turn enforcement, and is one of the "non-retryable by some other contract" exits referenced by D29's `Failed` accounting. For `codex app-server` (SPEC §10.1 default) the orchestrator no longer caps continuations — the runner's in-session loop is the sole turn budget, and the orchestrator keeps re-dispatching fresh sessions until tracker state changes. Gated by `runner.EnforcesMaxTurnsInternally`. | §5.3.5, §7.1, §10.1, §15.5 | Low | Closed (app-server path SPEC-aligned; legacy cap documented) | [#216](https://github.com/xrf9268-hue/aiops-platform/issues/216) |

Severity reflects the risk and the gap to SPEC, not the implementation effort.

D15 is closed because per-state caps are implemented and enforced; the active-state refresh edge case during reconcile was tracked separately in closed follow-up [#159](https://github.com/xrf9268-hue/aiops-platform/issues/159).

Status vocabulary:

- **Open**: no merged implementation has materially closed the deviation.
- **Partial**: at least one prerequisite or sub-behavior has landed, but the
  linked issue remains open because the full acceptance criteria are not met.
- **Reverting**: the current behavior is still present and is intentionally
  scheduled for removal/replacement.
- **Closed**: the linked implementation issue is closed; leave the row visible
  until umbrella #67 closes so future audits can see which D1–D24 items were
  resolved.

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
>   tracked as D7 and closed by #74 after replacing webhook ingress with
>   tracker polling.
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
