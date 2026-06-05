# SPEC / Upstream Research For Issue #627

## Live GitHub State

- `gh issue view 627` on 2026-06-05: open; labels `type:hardening`, `priority:p2`, `area:spec-alignment`; no comments.
- GraphQL issue dependency fields: `blockedBy.totalCount=0`, `blocking.totalCount=0`, `parent=null`, `subIssues.totalCount=0`, `trackedIssues.totalCount=0`, `trackedInIssues.totalCount=0`.

## Upstream State

- `handle-issue` bootstrap refreshed `/tmp/symphony-upstream` to `54b456b` (`[linear] Require opt-in labels for dispatch (#88)`).
- Latest upstream still has the unbounded cross-session continuation shape:
  - `SPEC.md` §7.1 says that after `agent.max_turns`, a normal worker exit schedules a short continuation retry if the issue remains active.
  - `agent_runner.ex` returns `:ok` after reaching `agent.max_turns` with the issue still active.
  - `orchestrator.ex` handles normal agent down by calling `schedule_issue_retry(... delay_type: :continuation ...)` unless the last event is input-required.

## Local Repo State

- `DEVIATIONS.md` D34 already accepts the cross-session clean-continuation budget deviation.
- PR #628 (`[codex] Bound clean continuation loops`) merged on 2026-06-04 and closed #621 by adding `agent.max_continuation_turns`, orchestrator budget carry, runner clean-budget stopping, blocked method `continuation_budget`, and related tests/docs.
- Issue #627 remains open because the governance lesson is not fully captured in `AGENTS.md` principle 6, and the D34 ledger references should distinguish #625 evidence from #628 implementation.

## #568 / #576 / #577 Re-Audit Conclusion

- #568 was correctly closed as an over-design sweep under the old evidence: D29 and D30 caps were wrong-side-of-§1 terminal/post-handoff gates and were removed by #576/#577 follow-ups.
- #621/#625/#628 do not resurrect the removed D29/D30 mechanisms wholesale. They justify a new, narrower D34 deviation that:
  - caps only clean still-active continuation turns,
  - does not cap failure retries,
  - parks locally in `Blocked` instead of writing tracker state,
  - uses structured runtime state instead of parsing natural-language agent output.
- The governance fix should therefore add an exception clause, not weaken principle 6's default delete verdict.

## Planned Documentation Changes

- `AGENTS.md`: add the under-hardened-upstream exception under principle 6 with evidence requirements: reproduce/validate the upstream defect, prove the prompt/tracker state cannot cover it, document a narrow accepted deviation, and keep the hardening on the scheduler/runner side only when SPEC's boundary permits it.
- `DEVIATIONS.md`: update D30/D34 wording/references so #625 is evidence, #628 is the implementation PR, and #627 is the process/governance lesson.

## Grill-With-Docs Notes

- `CONTEXT.md` already defines the domain language this PR should use: `Clean continuation turn`, `Clean turn budget`, and `Blocked claim`.
- The plan must not describe D34 as a resurrected `Failed` map, failure-retry cap, or legacy continuation-spawn cap. It is a clean-turn budget that parks a local blocked claim.
- `docs/adr/0001-symphony-style-personal-orchestrator.md` still supports a Symphony-style reference model but already records removed post-push gates as "do not re-add"; the #627 change should preserve that default.
- `docs/adr/0002-ready-gated-binary-self-hosted-development.md` confirms Trellis is task context, not execution authority. This task should keep implementation instructions in issue/PR docs and use Trellis only as the ledger.
- No new ADR is warranted: the accepted architectural decision already exists as D34/PR #628; #627 is governance wording and ledger hygiene.
