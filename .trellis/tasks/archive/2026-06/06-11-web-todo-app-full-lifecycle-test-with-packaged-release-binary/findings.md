# Lifecycle test findings (v0.1.0 packaged binary)

## F1: `Depends on #N` blocker gate dead on Gitea tracker

- Observed 12:06:50: worker dispatched issue #10 (body ends `Depends on #4`) while #4
  was still `aiops/todo` ("AI Ready") and #1 was only in human-review.
- Root cause: `internal/orchestrator/poller_test.go::TestPollOnceIgnoresBlockersForNonTodoStates`
  pins that the blocker gate applies only to issues whose state is literally `"Todo"`.
  Gitea's `DefaultStateLabelMappings()` maps the pre-dispatch label `aiops/todo` to
  state `"AI Ready"` — never `"Todo"` — so on Gitea the gate never fires.
  `internal/gitea/tracker_client.go::buildBlockedBy` (+ #677 per-tick blocker cache)
  produces `BlockedBy` data the poller then ignores for every Gitea issue.
- Class: cross-tracker integration gap (AGENTS.md checklist item 1 — adjacent-path
  consumers of a SPEC concept diverge). Needs upstream check: does SPEC §11.3 key the
  blocker rule on "Todo" or on "not-yet-started" semantics?
- Action: file aiops-platform issue (area:spec-alignment or bug) after the run.
- Operational workaround in this test: operator-side label gating — only label an
  issue `aiops/todo` once all its dependencies are `aiops/done`.

## F2 (positive): reconcile-cancel exercised, twice

- Stripping the running issue #10's `aiops/*` label triggered
  `StreamingTurn → CanceledByReconciliation` + "runner stopped: reconcile ineligible"
  at 12:09:02 (forced via `POST /api/v1/refresh`). PR #131 behavior confirmed on the
  packaged binary.
- Same for #11 at 12:11:44 (unforced, ~2.5 min after its label was already gone).

## F3: slot-free dispatch uses stale candidates without revalidation

- At 12:09:02, the instant #10 was reconcile-canceled, the freed slot dispatched #11 —
  whose `aiops/*` labels had been stripped ~1 minute earlier. The candidate came from
  an earlier tick's queue and was not revalidated against current tracker state at
  dispatch time. Reconcile later corrected it (F2), at the cost of one wasted ~2.5-min
  codex run.
- Needs upstream check: does the Elixir reference revalidate candidates at dispatch, or
  is reconcile-as-safety-net the designed behavior? If the latter, this is
  working-as-designed and only worth a doc note.

## Diagnostics observed (positive)

- `missing_aiops_state_label` emitted for each unlabeled issue per tick — matches
  label_state.go design.

## F4 (note): cancel vs agent-handoff race is benign here

- #11's agent hit the prompt's blocked-escape path (deps missing on main) and labeled
  the issue aiops/human-review at ~the same moment reconcile canceled the run. The
  label write (served by the worker-side gitea_issue_labels tool) landed despite the
  cancel. No PR existed; operator reset the label. No state corruption observed.

## F5 (lifecycle): Rework path exercised via merge conflict

- PR #17 (issue #5) hit a real merge conflict (both #4 and #5 appended tests to
  cmd/server/main_test.go; squash-merge made ai/5's base stale).
- Operator appended a rework note to the issue body and set `aiops/rework`
  (an active state) — expecting re-dispatch of #5 with the same ai/5 branch.
- Also noted: agent #5 touched internal/store/store.go on a frontend-only issue
  (mild scope creep; prompt could pin file scope harder).
