# aiops-platform vs upstream Symphony — same-flow E2E comparison

**Date:** 2026-06-03
**Goal:** reproduce the [autonomous E2E run](2026-06-03-binary-linear-codex-autonomous-e2e.md) on the **upstream reference** (`openai/symphony`, Elixir) with the *same* prep-from-zero flow, to find the root cause of the defects filed as #621–#624 — are they aiops bugs or inherited Symphony behavior?

**Upstream build:** `openai/symphony` @ HEAD, `elixir/bin/symphony` (escript) built with system Elixir 1.19.5 / OTP 29.
**Same flow:** fresh disposable GitHub repo `xrf9268-hue/symphony-todo-web-e2e-20260603223904` + identical 10 issue specs (6 feature, 2 blocking, 2 exception) + fresh Linear project `6ed4e24d1100` (AIS-71…80) + `max_concurrent_agents: 2`, `max_turns: 12`, real `codex app-server` (gpt-5.5).

## Headline

**Our worst defect (#621, the unbounded continuation loop) is mandated by SPEC §7.1 and reproduces identically on upstream Symphony — aiops did not introduce it, but "SPEC says so / upstream does it too" is not correctness.** The upstream reference loops the unfinishable issues exactly the way aiops did: turns climb to `max_turns`, the orchestrator re-dispatches a *fresh* session, `blocked` never fires, and quota burns unbounded (15.95M tokens). Both are faithful to SPEC §7.1, which leaves continuation worker spawns **unbounded** — and that is precisely a case where SPEC itself is operationally defective.

**Root cause (corrected — it is *not* the approval policy):** `max_turns` bounds a single in-session loop; the orchestrator then re-dispatches active issues with **no cumulative-turn / no-forward-progress budget across sessions** (SPEC §7.1; for the default `codex app-server` runner aiops has never capped continuations — D30 / #216). The only SPEC terminator is `blocked` via §16.5 `input_required`, which is **insufficient**: it fires only on approval/elicitation/sandbox events, so logically-unfinishable issues complete *normal* turns and loop regardless of approval policy. aiops once carried safety-net caps (#576 continuation-spawn, #577 retry) and **deleted them in the #568 sweep to align with SPEC §7.1** — trading hardening for fidelity (note #576's cap never covered `codex app-server` anyway). This is the lesson, not a one-off bug — see "Lessons learned".

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
| `approval_policy` default (codex-schema alignment — **orthogonal to the loop**) | **`{"granular": {all flags false}}`** = auto-reject every approval/elicitation, migrated to the **current** codex variant (`loader.go:664`, #329) | **`{"reject": {…}}`** (`config/schema.ex:162`) — the **obsolete** variant | **aiops is AHEAD of upstream here**: codex renamed `reject`→`granular` + flipped polarity; aiops migrated (#329), upstream did not |
| `reject` / `granular` on the **local** codex | `granular` accepted | **`reject` rejected: `-32600 unknown variant 'reject', expected untrusted/on-failure/on-request/granular/never`** | upstream's default approval config **does not run** on the current codex; to run upstream at all we set `never`. This is an upstream/codex **alignment** gap (relevant to #624), **not** the cause of the loop |
| MCP PR tool elicitation (#624) | **prompted interactively** despite the granular auto-reject default (the tool-call approval is apparently not one of the 5 granular flags) | under `never`: **auto-approved**, no prompt (AIS-72/73 used the `[codex]` MCP tool fine) | a specific tool-call approval type slips past aiops's granular flags; needs a current-schema audit of which approval kinds the flags cover |
| `counts` schema | `running, blocked, retrying, completed, agent_handoff_reconcile_stopped, reconcile_stopped_with_progress` | **`running, blocked, retrying` only** | aiops added the completed/handoff buckets (#617); **#623's misleading "Completed" KPI is aiops-introduced** |
| Dashboard | "Worker status" (#619 redesign) | **"Operations Dashboard"** (what aiops had pre-#619) | aiops diverged from upstream in #619 |

## Per-finding verdict

- **#621 (continuation loop) — SPEC §7.1-mandated defect; the fix is a deliberate deviation, not a bug patch.** Both implementations re-dispatch a fresh continuation session for any still-active issue with no cross-session budget; `max_turns` bounds only the in-session loop. For the default `codex app-server` runner aiops has **never** capped continuations (D30 / #216 — the cap that existed was scoped to legacy runners that don't self-enforce `max_turns`). aiops *did* carry safety-net caps and **deleted them in the #568 over-design sweep** — #576 (continuation-spawn cap, legacy-runner-only) and #577 (`max_retry_attempts` retry-count cap + `Failed` map) — explicitly to align with SPEC §7.1 / §8.4's unbounded model. The SPEC terminator (`blocked` via §16.5 `input_required`) is **necessary but insufficient**: it only fires on approval/elicitation/sandbox events, so logically-unfinishable issues (X1 contradictory tests, X2 unreachable host, and even the secret-needing B1/B2) complete *normal* turns and loop **regardless of approval policy** — this is **not** caused by, nor fixable by, the `approval_policy` (aiops already runs the granular auto-reject default). **Actionable fix:** re-introduce a worker-side cumulative-turn / no-forward-progress budget across re-dispatches for `codex app-server`, tracked as a justified `DEVIATIONS.md` row — i.e. consciously override SPEC §7.1 because §7.1 is operationally defective (unbounded quota burn + slot starvation). This reverses part of the #576/#577 alignment removals on the grounds that SPEC is wrong here.
- **#622 (operator cancel reverted) — shared race, confirmed.** Cancels only stuck once the agent process stopped (4/4 held after symphony was killed; 6/6 In Review untouched). The same reconcile-vs-agent-write window exists upstream; aiops amplifies it via the WORKFLOW prompt re-setting In Progress each turn. Keep open; the platform should treat an operator terminal transition as authoritative.
- **#623 (misleading Completed KPI) — aiops-specific, confirmed.** Upstream has **no** `completed` counter (its `counts` is `{running, blocked, retrying}` only); the whole completed/handoff/reconcile-stopped surface is aiops's #617 extension, and that is where the misleading top-line lives. Keep as an aiops issue.
- **#624 (MCP approval gate) — current-codex approval audit.** aiops's default *is* an auto-reject granular policy, yet the `codex_apps` GitHub PR tool still prompted interactively — so that tool-call approval kind is not covered by the 5 granular flags. Under `never` (upstream) it auto-approves with no prompt. Fix = audit which approval/elicitation kinds the **current** codex emits and ensure the granular policy (or an equivalent, current-schema-validated posture) covers the tool-call path, so no unattended elicitation can park a run. Do not regress to upstream's obsolete `reject` object (`-32600`).

## Lessons learned

1. **SPEC/upstream alignment is not a proxy for correctness.** The headline defect (#621) is *mandated* by SPEC §7.1 and reproduced identically by the upstream reference — yet it is a real operational defect (unbounded quota burn: 15.95M tokens on 4 stuck issues; concurrency starvation: 0 dispatches for the queued issues). Aligning faithfully to a defective SPEC reproduces the defect. When SPEC itself is wrong, the harness-engineering mandate ("be the *hardened* port") should **override** alignment — implemented as a deliberate, justified `DEVIATIONS.md` row, not silently matched. Upstream's own README says it is "prototype … harden it yourself."
2. **The #568 "delete to align" sweep removed real hardening.** #576/#577 deleted continuation/retry caps to match SPEC §7.1/§8.4. The principle-6 heuristic ("upstream absence ⇒ over-design, delete it") has a genuine exception this run exposes: **upstream absence can instead mean upstream is under-hardened.** Before deleting a safety net to align, ask whether SPEC's position is itself the defect.
3. **Don't conflate independent threads.** Two separate stories got tangled mid-investigation: (a) the unbounded-continuation loop (SPEC §7.1, orthogonal to approval policy), and (b) codex approval-schema drift (`reject`→`granular`). aiops is **ahead** of upstream on (b) — it migrated to the current `granular` variant (#329) while upstream's default `reject` is dead on the current codex (`-32600`). (b) is about #624; it is *not* the cause of the loop. An earlier draft of this report wrongly attributed the loop to a permissive approval policy — corrected here.
4. **Reading the actual config/history beats inference.** The "aiops ran permissive" assumption was wrong (its default is a blocking granular policy); the "add a budget" fix turned out to already exist and have been deliberately removed (#576/#577). The operator's pointer to the prior cap-removal work, plus reading `loader.go` + the #216/#576/#577 history, corrected both.

## Evidence

Under `docs/validation/assets/upstream/`:
- `sym-dash-01-running2.png` — upstream "Operations Dashboard", concurrency `running:2`
- `sym-dash-02-runaway-loop.png` — `Blocked 0` while AIS-77/78 churn turns; 15.79M tokens / 62m
- `04-runaway-loop-AIS77-78.json` — `/api/v1/state` during the loop (`counts: running 2, blocked 0, retrying 0`)
- upstream worker log: `~/symphony-e2e-20260603223904/logs/log/symphony.log.1` (the `-32600`, `Reached agent.max_turns`, `scheduling active-state continuation` lines)

## Closeout

Symphony stopped; AIS-77–80 Canceled (4 Canceled + 6 In Review). No orphaned codex children. Secret scan clean. Disposable repo/project external to this repo.
