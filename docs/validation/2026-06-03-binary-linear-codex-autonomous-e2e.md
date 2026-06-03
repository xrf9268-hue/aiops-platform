# Autonomous binary + Linear + Codex E2E validation

**Date:** 2026-06-03
**Validated binary:** `main` @ `c42c33d` (built `-trimpath -ldflags="-s -w"`, `aiops-platform/0.136.0`), includes #616 (codex turn-start stall diagnostics), #617 (agent-handoff reconcile stops), #619 (lean Worker-status dashboard).
**Deployment:** binary path — per-user launchd `com.aiops-platform.worker.e2e`, loopback HTTP on `127.0.0.1:4104`, real `codex app-server` (gpt-5.5, `danger-full-access`), real Linear.
**Disposable repo:** `xrf9268-hue/aiops-todo-web-e2e-20260603184332`
**Disposable Linear project:** `aiops e2e todo web 20260603184332` (`project_slug: df869f28afef`, team AIS)
**Agent config:** `max_concurrent_agents: 2`, `max_turns: 12`, `timeout: 45m`, `active_states: [Todo, In Progress]`, `terminal: [Done, Canceled, Duplicate]`, `inactive: [Backlog, In Review]`.

The operator's role was **prepare + observe only** — create the repo/issues/config, trigger the first poll, capture evidence, and perform closeout. No intervention in agent work (no code edits, no lifecycle advancement, no PR merges). The two operator actions taken — canceling pathological issues and stopping the worker at closeout — are documented below as closeout, and one of them surfaced a finding.

## Scope

Drove the worker autonomously over **10 GitHub issues** (specs live in the disposable repo; each Linear issue is a thin pointer the agent reads via `gh issue view`), engineered to exercise every requested scenario:

| Linear | GH# | Class | Intent |
|---|---|---|---|
| AIS-61…66 | #1…#6 | feature | concurrency + a real web UI + normal handoff |
| AIS-67 | #7 | blocking | needs `SENDGRID_API_KEY` (unavailable) |
| AIS-68 | #8 | blocking | needs `FLY_API_TOKEN` + Fly app (unavailable) |
| AIS-69 | #9 | exception | self-contradictory acceptance (impossible tests) |
| AIS-70 | #10 | exception | test pings unreachable `*.invalid` host |

## Timeline (from `/api/v1/state`, counts-change only)

```
18:50  run=2  hand=0  done=0    AIS-61,62 dispatched (concurrency; capacity gate holds the other 8)
18:55  run=2  hand=2            AIS-61,62 → handoff;  AIS-63,64 dispatched
19:00  run=2  hand=4            AIS-63,64 → handoff;  AIS-65,66 dispatched
19:07  run=2  hand=6            all 6 features handed off; AIS-67,68 (blocking) dispatched
19:09  run=2  hand=6  done=1→2  blocking issues begin completing turns (NOT blocking)
19:11–19:16   done=3→8         AIS-67/68 continuation loop; ret=1 blips are kind=continuation (1s)
[operator cancels AIS-67/68 @ 19:16 to bound the runaway]
19:17  hand=7                  AIS-68 reaped → handoff; AIS-69 (exception) dispatched
19:18–19:22  done=9→12         AIS-67 KEEPS running after cancel (reverted itself); AIS-69 loops too
[operator bootout worker @ ~19:22 — the only reliable stop]
final  done=12  hand=7  blk=0  ret(failure)=0  tokens=13.79M  agent-runtime=68min
```

## Scenario coverage

| Requested | Observed | Evidence |
|---|---|---|
| **Concurrency** | ✅ `running:2` sustained; capacity gate held 8 Todo issues back, dispatched as slots freed | `aiops-dash-01-running2.png`, `tui/01-running2.txt`, `state/01` |
| **Web UI (dashboard)** | ✅ new #619 "Worker status" dashboard, incl. #617 reconcile roll-up | `aiops-dash-0{1,2,3}.png` |
| **Web UI (the app itself)** | ✅ agents built a working Express+vanilla-JS todo app (PR #11 API + #13 UI assembled, 7 tests green) | `todo-app-ui.png` |
| **TUI** | ✅ `cmd/tui --raw` rendered agents/handoffs/tokens/backoff live | `tui/0{1,2}.txt` |
| **Normal handoff** | ✅ 6/6 features → draft PR + Linear `Todo→In Progress→In Review` | PRs #11–#16, `linear-board-01-inflight.png` |
| **Blocking** | ⚠️ did **not** reach `blocked` state — degenerated into continuation loop (Finding 1) | `state/04`, logs |
| **Exception/retry** | ⚠️ no `failure`-retry; same continuation loop; one exception issue **starved** (0 dispatches) | Finding 1, re-dispatch counts |

The autonomous loop **works** for the happy path: 6 independent features ran 2-at-a-time, each produced a focused draft PR and the agent-owned Linear lifecycle, with zero operator help. The board screenshot (`linear-board-01-inflight.png`) captures the mid-run state: 4 In Review, 2 In Progress, 4 Todo, 0 Done.

## Findings

### Finding 1 — Unfinishable issues degenerate into an unbounded continuation loop; `blocked` / `failure`-retry are never reached (HIGH)

All four pathological issues (2 blocking, 2 exception) exhibited the **same** failure mode. The codex turn exits `Succeeded` regardless of whether the task is actually impossible; the agent keeps the issue in `In Progress` (an `active_state`); per the SPEC §16.5 continuation loop the worker immediately re-dispatches. `completed_total` therefore counts **turn-completions, not issue-completions**, and climbs without bound.

Re-dispatch (`runner_start`) counts in this run:

```
AIS-61 (feature):           1     ← features dispatch once, then hand off cleanly
AIS-65 (feature):           1
AIS-67 (B1 SendGrid):       8     ← continuation loop
AIS-68 (B2 Fly.io):         5
AIS-69 (X1 contradiction):  2
AIS-70 (X2 unreachable):    0     ← never ran: starved by the loopers holding both slots
total runner_start: 21   total Succeeded: 12 (== done=12)
```

One AIS-68 cycle (each ~30–110s, indefinitely): `runner_start → … → Succeeded → PreparingWorkspace → BuildingPrompt → runner_start → …`. The `retrying` blips in the timeline are `kind=continuation` (the 1s continuation delay), **not** `kind=failure`. Net effect: unbounded codex quota burn (13.79M tokens / 68 agent-min, still climbing at closeout), concurrency-slot starvation (AIS-70 got 0 turns), and the `blocked` state — the documented home for "agent is waiting on an external dependency" — was never entered despite two issues whose acceptance was explicitly unsatisfiable.

Note the configuration is already correct per [[project_active_states_must_exclude_handoff_states]] (`In Review` is excluded from `active_states`). The gap is that an agent which *cannot* finish has no inactive state to move a genuinely-blocked issue to (`In Review` means "done, awaiting human"; there is no `Blocked` state in the active/inactive config), and the worker has no per-issue progress/turn-budget across re-dispatches to detect "no forward progress."

### Finding 2 — Operator cancellation via the tracker is overridden by the running agent; runaway runs are not reliably stoppable from the tracker (HIGH)

When the operator moved AIS-67 to `Canceled` (a terminal state) at 19:16 to bound the loop, **the running agent reverted it**: its next turn issued `linear_graphql issueUpdate(→ In Progress)` (the WORKFLOW prompt instructs the agent to move the issue to In Progress at the start of every turn). Log — `issueUpdate` calls for AIS-67 continued **after** the cancel:

```
19:15:27  issueUpdate   (before cancel)
[operator cancel @ 19:16]
19:17:21  issueUpdate   ← re-activates the issue
19:19:22  issueUpdate
19:23:12  issueUpdate
```

AIS-67 stayed `In Progress` and kept looping for ~6 minutes and ~4 more turns after being canceled; the only reliable stop was a worker `launchctl bootout`. (AIS-68 *did* stop on cancel — but only because reconcile happened to reap it in the narrow window before its agent's next turn.) This directly contradicts the operator mental model ("cancel a ticket and the agent stops on the next poll"): the D9 reconcile-cancel races, and loses to, continuation re-dispatch + the agent's own lifecycle write.

### Finding 3 — Successful handoffs are recorded as `agent_handoff_reconcile_stopped`, not `completed`; the dashboard "Completed" KPI is misleading (MEDIUM)

All 6 delivered features (draft PRs #11–#16, issues moved to In Review) were recorded under `agent_handoff_reconcile_stopped` (= 6, then 7), and **`completed` stayed 0** for them. They move to In Review (inactive) and are reconcile-cancelled before a clean turn-completion is recorded → the handoff bucket. Meanwhile the `completed` counter accumulated the **looping pathological** turns (reaching 12). The headline dashboard "Completed" card therefore reads `0` while 6 PRs shipped, then climbs on stuck work — backwards from operator intuition. The data is present (the #617 "Reconcile roll-up" panel lists the handoffs), but the top-line KPI mis-summarizes delivery.

### Finding 4 — Agent tool-choice inconsistency: one agent used the Codex GitHub MCP tool, triggering an interactive approval gate that breaks unattended autonomy (LOW–MEDIUM)

5 of 6 feature agents opened their PR with `gh pr create` (the WORKFLOW-prescribed path; PR titles `AIS-XX …`, no prompt). AIS-65's agent instead called `mcp__codex_apps__github__create_pull_request` (PR #16 title `[codex] …`), which raised an interactive Codex approval dialog (`finding-codex-mcp-pr-approval-gate.png`). In a truly unattended deployment nobody clicks it; the run would stall until the `timeout: 45m` (or the #616 turn-start-stall diagnostic) fires. The harness should constrain the agent to the sanctioned `gh` path or auto-approve the MCP tool in the worker's codex context.

## Final state

- **Linear:** AIS-61…66 `In Review` (6 draft PRs awaiting human review — the intended end state); AIS-67…70 `Canceled` (operator closeout).
- **GitHub:** 6 draft PRs `#11`–`#16` on `ai/<linear-issue-id>` branches, each `Closes #<n>`.
- **Metrics at closeout:** `completed_total=12` (turn-completions), `agent_handoff_reconcile_stopped=7`, `blocked=0`, `failure-retry=0`, `reconcile_stopped_with_progress=0`, codex `13.79M` tokens / `68` agent-min.

## Issues filed

- **#621** (p1) — Finding 1: unfinishable issues → unbounded continuation loop; `blocked`/`failure`-retry never reached.
- **#622** (p1) — Finding 2: operator tracker-cancel reverted by the running agent; runaway not stoppable from the tracker.
- **#623** (p2) — Finding 3: misleading "Completed" KPI (deliveries land in `agent_handoff_reconcile_stopped`).
- **#624** (p2) — Finding 4: agent's Codex GitHub MCP PR tool triggers an interactive approval gate.

## Evidence

All under `docs/validation/assets/`:

- `aiops-dash-01-running2.png` — concurrency (`running:2`), new #619 dashboard
- `aiops-dash-02-handoff.png` — #617 reconcile roll-up after feature handoffs
- `aiops-dash-03-zombie-exception.png` — post-cancel zombie AIS-67 + exception AIS-69
- `linear-board-01-inflight.png` — Linear kanban mid-run (4 In Review / 2 In Progress / 4 Todo)
- `todo-app-ui.png` — the agent-built todo app's own web UI (running locally)
- `finding-codex-mcp-pr-approval-gate.png` — Finding 4 approval dialog
- `tui/01-running2.txt`, `tui/02-handoff.txt` — `cmd/tui --raw` frames
- `state/01,04,06,07-*.json` — curated `/api/v1/state` snapshots (baseline, runaway loop, zombie, final)
- worker log: `~/Library/Application Support/aiops-platform/e2e-20260603184332/logs/worker.err.log`

## Closeout

- Canceled AIS-67/68 (bound the runaway), then bootout the worker (the only reliable stop), then canceled AIS-67/69/70 (sticks once no agent runs). No orphaned codex children remained.
- Secret scan: `LINEAR_API_KEY` value absent from all committed evidence. ✅
- Disposable draft PRs #11–#16 left open for inspection (disposable repo); dispose per normal follow-through.
- The `com.aiops-platform.worker.e2e` launchd job is left **stopped** (was pointed at this run's disposable project). Re-point + restart if the standing e2e deployment is wanted again.
