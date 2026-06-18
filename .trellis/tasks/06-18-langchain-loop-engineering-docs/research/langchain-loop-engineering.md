# LangChain loop engineering research

## Sources

* LangChain, "The Art of Loop Engineering" (Sydney Runkle, 2026-06-16):
  https://www.langchain.com/blog/the-art-of-loop-engineering
* LangSmith Engine docs:
  https://docs.langchain.com/langsmith/engine
* Zach Lloyd, "How to build a self-improvement loop for your Skills"
  (2026-06-16):
  https://x.com/zachlloydtweets/status/2066908445425496348
  * Retrieved with `/Users/yvan/developer/yy-skills/x-content-archiver`.
    The status archive captured 4 posts after 8 scroll iterations; only the
    manifest is retained under the task research directory.

## Summary

LangChain frames loop engineering as four nested levels:

1. Agent loop: a model calls tools until the task is complete.
2. Verification loop: a grader/rubric evaluates the output and feeds failures
   back into another attempt.
3. Event-driven loop: schedules, webhooks, or application events trigger agent
   runs without manual prompting.
4. Hill-climbing loop: traces from production runs feed an analysis agent that
   improves the harness configuration, such as prompts, tools, graders, memory,
   or skills.

LangSmith Engine is the closest productized example of level 4: it analyzes
traces, surfaces recurring issues, diagnoses root causes, proposes fixes,
creates evaluators or examples to prevent regressions, and can reopen issues if
the failure recurs.

Zach Lloyd's Warp/Oz practitioner account describes the same L4 shape in Skill
vocabulary: an inner task-triggered agent applies a file-based Skill and
records interactions; a scheduled outer agent reviews past runs plus human or
grader feedback, then opens a diff that improves the Skill. A follow-up reply
explicitly calls out evaluation and result tracking as the next layer.

## Mapping to aiops-platform

* L1 is covered by the worker/runner/workspace loop.
* L2 is covered by reviewer-worker, in-run grader sub-agent, rubric checks, and
  Rework re-dispatch.
* L3 is covered by tracker polling and state/label transitions.
* L4 is partially covered by repo-owned memory (`LEARNINGS.md`) and review
  feedback, but trace-driven harness improvement is not yet a first-class
  platform workflow.

## Recommendation

Document L1-L3 as supported product capabilities and L4 as a follow-up product
direction. The follow-up should preserve the Symphony boundary: the worker
remains a scheduler/runner/tracker reader, and any harness changes should land as
issues or PRs against repo-owned surfaces such as `WORKFLOW.md`, reviewer rubrics,
`LEARNINGS.md`, skills, hooks, or CI.
