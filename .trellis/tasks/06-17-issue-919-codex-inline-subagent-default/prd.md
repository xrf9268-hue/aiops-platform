# Fix Codex inline reviewer subagent default-on protocol

## Goal

Close issue #919 by aligning the live reviewer protocol and skills with the
default-on reviewer subagent policy. Codex inline sessions should not require an
extra current-turn authorization phrase before using an available reviewer
subagent; opt-out phrases should still switch to CLI fallback.

## Requirements

* Update the canonical reviewer protocol so Codex inline uses available
  reviewer subagents by default when `spawn_agent` is callable for the session.
* Remove stale "explicit authorization" and "Codex inline is different"
  wording from live docs and skills.
* Keep opt-out behavior (`CLI review only`, `subagent review disabled`).
* Keep the `codex-sub-agent` self-exemption and no-recursive-spawn guard.
* Keep reviewer mechanics centralized in `docs/runbooks/pr-review-merge-protocol.md`.

## Acceptance Criteria

* [ ] `docs/runbooks/pr-review-merge-protocol.md` documents default-on Codex
  inline reviewer subagent routing.
* [ ] `.claude/skills/handle-issue/SKILL.md` and
  `.claude/skills/handle-pr/SKILL.md` no longer say Codex inline requires
  per-request authorization.
* [ ] `scripts/reviewer_workflow_docs_test.go` pins the new behavior and fails
  on stale authorization wording.
* [ ] Focused scripts tests pass.

## Out of Scope

* Changing Trellis implement/check dispatch rules.
* Changing post-push `@codex review` or merge authorization gates.
* Editing historical archived Trellis tasks.

## Technical Notes

* Follow-up issue: #919.
* Regression source: the old docs still pinned explicit Codex inline
  authorization after PR #903/#900 established default-on reviewer behavior.
