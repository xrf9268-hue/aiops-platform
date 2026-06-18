# Docs: add LangChain loop engineering mapping

## Goal

Document how LangChain's "The Art of Loop Engineering" maps onto
aiops-platform's existing loop and harness surfaces, and create a follow-up
GitHub issue for the LangChain L4 hill-climbing loop work without changing
worker behavior in this task.

## Requirements

* Add a research note for the LangChain article under `docs/research/`.
* Add Zach Lloyd's self-improving Skills article as an advisory practitioner
  account for the same L4 follow-up, using a local X archive when X is
  login-gated.
* Link the LangChain note from the existing practitioner-account surfaces.
* Update the reviewer-worker and workflow-authoring docs so the L1-L4 mapping is
  discoverable where operators configure verification and memory loops.
* Create a GitHub follow-up issue for trace-driven harness improvement aligned
  with LangChain L4.
* Keep this task docs-only for product behavior; do not add worker phases,
  tracker writes, merge automation, or platform verifier gates.

## Acceptance Criteria

* [x] `docs/research/` contains a LangChain loop-engineering note with the four
  loop levels and the aiops-platform mapping.
* [x] `AGENTS.md` references the LangChain note as an advisory practitioner
  account.
* [x] `docs/research/` contains a Zach Lloyd self-improving Skills note and
  treats it as practitioner evidence, not a SPEC authority.
* [x] Operator docs distinguish "L1-L3 already supported" from "L4 follow-up
  planned" without overstating current automation.
* [x] A GitHub issue exists for L4 trace-driven harness improvement and keeps the
  orchestrator boundary explicit.
* [x] Markdown/link checks that are practical locally pass.

## Definition of Done

* Docs are updated in English.
* No production code behavior changes.
* `git diff --check` passes.
* A focused docs validation command runs.

## Out of Scope

* Implementing trace collection, trace analysis, prompt rewriting, or evaluator
  generation.
* Adding a worker-side post-turn verifier, PR/merge action, or tracker writer.
* Changing SPEC-sensitive worker/orchestrator paths.

## Technical Approach

Add a concise research note mirroring the project's existing advisory-note style,
then update `AGENTS.md`, `README.md`, `docs/runbooks/reviewer-worker.md`, and
`docs/runbooks/workflow-authoring.md` to point to it. Use `gh issue create` for
the L4 follow-up once the docs wording settles. After the X article is archived,
add a second concise practitioner note and link it from the same surfaces.

## Technical Notes

* LangChain article: https://www.langchain.com/blog/the-art-of-loop-engineering
* LangSmith Engine docs describe the closest productized L4 shape:
  trace analysis -> recurring issue -> proposed fix -> evaluator -> reopen.
* Existing aiops-platform docs already cover reviewer-worker and LEARNINGS.md
  as the L2/L4-adjacent surfaces.
* Follow-up issue created: https://github.com/xrf9268-hue/aiops-platform/issues/931
* Zach Lloyd source added to the follow-up issue:
  https://github.com/xrf9268-hue/aiops-platform/issues/931#issuecomment-4737711896
* Zach Lloyd article archived with
  `/Users/yvan/developer/yy-skills/x-content-archiver`. The status archive
  captured 4 posts after 8 scroll iterations; the raw JSON/media were not kept
  in the docs commit to avoid storing a full X Article dump.
