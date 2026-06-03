# aiops-platform vs upstream Symphony — same-flow E2E comparison

**Date:** 2026-06-03
**Goal:** reproduce the [autonomous E2E run](2026-06-03-binary-linear-codex-autonomous-e2e.md) on the **upstream reference** (`openai/symphony`, Elixir) with the *same* prep-from-zero flow, to find the root cause of the defects filed as #621–#624 — are they aiops bugs or inherited Symphony behavior?

**Upstream build:** `openai/symphony` @ HEAD, `elixir/bin/symphony` (escript) built with system Elixir 1.19.5 / OTP 29.
**Same flow:** fresh disposable GitHub repo `xrf9268-hue/symphony-todo-web-e2e-20260603223904` + identical 10 issue specs (6 feature, 2 blocking, 2 exception) + fresh Linear project `6ed4e24d1100` (AIS-71…80) + `max_concurrent_agents: 2`, `max_turns: 12`, real `codex app-server` (gpt-5.5).

## Headline

**Our worst defect (#621, the unbounded continuation loop) is a genuine defect that exists in upstream Symphony too — aiops did not introduce it, but "upstream has it" is not absolution.** Run with the same permissive approval posture, the upstream reference loops the unfinishable issues exactly the way aiops did: turns climb to `max_turns`, the orchestrator re-dispatches, `blocked` never fires, and quota burns unbounded (15.95M tokens). aiops faithfully ported the behavior — and since aiops's mandate (AGENTS.md harness-engineering) is to be the *hardened* port, it should fix this past upstream's prototype behavior ("prototype … harden it yourself", per upstream's own README).

The proximate trigger is the permissive `approval_policy`, but the **deeper defect (present in both) is that nothing bounds the re-dispatch chain when an active issue keeps completing turns without progress** — and the `blocked` safety net is both insufficient (it only catches approval/elicitation, not logical-impossibility) and fragile (it depends on a codex approval schema that has already drifted out from under upstream). See the per-finding verdict.

## What ran identically

| Dimension | aiops-platform | upstream Symphony | Verdict |
|---|---|---|---|
| Feature delivery | 6/6 → In Review + draft PRs #11–#16 | 6/6 → In Review + draft PRs #11–#16 | **equivalent** |
| Unfinishable issues (B1/B2/X1/X2) | unbounded continuation loop; `blocked` never reached | **identical** unbounded loop; `blocked` stayed 0 | **same — inherited** |
| `max_turns` role | caps one invocation; loop re-dispatches across invocations | caps one invocation (`turn 1→12 → reset → 1`); loop re-dispatches | **same mechanism** |
| Loop log signature | `Succeeded → re-dispatch` | `Continuing agent run … turn=N/12` → `Reached agent.max_turns … returning control to orchestrator` → `scheduling active-state continuation` | same shape |
| Token burn on the loop | 13.79M / 68 agent-min | **15.95M / 62 min** (capped by operator) | same magnitude |
| Concurrency starvation | AIS-70 got **0** dispatches | AIS-79/80 got **0** dispatches | **same** |
| Operator tracker-cancel | reverted by running agent; held only after worker stop | cancels held only after symphony stopped (same race window) | same race |

## What differs (and why)

| Dimension | aiops-platform | upstream Symphony | Root cause |
|---|---|---|---|
| `approval_policy` default | not set (ran `danger-full-access`) | **`{"reject":{…mcp_elicitations…}}`** (`config/schema.ex:162`) | upstream's reject-on-stuck → `input_required` → **BLOCKED** is the loop-terminator |
| `reject` on the **local** codex | n/a | **rejected: `-32600 unknown variant 'reject', expected untrusted/on-failure/on-request/granular/never`** | upstream's safety net **does not run** on the codex aiops targets; had to fall back to `never` (auto-approve) → loop |
| MCP PR tool elicitation | **prompted interactively** (#624 stall) | under `never`: **auto-approved**, no prompt (AIS-72/73 used the `[codex]` MCP tool fine) | aiops sat in the "neither approve nor reject" middle state |
| `counts` schema | `running, blocked, retrying, completed, agent_handoff_reconcile_stopped, reconcile_stopped_with_progress` | **`running, blocked, retrying` only** | aiops added the completed/handoff buckets (#617); **#623's misleading "Completed" KPI is aiops-introduced** |
| Dashboard | "Worker status" (#619 redesign) | **"Operations Dashboard"** (what aiops had pre-#619) | aiops diverged from upstream in #619 |

## Per-finding verdict

- **#621 (continuation loop) — genuine defect, present in upstream too; aiops should harden past it.** Not an aiops-introduced regression (upstream reproduces it identically), but a real Symphony design gap in both. `max_turns` bounds a single invocation; the orchestrator re-dispatches active issues indefinitely with **no cumulative-turn / no-forward-progress budget across re-dispatches** — that missing budget is the core defect, and neither implementation has it. The `blocked` path is **necessary but not sufficient**: it only fires on codex `input_required` (approval/elicitation/sandbox denial), so logically-unfinishable issues (X1 contradictory tests, X2 unreachable host) end each turn as a *normal completion* and would loop **even under a rejecting `approval_policy`** (empirically, the secret-needing blocking issues also completed normal turns under `never` and never raised `input_required`). And the safety net is **fragile**: upstream's default `approval_policy: {"reject": …}` is **rejected by the codex we target** (`-32600 unknown variant 'reject'`) — upstream is pinned to an older Codex schema, and its README admits supported values are version-dependent. **Actionable (robust) fix:** a worker-side no-forward-progress / cumulative-turn budget across re-dispatches — codex-version-independent. Do **not** make the loop-terminator depend on a specific codex `approval_policy` variant (drift-prone per AGENTS.md cross-cutting checklist #3, and insufficient anyway); a current-schema-validated blocking policy (`on-request`/`untrusted`) is at most a complementary partial mitigation.
- **#622 (operator cancel reverted) — shared race, confirmed.** Cancels only stuck once the agent process stopped (4/4 held after symphony was killed; 6/6 In Review untouched). The same reconcile-vs-agent-write window exists upstream; aiops amplifies it via the WORKFLOW prompt re-setting In Progress each turn. Keep open; the platform should treat an operator terminal transition as authoritative.
- **#623 (misleading Completed KPI) — aiops-specific, confirmed.** Upstream has **no** `completed` counter; the whole completed/handoff/reconcile-stopped surface is aiops's #617 extension, and that is where the misleading top-line lives. Keep as an aiops issue.
- **#624 (MCP approval gate) — config root cause, confirmed.** Under `never` the same MCP PR tool auto-approves with no prompt. aiops's effective policy neither approved nor rejected the elicitation, so it stalled. Fix = a definite approval posture (reject preferred; never acceptable) so no unattended elicitation can park a run.

## Evidence

Under `docs/validation/assets/upstream/`:
- `sym-dash-01-running2.png` — upstream "Operations Dashboard", concurrency `running:2`
- `sym-dash-02-runaway-loop.png` — `Blocked 0` while AIS-77/78 churn turns; 15.79M tokens / 62m
- `04-runaway-loop-AIS77-78.json` — `/api/v1/state` during the loop (`counts: running 2, blocked 0, retrying 0`)
- upstream worker log: `~/symphony-e2e-20260603223904/logs/log/symphony.log.1` (the `-32600`, `Reached agent.max_turns`, `scheduling active-state continuation` lines)

## Closeout

Symphony stopped; AIS-77–80 Canceled (4 Canceled + 6 In Review). No orphaned codex children. Secret scan clean. Disposable repo/project external to this repo.
