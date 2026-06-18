# Design: trace-driven harness improvement loop

Status: proposed
Issue: #931
Scope: design, inventory, and milestone plan only. This document does not add
a worker phase, durable scheduler state, tracker write, PR writer, prompt
rewriter, or merge gate.

## Problem

LangChain's loop-engineering vocabulary names a fourth loop: production traces
from agent runs are grouped, diagnosed, and turned into harness improvements.
aiops-platform already has the agent loop, reviewer/verification loop, and
event-driven tracker loop. What is missing is a SPEC-aligned workflow that turns
recurring run evidence into reviewable changes to repo-owned harness surfaces.

The danger is implementing L4 at the wrong layer. Symphony's core contract keeps
the worker as a scheduler/runner/tracker reader. Harness changes should ship as
ordinary reviewed artifacts: a report, issue, or draft PR against `WORKFLOW.md`,
reviewer rubrics, `LEARNINGS.md`, skills, hooks, tests, CI, or docs.

## Authority and constraints

- [SPEC section 1](../research/SPEC.md) is authoritative: Symphony is a
  scheduler/runner and tracker reader; ticket writes are handled by the coding
  agent through workflow/runtime tools.
- [SPEC section 13](../research/SPEC.md) requires operator-visible observability
  but does not prescribe a durable trace store. `/api/v1/state` is an optional
  current-runtime snapshot, not scheduler state.
- The Elixir reference keeps orchestration in an in-process GenServer state and
  folds Codex app-server events into that live state. It has no worker-owned
  post-turn verifier, PR writer, tracker writer, or harness rewrite phase.
- LangChain Engine and Zach Lloyd's Skills loop are advisory product patterns:
  recurring traces or run records are diagnosed, a fix is proposed, evaluators
  may be added, and recurrence can reopen work. In this repo, those steps must
  produce reviewable repo artifacts before changing runtime behavior.

## Research inputs

- [Symphony SPEC](../research/SPEC.md), especially sections 1, 7, 10.4, 11.5,
  and 13.
- Upstream Elixir reference modules:
  `orchestrator.ex`, `codex/app_server.ex`, `tracker.ex`, and
  `config/schema.ex`.
- [LangChain loop-engineering note](../research/2026-06-16-langchain-art-of-loop-engineering.md)
  and the current LangSmith Engine docs, used as advisory vocabulary for
  recurring trace grouping and evaluator proposals.
- [Zach Lloyd Skills loop note](../research/2026-06-16-zach-lloyd-self-improving-skills.md),
  used as advisory evidence that outer loops should propose ordinary
  file-based harness diffs.
- Current runbooks for
  [runtime debugging](../runbooks/task-api.md),
  [runtime status](../runbooks/runtime-status.md),
  [reviewer worker](../runbooks/reviewer-worker.md), and
  [workflow authoring](../runbooks/workflow-authoring.md).

## Adjacent path audit

Existing paths already supply L4 inputs, but most are not normalized into one
durable dataset:

- `internal/task/task.go` names the worker event vocabulary. Tracker write event
  constants are deliberately negative-assertion vocabulary: the worker must not
  emit tracker-transition/comment handoff events.
- `internal/worker/runtask.go` forwards run phases and runner runtime events to
  the event emitter. `runner_end` carries bounded `output_head`, `output_tail`,
  `output_bytes`, and `output_dropped` fields when runner output exists.
- `internal/runner/codex_app_server.go` captures combined app-server output to
  `.aiops/CODEX_APP_SERVER_OUTPUT.txt`, capped at 1 MiB.
- `docs/runbooks/task-api.md` says runtime rows are exposed by the API, while
  per-run detail lives in process logs and workspace artifacts.
- `docs/runbooks/reviewer-worker.md` and `docs/runbooks/workflow-authoring.md`
  already define reviewer findings, `Rework`, and `LEARNINGS.md` as
  configuration/prompt-level improvement surfaces, not worker gates.

## Evidence inventory

| Evidence source | Available today | Durable today | Secret / size considerations | L4 use | Minimum missing capture |
| --- | --- | --- | --- | --- | --- |
| Worker task events (`runner_start`, `runner_end`, `run_phase_transition`, runtime events) | Emitted through the worker event emitter and process logs | Only if the operator keeps logs; not queryable through `/api/v1/state` after rotation | Payloads may include agent text or bounded output excerpts; keep excerpts small | Classify failures, timeouts, stalls, malformed output, input-required stops, and handoff stops | A log/report importer that reads existing logs and extracts only event kind, issue, session, timestamp, bounded payload fields, and source reference |
| `/api/v1/state` runtime snapshot | Current HTTP state surface | No; in-memory, FIFO-bounded, restart-local | Contains issue IDs, URLs, last messages, workspace paths, tokens, rate limits | Live triage and report seeding while a run is still visible | No durable store in phase 1 or 2; report commands may embed one explicit snapshot with timestamp |
| Workspace `.aiops/PROMPT.md` and `.aiops/TASK.md` | Written for each run | Durable while workspace exists; usually untracked | Prompt and task text can include tracker content or secrets from user-provided issue text | Reconstruct the instructions that led to repeated failure | Extract references and bounded summaries by default; raw prompt inclusion requires explicit operator opt-in |
| Workspace `.aiops/PLAN.md` | Written in analysis-only mode | Durable while workspace exists; usually untracked | Agent-authored text can contain copied secrets or user content | Understand assessment-only handoff failures | Same bounded-summary rule as prompt/task artifacts |
| Workspace `.aiops/CODEX_APP_SERVER_OUTPUT.txt` | Written by the Codex app-server runner | Durable while workspace exists; capped at 1 MiB | Raw model/tool stream can contain sensitive snippets | Diagnose malformed protocol output, tool-call loops, auth failures, and runner crashes | Store only digest, byte count, dropped flag, and bounded head/tail excerpts in reports |
| `runner_end` output fields | Present in event payloads when output exists | Same durability as logs/events | Already bounded head/tail, plus byte count and dropped flag | Cheap symptom extraction without opening the workspace | Import those fields from existing logs; do not add a second output artifact |
| Tracker issue state, comments, labels, and history | Available through tracker APIs and UI | Durable in the tracker | User-authored content; may contain secrets or private repo context | Detect `Rework`, repeated clarification, cancellation, or dependency patterns | Read-only importer with explicit tracker scope; copy only quoted snippets needed for evidence |
| PR body, review threads, Codex reviews, and human review comments | Available through GitHub/Gitea APIs | Durable in forge, subject to platform retention | Review comments can quote source or secrets; unresolved threads are merge-significant | Group recurring review findings and map them to harness surfaces | Use existing GraphQL/CLI queries; store thread URLs/ids and bounded summaries |
| CI status and logs | Available through GitHub Actions and `gh pr checks` | Durable until forge retention expires | Logs may include redacted secrets and noisy build output | Group flaky checks, environment mismatches, and missing local gates | Capture check name, conclusion, run URL, failing step, and bounded excerpts before retention expiry |
| Reviewer-worker verdicts and `Rework` comments | Available as tracker/PR comments and labels | Durable in tracker/forge | Same as tracker/PR comments | High-signal feedback for prompt/rubric updates | No worker capture; import from tracker/forge surfaces |
| `WORKFLOW.md`, reviewer rubrics, `LEARNINGS.md`, skills, hooks, tests, and CI config | Repo-owned files | Durable via git history | Changes can increase prompt budget or grant capability | Target surfaces for proposed harness fixes | Reports should point to exact files and proposed acceptance criteria, not silently edit them |
| Validation artifacts under `docs/validation/` | Committed selectively by operators | Durable when committed | Screenshots/logs can reveal local paths or tokens | Reproduce and compare end-to-end failures | No new capture by default; reference committed artifacts when already present |

## Redaction, retention, and bounds

The default L4 artifact is a report, not a trace lake.

- Store metadata first: issue/PR ids, session id, event kind, timestamps,
  command/check names, URLs, and file paths.
- Store text excerpts only when they are needed to prove the grouping. Each
  excerpt must be bounded and labeled with its source.
- Do not store full prompts, full agent streams, full CI logs, raw GraphQL
  payloads, tokens, clone URLs with userinfo, or complete tracker comments by
  default.
- Use the existing `workflow.MaskCloneURL` convention for any clone URL that
  enters a report.
- A generated report should cap each run's embedded evidence at 64 KiB and each
  failure cluster at 256 KiB. Larger inputs are referenced by URL/path plus
  digest and byte count.
- Report files should be operator-owned artifacts. If committed, they belong
  under `docs/validation/` or another explicit evidence directory with a short
  retention rationale; otherwise they may remain local scratch output.
- Any future worker-written durable capture must be justified as evidence for
  harness improvement, not scheduler state. The smallest acceptable shape would
  be a bounded per-run evidence manifest containing ids, event kinds, byte
  counts, digests, and redacted references, not raw logs.

## Grouping model

Group by failure class, not by run id. A cluster should contain enough evidence
for a reviewer to decide whether to update a harness surface. Grouping is agent
or tool work, not operator homework; the operator may approve promotion to an
issue or draft PR, but should not have to hand-assemble the report.

Required fields:

- cluster id and short title
- symptom class, for example `prompt ambiguity`, `missing reviewer rubric`,
  `local/CI mismatch`, `runner timeout`, `input required`, `tool unsupported`,
  `scope leak`, `flaky check`, or `review recurrence`
- affected issue/PR/run ids
- evidence references with bounded excerpts
- suspected harness surface to change
- proposed reviewable output: report-only, issue proposal, or draft PR proposal
- acceptance criteria for the proposed harness change
- redaction note naming what was omitted

A cluster should normally require at least two independent occurrences. A single
severe safety or data-loss finding may become a cluster if the evidence explains
why waiting for recurrence is unacceptable.

## Workflow

Split delivery into complete milestones. Do not leave a phase that only creates
manual sorting work for the operator; each milestone must produce a reviewable,
usable artifact and explicit next action.

1. **Milestone 0: inventory/design (this PR).** Settle boundaries, evidence
   sources, redaction, retention, and the delivery sequence. Done means the next
   implementation PR can point to this document for what to read, what to omit,
   and what runtime boundaries not to cross.
2. **Milestone 1: agent-generated grouped report.** Add a command or agent
   workflow that reads existing logs, workspace artifacts, tracker/PR state,
   review findings, and CI status, then writes a grouped report without manual
   clustering by the operator. The report command should not mutate tracker
   state, open a PR, edit prompts, or merge anything. Done means an operator can
   point it at an issue, PR, run directory, or bounded evidence bundle and
   receive clusters with evidence references, redaction notes, and proposed
   next actions.
3. **Milestone 2: issue / draft-PR proposal.** Extend the report renderer so
   each actionable cluster can produce a ready-to-open issue body or draft-PR
   plan against repo-owned harness files. Done means the agent can promote a
   reviewed cluster into an issue or draft PR through normal workflow tooling;
   the operator should approve intent, not write the body by hand.
4. **Milestone 3: agent follow-through.** Let an outside coding-agent workflow
   consume an approved report/proposal and create the actual harness diff. It
   still acts as a normal coding agent: owned branch, reviewed change, normal
   tests, normal PR, and no worker-side writeback. Done means the result is a
   merge-ready PR or a clearly closed no-op with evidence.
5. **Milestone 4: reviewed evaluator candidate.** If a cluster proves a
   recurring class of failure, ship an advisory evaluator through ordinary
   review with fixtures and false-positive expectations. Done means the
   evaluator can produce report-only signal. Making it a CI/runtime gate is a
   separate later decision after the evaluator itself has review history.

## Rejected alternatives

- **Worker-side post-turn verifier.** Rejected because verification/handoff is
  agent/prompt-owned in this repo, and post-turn worker gates have repeatedly
  raced reconcile-cancel and duplicated upstream-absent behavior.
- **Orchestrator-owned PR creation, merge, or tracker write-back.** Rejected by
  SPEC section 1 and the project boundary closed under #76.
- **Automatic prompt, rubric, skill, or `LEARNINGS.md` rewrite.** Rejected
  because L4 output must be reviewable repo cargo before it affects future runs.
- **Durable scheduler trace DB.** Rejected for this phase. Restart recovery is
  tracker/filesystem-driven, and L4 evidence should not become scheduler state.
- **Unbounded log persistence.** Rejected because prompts, model output, tracker
  text, and CI logs can contain secrets or private code.
- **Natural-language merge/evaluator gate from comments.** Rejected because
  forge comments are useful evidence, not a stable machine contract unless a
  reviewed parser/evaluator has been explicitly added.

## Acceptance mapping

- Evidence sources and durability are listed in [Evidence inventory](#evidence-inventory).
- Redaction, retention, size bounds, and secret-safety are covered in
  [Redaction, retention, and bounds](#redaction-retention-and-bounds).
- The Symphony boundary is preserved by [Authority and constraints](#authority-and-constraints)
  and [Rejected alternatives](#rejected-alternatives).
- First implementation outputs are reviewable reports, issue bodies, or draft
  PR plans per [Workflow](#workflow).
- Recurring failures are grouped with reviewer-decision evidence per
  [Grouping model](#grouping-model).
- Evaluators and gates are deferred until reviewed per [Workflow](#workflow).
