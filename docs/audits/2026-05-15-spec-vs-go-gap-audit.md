# SPEC.md vs aiops-platform Go code — gap audit

**Date:** 2026-05-15
**Audit base:** `aiops-platform` working tree at `main` (commit `8171b05`, two un-tracked-only changes), SPEC.md @ openai/symphony main (saved to `/tmp/symphony-spec/SPEC.md`, 2169 lines).
**Existing deviations cross-referenced:** D1–D9 in `DEVIATIONS.md`.

## Method

Read SPEC.md in full (sections 1–18 + appendix A) and built a checklist of every `MUST` / `REQUIRED` / `MUST NOT` clause plus the `SHOULD`s in the conformance checklist (§17–§18). Then read every non-test `.go` file under `cmd/` and `internal/` (about 3.5k LOC) and mapped each clause to: present (A), known deviation D1–D9 (B), new deviation D10+ (C), silent (D), or deliberate extension (E). When a single clause spans both a known and a new gap (e.g. SPEC §10 requires app-server *and* tool advertisement *and* token isolation; D1 covers app-server but not tool advertisement), the new gap is listed separately so its scope is not hidden under D1's label.

## Summary

- Total SPEC `MUST`/`REQUIRED` clauses examined: ~58
- Aligned (A): 6
- Known deviations confirmed (D1–D9): 7 (D2/D6/D7/D8 still accurate; D1, D3, D9 accurate but understated; D4 stale; D5 accurate)
- New deviations proposed (D10–D24): 15
- Silent / unimplemented (D): 12 (listed under "Silent areas")
- Deliberate extensions found (E): 9 (none survive AGENTS.md "name the behavior it delivers" test cleanly)

## Confirmed existing deviations

| ID | Still accurate? | Notes |
|----|-----------------|-------|
| D1 | Accurate but understated | Description says "agent protocol: `codex exec` vs `codex app-server`". Confirmed: `internal/runner/codex.go:101-122` shells out `codex exec --full-auto …` (one-shot) and `internal/runner/shell.go:35` runs `sh -lc <command> < .aiops/PROMPT.md`. The gap is wider than the title suggests — it also drags in: no `thread_id`/`turn_id` extraction, no `session_started` / `turn_completed` events, no continuation turns (SPEC §10.3, §7.1), no `read_timeout_ms` / `stall_timeout_ms`, no rate-limit telemetry, no token accounting (§13.5). The exec-vs-app-server framing only covers the launch contract; the surrounding event/telemetry model is also entirely absent. Recommend renaming D1 to "Agent runner protocol and session model" so reviewers see the full surface. |
| D2 | Accurate | `internal/worker/transitions.go:48-58, 68-104` writes Linear states from the orchestrator, not from agent tools. D2 framing ("not written back after handoff") is now wrong in the opposite direction: writes *are* happening, just from the wrong side of the boundary. Title should be "Orchestrator-side Linear status writes (should be agent tool)" so it does not collide with D8. |
| D3 | Accurate | `internal/worker/run.go:88-138` claims one task per iteration with no startup reconciliation pass; no `fetch_issues_by_states(terminal_states)` call exists anywhere. |
| D4 | Stale | `DEVIATIONS.md` says multi-path discovery (`WORKFLOW.md`, `.aiops/`, `.github/`) is "being reverted under #72". Code still has all three paths: `internal/workflow/resolver.go:34-38` (`resolveCandidates`). Status should remain Reverting but mark the revert as not-yet-landed; the table entry implies it is in flight. |
| D5 | Accurate | `internal/runner/codex.go:101-122` relies entirely on codex CLI's `--full-auto` / `--dangerously-bypass-approvals-and-sandbox`; no OS isolation. SPEC §15.5 lets implementations choose, but D5 already flags that the harness has no defense-in-depth layer. |
| D6 | Accurate | `internal/queue/postgres.go` and `migrations/` are still in tree; `cmd/worker/main.go:29-35` opens a pool unconditionally. SPEC §14.3 says scheduler state is in-memory; this is structural drift. |
| D7 | Accurate | `cmd/trigger-api/main.go` + `internal/triggerapi/handlers.go:15-69` is the webhook path; SPEC §8 / §16 polls only. |
| D8 | Accurate (P0) | `internal/worker/runtask.go:281-307` calls `workspace.CommitAndPush` + `CreatePR` + `OnPRCreated` from the orchestrator. SPEC §1 boundary, §11.5 tracker writes, §10.5 dynamic tools all say these are agent responsibilities. |
| D9 | Accurate but understated | `internal/worker/run.go:88-138` has no per-tick reconciliation, so SPEC §8.5 "Stop active runs when issue state changes make them ineligible" is unimplemented. The deviation entry covers this. What's also missing: stall detection (§8.5 Part A) and stall_timeout_ms entirely — see new deviation D14. |

> **Aside on the "scrubbed deviations" block in DEVIATIONS.md.** The note that retires "Runner abstraction supporting `mock`/`codex`/`claude`" as "not a deviation at all" stands. SPEC §10 is explicit that the codex protocol is the source of truth but does not forbid additional runner backends — the deviation is the protocol shape (D1), not the existence of multiple runners.

## New deviations proposed (D10+)

### D10 — Workflow path: explicit/cwd precedence vs repo-relative multi-path discovery
- **SPEC reference:** §5.1: "Workflow file path precedence: 1. Explicit application/runtime setting (set by CLI startup path). 2. Default: `WORKFLOW.md` in the current process working directory."
- **Go code reference:** `cmd/worker/main.go:16-23` only accepts `--print-config` then takes config from env; the worker loop calls `workflow.Resolve(workdir)` per task (`internal/worker/runtask.go:151`), which searches **inside the cloned workdir** for `WORKFLOW.md`, `.aiops/WORKFLOW.md`, `.github/WORKFLOW.md`. There is no CLI workflow path argument at all, and the cwd-default rule is not applied.
- **Gap:** SPEC mandates a *process-level* workflow file selected at startup (CLI arg or cwd) that defines the orchestrator's runtime config. The Go worker treats `WORKFLOW.md` as a *per-repo, per-task* file living inside the issue workspace, so it is loaded *after* the task is claimed and is scoped to one repo. This collides with SPEC §6.2 (the same file is supposed to drive dynamic reload of polling cadence, concurrency, etc. for the whole service) and §17.7 (CLI accepts a positional workflow path argument).
- **Severity:** High — structural. Every SPEC subsystem that reads from "the workflow" assumes there is one workflow file per service, not per repo.
- **Suggested status:** Open
- **Tracking issue:** TBD

### D11 — Dynamic WORKFLOW.md watch/reload is unimplemented
- **SPEC reference:** §6.2: "The software MUST detect `WORKFLOW.md` changes. On change, it MUST re-read and re-apply workflow config and prompt template without restart. … Invalid reloads MUST NOT crash the service; keep operating with the last known good effective configuration."
- **Go code reference:** No fsnotify / inotify / poll loop touches `WORKFLOW.md`. `grep -r 'watch\|fsnotify\|reload'` over `internal/` and `cmd/` returns nothing. `internal/worker/runtask.go:151` re-resolves the workflow on every task because the file is repo-scoped (see D10), but there is no service-level reload.
- **Gap:** Every `MUST` in §6.2 is unimplemented. Concrete consequences: editing `polling.interval_ms` requires restart; editing `agent.max_concurrent_agents` requires restart; editing the prompt requires restart and only takes effect on future tasks.
- **Severity:** High (one of the few §6 `MUST` clauses; failing this means the implementation does not pass §17.1).
- **Suggested status:** Open
- **Tracking issue:** TBD

### D12 — Workspace hooks (`after_create` / `before_run` / `after_run` / `before_remove`) are unimplemented
- **SPEC reference:** §5.3.4 + §9.4 + §18.1: "Workspace lifecycle hooks (`after_create`, `before_run`, `after_run`, `before_remove`)" listed as REQUIRED for conformance, with timeout semantics and failure-mode contract.
- **Go code reference:** `internal/workflow/config.go:5-15` (`Config` struct) has no `Hooks` field. `internal/workspace/manager.go:104-142` (`PrepareGitWorkspace`) has no hook dispatch. `grep -r 'after_create\|before_run\|after_run\|before_remove'` returns no hits.
- **Gap:** Entire SPEC §9.4 missing. Workspaces are git worktrees only; there is no way for an operator to bootstrap deps, run pre-flight, post-flight cleanup, or before-remove guards via the workflow file.
- **Severity:** High (Section 18.1 REQUIRED checklist item).
- **Suggested status:** Open
- **Tracking issue:** TBD

### D13 — Workspace key derivation: SPEC says sanitized issue identifier, Go uses `<repo>_<task_id>`
- **SPEC reference:** §4.2 + §9.1 + §9.2: "Per-issue workspace path: `<workspace.root>/<sanitized_issue_identifier>`. Workspaces are reused across runs for the same issue."
- **Go code reference:** `internal/workspace/manager.go:91-94`:
  ```go
  func (m *Manager) PathFor(t task.Task) string {
      repo := sanitize(t.RepoOwner + "_" + t.RepoName)
      return filepath.Join(m.Root, repo, t.ID)
  }
  ```
  And `internal/queue/postgres.go:19-21` makes `t.ID = "tsk_" + nanos` — a fresh ID per enqueue.
- **Gap:** SPEC says workspaces are keyed by **issue identifier** so reruns of the same issue land in the same directory (and §9.1 "Workspaces are reused across runs for the same issue"). Go keys workspaces by **task ID**, where each enqueue is a new ID, so the same Linear/Gitea issue gets a fresh worktree every poll. This breaks the "preserved across runs" invariant and §9.2 step 4's `created_now` distinction. It also makes the `before_remove` hook unreachable even if D12 were closed, since workspaces are torn down per task and never reused.
- **Severity:** Medium-High (interacts with D3 — without persisted workspaces, startup terminal cleanup has nothing to clean).
- **Suggested status:** Open
- **Tracking issue:** TBD

### D14 — Stall detection (`codex.stall_timeout_ms`) and turn timeout (`turn_timeout_ms`) are unimplemented
- **SPEC reference:** §5.3.6 + §8.5 Part A: "If `elapsed_ms > codex.stall_timeout_ms`, terminate the worker and queue a retry."
- **Go code reference:** `internal/workflow/config.go` has no `codex.stall_timeout_ms` / `read_timeout_ms` / `turn_timeout_ms` fields at all. The only timeout is `agent.timeout` on `AgentConfig` (single global per-runner timeout, `internal/workflow/config.go:63`). `internal/runner/codex.go:33-86` uses one `exec.CommandContext` with the parent ctx deadline; there is no inactivity-since-last-event detector.
- **Gap:** A wedged codex that doesn't print but doesn't exit will run until `agent.timeout`; SPEC wants a much shorter event-inactivity detector independent of total turn timeout.
- **Severity:** Medium (operational; the failure mode is real — codex CLI can hang on a stuck approval prompt with `--full-auto` off).
- **Suggested status:** Open
- **Tracking issue:** TBD

### D15 — Per-state concurrency (`max_concurrent_agents_by_state`) is unimplemented
- **SPEC reference:** §5.3.5 + §8.3: "`max_concurrent_agents_by_state[state]` if present (state key normalized) otherwise fallback to global limit."
- **Go code reference:** `internal/workflow/config.go:55-73` `AgentConfig` has `MaxConcurrentAgents` only; no `ByState` map. Concurrency is also not actually enforced inside the worker — `internal/worker/run.go:89-99` is single-threaded by virtue of `store.Claim` returning one task per iteration; SQL is `LIMIT 1 ... FOR UPDATE SKIP LOCKED`. Multi-worker setups would scale via additional processes, not via the orchestrator's concurrency limit.
- **Gap:** §8.3 specifies that the *runtime counts issues by current tracked state* in the running map and gates dispatch; Go has no in-memory running map (D6 explains why) and therefore cannot implement per-state caps.
- **Severity:** Medium
- **Suggested status:** Open (depends on D6 reversion)
- **Tracking issue:** TBD

### D16 — Exponential backoff and continuation retries are unimplemented
- **SPEC reference:** §7.3 + §8.4 + §16.6: "Normal continuation retries after a clean worker exit use a short fixed delay of `1000` ms. Failure-driven retries use `delay = min(10000 * 2^(attempt - 1), agent.max_retry_backoff_ms)`."
- **Go code reference:** `internal/queue/postgres.go:146-153`:
  ```go
  UPDATE tasks SET status='queued', available_at=now()+interval '60 seconds' …
  ```
  Both `Fail` and `FailTimeout` use a fixed 60-second delay. There is no `max_retry_backoff_ms` config field, no power-of-two computation, no continuation-retry concept ("if issue is still active, retry in 1s on the same thread").
- **Gap:** SPEC's continuation-retry primitive — successful exit -> short delay -> re-check tracker -> potentially same thread, next turn — is the whole engine behind "the agent keeps working until the issue moves out of active". Go has no such loop; once a task succeeds, the queue marks it `'succeeded'` and the issue is forgotten until a fresh poll re-enqueues it.
- **Severity:** High — this is the "Symphony continuously watches the task board and ensures every active task has an agent running in the loop until it's done" behavior from the announcement post. Without it, the system is closer to a one-shot CI runner than to Symphony.
- **Suggested status:** Open
- **Tracking issue:** TBD

### D17 — Liquid-strict prompt rendering and `attempt` variable are unimplemented
- **SPEC reference:** §5.4 + §12.2: "Use a strict template engine (Liquid-compatible semantics are sufficient). Unknown variables MUST fail rendering. Unknown filters MUST fail rendering."
- **Go code reference:** `internal/workflow/template.go:16-26` is `strings.ReplaceAll`. Unknown variables silently leave `{{ var }}` literal in the prompt. The `attempt` variable is never threaded — `internal/worker/runtask.go:174-182` renders with only `task.id/title/description/actor/repo.owner/name/branch` and never an `attempt` integer.
- **Gap:** §17.1 "Prompt rendering fails on unknown variables (strict mode)" is unimplemented (the conformance test would fail). §12.3 retry/continuation semantics are unreachable because the `attempt` value never enters the template.
- **Severity:** Medium (workflows that rely on `{{ attempt }}` to produce different retry prompts are silently broken).
- **Suggested status:** Open
- **Tracking issue:** TBD

### D18 — Issue domain model is missing `priority`, `branch_name`, `labels`, `blocked_by`, `created_at`, `updated_at` semantics
- **SPEC reference:** §4.1.1: `Issue` fields include `priority`, `branch_name`, `labels` (lowercase), `blocked_by` (list of refs with id/identifier/state), `created_at`, `updated_at`. §8.2 candidate selection uses `priority` + `created_at` + `blocked_by` rules.
- **Go code reference:** `internal/tracker/tracker.go:5-13`:
  ```go
  type Issue struct {
      ID, Identifier, Title, Description, URL, State, UpdatedAt string
  }
  ```
  Only `UpdatedAt` is present (as a string), used solely for Rework dedupe. No priority, no labels, no blockers.
- **Gap:** The Linear GraphQL query in `internal/tracker/linear.go:29-33` does not request these fields at all. SPEC §8.2 dispatch sort (`priority ASC, created_at oldest first, identifier lexicographic`) cannot be implemented against this model; the queue sorts by `priority DESC, created_at ASC` from the *task* table which is not the same thing (queue priority is a per-task scheduling knob, not the tracker priority).
- **Severity:** Medium (one of the easier gaps to close: extend the GraphQL query + struct; but it touches the candidate-selection algorithm, which is also missing — see D19).
- **Suggested status:** Open
- **Tracking issue:** TBD

### D19 — Candidate selection, `Todo` blocker rule, and dispatch sort are unimplemented
- **SPEC reference:** §8.2: "An issue is dispatch-eligible only if … Blocker rule for `Todo` state passes: if the issue state is `Todo`, do not dispatch when any blocker is non-terminal."
- **Go code reference:** No equivalent. `cmd/linear-poller/main.go:53-93` polls Linear and enqueues whatever Linear returns; the dispatch decision is delegated to the SQL `Claim` query, which has no blocker awareness and uses queue priority not tracker priority.
- **Gap:** The poller cannot filter on blockers because it never asks for them (D18); the worker cannot enforce per-state slot limits because there's no in-memory running map (D6); the sort key is wrong (D18). All three feed into the same algorithm gap.
- **Severity:** Medium-High (affects correctness of which issues actually get worked on).
- **Suggested status:** Open
- **Tracking issue:** TBD

### D20 — Linear GraphQL: missing `project.slugId` filter, missing pagination, missing `[ID!]` typing for state refresh
- **SPEC reference:** §11.2: "Candidate issue query filters project using `project: { slugId: { eq: $projectSlug } }`. Issue-state refresh query uses GraphQL issue IDs with variable type `[ID!]`. Pagination REQUIRED for candidate issues. Page size default: `50`."
- **Go code reference:** `internal/tracker/linear.go:29-33`:
  ```graphql
  issues(filter: { state: { name: { in: $states } } }, first: 50)
  ```
  No project filter, no `pageInfo`, no `endCursor` loop. There is no state-refresh query at all (there is no reconciliation pass — D9). The `MoveIssueToState` mutation `internal/tracker/linear.go:86-88` uses `$stateId: String!` not `[ID!]`, but this is a different operation; the SPEC `[ID!]` requirement is on the *state-refresh* query that doesn't exist.
- **Gap:** Polls miss issues beyond the first 50, and (because no project filter is applied) polls fetch issues from across the workspace rather than the configured project.
- **Severity:** High (silent data correctness on any non-trivial Linear workspace).
- **Suggested status:** Open
- **Tracking issue:** TBD

### D21 — Single-source-of-truth orchestrator state is absent
- **SPEC reference:** §3.1 + §4.1.8 + §7.4: "The orchestrator is the only component that mutates scheduling state. All worker outcomes are reported back to it and converted into explicit state transitions." `OrchestratorRuntimeState` contains `running`, `claimed`, `retry_attempts`, `completed`, `codex_totals`, `codex_rate_limits`.
- **Go code reference:** There is no orchestrator struct at all. `cmd/worker/main.go:25-35` is a Postgres-backed queue consumer. State lives in the `tasks` and `task_events` SQL tables (`internal/queue/postgres.go`), and concurrency is enforced by `FOR UPDATE SKIP LOCKED` rather than by an in-process authority.
- **Gap:** SPEC §7.4 "serializes state mutations through one authority" is implemented by the database transaction model instead, which has different semantics — e.g. there is no `claimed` set distinct from `running`, no `retry_attempts` map distinct from `available_at`, no aggregate `codex_totals`. The "Orchestrator" component in §3.1 is not present in the Go code.
- **Severity:** High — this is the structural reason most of the other deviations exist.
- **Suggested status:** Open (this is the umbrella that D6 partly addresses)
- **Tracking issue:** TBD

### D22 — Run attempt phases (§7.2) and emitted runtime events (§10.4) are not modeled
- **SPEC reference:** §7.2 lists 11 phases (`PreparingWorkspace`, `BuildingPrompt`, `LaunchingAgentProcess`, `InitializingSession`, `StreamingTurn`, `Finishing`, `Succeeded`, `Failed`, `TimedOut`, `Stalled`, `CanceledByReconciliation`). §10.4 lists agent runtime events (`session_started`, `turn_completed`, `turn_failed`, `turn_input_required`, `notification`, `malformed`, …).
- **Go code reference:** `internal/task/task.go:23-40` defines an event vocabulary (`enqueued`, `claimed`, `workflow_resolved`, `runner_start`, `runner_end`, `runner_timeout`, `verify_start`, `verify_end`, `push`, `pr_created`, `pr_reused`, `succeeded`, `failed_attempt`, plus the three `tracker_transition*` extensions). No `session_*`, no `turn_*`, no `stalled`, no `canceled_by_reconciliation`.
- **Gap:** The Go event vocabulary is shaped around the orchestrator-runs-everything workflow (push, pr_created, verify_*) rather than the agent-runs-everything workflow SPEC describes. Closing D1/D8 implies replacing most of this vocabulary.
- **Severity:** Medium (downstream observability and §17.5 conformance tests).
- **Suggested status:** Open
- **Tracking issue:** TBD

### D23 — `linear_graphql` client-side tool is absent; the orchestrator instead embeds Linear writes
- **SPEC reference:** §10.5: "Optional client-side tool extension: `linear_graphql`. … Reuse the configured Linear endpoint and auth from the active Symphony workflow/runtime config; do not require the coding agent to read raw tokens from disk."
- **Go code reference:** `internal/tracker/linear.go` exports `MoveIssueToState` and `AddComment`; these are called *by the orchestrator* (`internal/worker/transitions.go:48-104`), not advertised to the agent as a tool. There is no app-server tool advertisement code path because there is no app-server client (D1).
- **Gap:** Different from D8 in scope: D8 says the *writes* are on the wrong side; D23 says the *tool surface* SPEC defines as optional is also missing. Without it, a SPEC-aligned workflow that says "use `linear_graphql` to move to Human Review" has no tool to call. The orchestrator-side write is a workaround that only works for the in-built transitions the worker happens to know about.
- **Severity:** Medium (extension-level, but it is the SPEC's canonical solution to the token-isolation requirement in §15.3).
- **Suggested status:** Open
- **Tracking issue:** TBD (related to #76)

### D24 — Workflow front-matter schema has aiops-specific top-level keys not in SPEC
- **SPEC reference:** §5.3: "Top-level keys: `tracker`, `polling`, `workspace`, `hooks`, `agent`, `codex`. Unknown keys SHOULD be ignored for forward compatibility. … Extensions MAY define additional top-level keys without changing the core schema above."
- **Go code reference:** `internal/workflow/config.go:5-15` adds `repo`, `claude`, `policy`, `verify`, `pr` as siblings of `tracker`/`agent`/`codex`, and *omits* `polling`, `workspace.root` defaults from §5.3.3, `hooks` entirely (D12), and the `codex.*` policy/timeout sub-fields (D14).
- **Gap:** The extensions (`repo`, `claude`, `policy`, `verify`, `pr`) are SPEC-permitted in principle (§5.3 note), but each needs to survive the AGENTS.md "behavior first" test. The omissions (`hooks`, `polling.interval_ms` under the `polling` key, `workspace.root`, `codex.*` sub-fields) are not extensions — they are missing core schema. The `polling.interval_ms` key is currently spelled `tracker.poll_interval_ms` (`internal/workflow/config.go:31`); a workflow file written against SPEC would not parse correctly.
- **Severity:** Medium (existing workflow files in the wild that follow SPEC's key names would silently lose their settings).
- **Suggested status:** Open
- **Tracking issue:** TBD

## Silent areas

SPEC requirements with no corresponding Go code at all. Listed even when overlapping with a D10+ entry because each is a distinct test target in §17.

- **§3.1 "Status Surface" / §13.7 HTTP server / dashboard / `/api/v1/*` endpoints** — `internal/triggerapi/` exists but exposes the Postgres queue ingestion API, not the SPEC observability surface (`GET /api/v1/state`, `GET /api/v1/<identifier>`, `POST /api/v1/refresh`).
- **§4.1.6 `LiveSession` / token accounting** — fields `session_id`, `thread_id`, `turn_id`, `codex_input/output/total_tokens`, `last_reported_*_tokens`, `turn_count` are not tracked; SPEC §13.5 token-accounting rules have nothing to read from.
- **§4.1.7 / §8.4 `RetryEntry`** — there is no `retry_attempts` map; retries are encoded as a deferred `available_at` on the queue row, not as a typed entry with `attempt`, `due_at_ms`, `timer_handle`, `error`.
- **§5.5 typed error classes** (`missing_workflow_file`, `workflow_parse_error`, `workflow_front_matter_not_a_map`, `template_parse_error`, `template_render_error`) — `internal/workflow/loader.go:19-48` returns wrapped raw errors; no typed sentinels.
- **§6.3 dispatch preflight validation** — startup config validation runs once in `internal/workflow/loader.go:87-104` (only schema, not tracker-reachable, not codex-found, not API-key-resolved); per-tick re-validation does not exist.
- **§8.5 Part A stall detection + Part B tracker-state refresh** — neither implemented (overlaps D9, D14).
- **§8.6 startup terminal workspace cleanup** — unimplemented (overlaps D3, D13).
- **§10.4 emitted events**: `session_started`, `startup_failed`, `turn_completed`, `turn_failed`, `turn_cancelled`, `turn_ended_with_error`, `turn_input_required`, `approval_auto_approved`, `unsupported_tool_call`, `notification`, `other_message`, `malformed` (overlaps D22).
- **§10.5 user-input-required policy** — there is no Codex protocol path, so the question of how to handle user-input-required never arises; D1 makes this silent rather than wrong.
- **§13.5 rate-limit tracking** — no agent event ingestion path.
- **§14.3 partial state recovery (restart)** — D6's Postgres queue partially substitutes by persisting tasks, but the SPEC model is "rebuild from tracker + filesystem on restart". The Go worker re-reads tasks from Postgres on restart, not the tracker, so the recovery semantics diverge — terminal-state cleanup is not run, retry timers are not reseeded, and the workspace fleet is not pruned against the tracker.
- **§17.7 CLI conformance** — `cmd/worker/main.go` has no positional `path-to-WORKFLOW.md` argument, no `./WORKFLOW.md` cwd default, and produces no clean startup-failure exit codes distinct from runtime exits.

## Deliberate extensions

Each entry calls out the behavior the component delivers and whether it survives AGENTS.md's "name the behavior it delivers" / SPEC-alignment-is-a-hard-requirement test.

| File:line | Behavior | Survives AGENTS.md test? |
|-----------|----------|--------------------------|
| `internal/queue/postgres.go` (entire file) | Persistent task queue across restarts. | No — SPEC §14.3 explicitly says no DB; D6 already flags. |
| `internal/triggerapi/handlers.go:15-70`, `internal/gitea/webhook.go` | Gitea webhook ingest for issue comments containing `/ai-run`. | No — SPEC §8.1 polls; D7 flags. |
| `internal/workspace/manager.go:96-142` (`PrepareGitWorkspace`) + `internal/workspace/mirror.go` | Bare-mirror git cache + per-task worktree to avoid re-cloning large repos. | Partial — the mirror cache is a defensible perf optimisation, but the per-task (not per-issue) worktree key is D13. |
| `internal/workspace/manager.go:155-254` (`RunVerify`) | Runs `verify.commands` after the runner and gates PR creation on success. | Survives — names a behavior (block PR on red verify) SPEC's "scheduler/runner" boundary does not preclude. But SPEC's verify is the *agent's* responsibility per §1, so this is structurally on the wrong side; it should be a workflow `hooks.before_run` / a tool, not an orchestrator phase. |
| `internal/workspace/secretscan.go` | Pre-push secret scan with structured event payloads. | Same as RunVerify — defensible behavior, wrong side of the agent boundary. |
| `internal/workspace/manager.go:283-358` (`CheckSummary`) + `internal/worker/runtask.go:231-264` | Reject PR creation when the runner didn't write `RUN_SUMMARY.md`. | Tied to D8 — once PR creation moves to the agent (#76), the orchestrator has no PR to gate, so this disappears. |
| `internal/policy/policy.go` | Path-glob deny/allow + max-changed-files/lines policy enforcement. | Survives the "name the behavior" test, but SPEC §15.5 (Harness hardening) frames this as a deployment-specific control, not an orchestrator phase. Better expressed as a workflow extension key (D24) than as a hard-coded orchestrator step. |
| `cmd/worker/main.go:17-23` (`--print-config`) | Debug subcommand that prints the resolved workflow + masked secrets. | Survives — pure observability, SPEC §13 is permissive. The only concern is that it documents the multi-path resolver (D4) and thereby normalises that deviation. |
| `internal/worker/transitions.go` + Linear `MoveIssueToState` / `AddComment` | Orchestrator-driven Linear state transitions and failure comments. | No — SPEC §1 + §11.5 say tracker writes belong to the agent; D2 + D8 cover. |

In aggregate, eight of the nine extensions either fail the SPEC-alignment test (1, 2, 3-partial, 6, 9) or sit on the wrong side of the agent boundary (4, 5, 7). Only `--print-config` survives cleanly.

## Verdict

**1. Is the gap count bounded such that continuing the Go port is feasible?**

No. Including the 9 already-known deviations, the audit identifies 15 new ones (D10–D24), of which 7 (D10, D11, D12, D16, D20, D21, D24) are SPEC-`MUST` failures rather than `SHOULD` drift. Several are structural rather than additive: D10/D11 (workflow file is per-service not per-repo and must hot-reload), D13 (workspace identity is per-issue not per-task), D16 (continuation-retry loop is *the* engine), and D21 (single in-memory orchestrator authority) cannot be patched in — they reshape the worker loop, the queue, the workspace manager, and the tracker client. After the reshape, very little of the current Go code stays put: `internal/queue` deletes, `internal/triggerapi` and `internal/gitea/webhook.go` delete (or shrink to a healthcheck), `internal/worker/run.go` and `runtask.go` get rewritten, `internal/workspace/manager.go` gets re-keyed, `internal/runner/codex.go` and `shell.go` get rewritten for app-server, `internal/tracker/linear.go` gets a state-refresh query and pagination, and a brand-new orchestrator state machine module appears. The pieces that genuinely survive are `internal/policy`, `internal/workspace.RunVerify`, `internal/workspace.secretscan`, the `WORKFLOW.md` loader+template (after switching to a real template engine), and the mock runner — maybe 800 of the current 3,500 non-test LOC.

**2. Is the cumulative drift large enough that forking the Elixir demo is cheaper?**

Yes, and `DECISION.md` already reaches this conclusion on independent grounds; this audit corroborates it with concrete file:line evidence. The gap profile splits cleanly: 80% of the work is *replacing* the orchestrator core (D1, D6, D8, D9, D10, D11, D13, D14, D16, D17, D19, D21, D22, D23) — which is the same code that would be inherited fresh from an upstream fork — and 20% is *adding* deltas (Gitea adapter, the harness gates that survive). Cloning a working reference and adding the deltas costs ~20% of the surface; rewriting the orchestrator core to match the reference costs ~80%.

**Calibration of alignment effort if the Go port continues anyway:**

- **1-week scope (impossible).** No combination of D10–D24 is closable in a week. Even the cheapest individual gap (D17, switch to a strict template engine and thread `attempt`) takes 1–2 days end-to-end with tests. The interdependent set (D10/D11, D16, D21) demands a redesign discussion before any code lands.
- **4-week scope (≈ MVP-aligned).** Realistic targets: close D11 (fsnotify reload of a per-service WORKFLOW.md, ~4 days), D17 (strict template engine, 2 days), D18+D20 (full Linear domain model + project filter + pagination, 4 days), D24 (schema cleanup, 2 days), plus three of {D12, D14, D15, D19}. Leaves D1, D6, D8, D9, D10, D13, D16, D21, D22, D23 unaddressed — which means the core loop is still SPEC-non-aligned.
- **12-week scope (full SPEC alignment).** Requires writing a Symphony orchestrator in Go from scratch: in-memory state machine, app-server client (study `elixir/lib/symphony_elixir/codex/app_server.ex`), per-issue workspace lifecycle, dynamic reload, tool advertisement including `linear_graphql`, and a §13.7 HTTP surface. This is the Go-rewrite-of-Elixir-reference work `DECISION.md §"Why not just finish the Go port"` rejects, and the rejection holds: that 12 weeks of AI-written-Go-against-Elixir-reference is exactly the verification surface the operator wants to *avoid*. A fork would get there in 1–3 weeks of "graft Gitea + port harness gates" instead.

**Recommendation:** the audit reinforces `DECISION.md`. Forking a working Symphony implementation and adding Gitea + the surviving harness gates (RunVerify, secret scan, policy, --print-config) is the cheaper path by a wide margin. The Go code under audit has roughly 800 LOC worth of reusable behavioral spec (in its test suite and policy/verify/secret-scan modules); everything else is either compensating for the wrong architecture or implementing a feature SPEC says is owned by the agent.
