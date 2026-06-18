<!--
Source: https://www.langchain.com/blog/the-art-of-loop-engineering
Archived: 2026-06-18
Author: Sydney Runkle
Published: 2026-06-16
-->

> **Source:** https://www.langchain.com/blog/the-art-of-loop-engineering
> **Author:** Sydney Runkle
> **Published:** June 16, 2026

---

# The Art of Loop Engineering

**Advisory practitioner/framework account.** This is not authoritative on
Symphony SPEC behavior, but it is useful vocabulary for describing
aiops-platform's current loop shape and for naming the next self-improvement
frontier. Treat it like the other practitioner accounts in this directory: it
can calibrate harness-engineering decisions, but concrete platform changes still
need to land on the correct side of the SPEC scheduler/runner boundary.

## Four loop levels

LangChain frames loop engineering as four nested loops:

1. **Agent loop.** A model calls tools in a loop until the task is complete.
2. **Verification loop.** A grader, rubric, or deterministic check evaluates
   the result and routes failures back into another attempt.
3. **Event-driven loop.** Schedules, webhooks, cron jobs, or application events
   trigger agent runs without a human manually prompting each one.
4. **Hill-climbing loop.** Production traces from agent runs feed an analysis
   agent that finds recurring failures and improves the harness itself: prompts,
   tools, graders, memory, skills, or related configuration.

The LangSmith Engine documentation describes a productized version of level 4:
trace analysis detects recurring issues, diagnoses root cause, proposes a fix,
adds regression evaluators or examples, and can reopen the issue if the failure
returns.

## Mapping to aiops-platform

| LangChain level | aiops-platform surface | Current status |
| --- | --- | --- |
| L1 Agent loop | `cmd/worker` prepares a deterministic workspace and runs Codex, Claude, or mock agents through the runner abstraction | Supported |
| L2 Verification loop | `reviewer-worker.md`, `examples/reviewer-WORKFLOW.md`, in-run grader sub-agent, rubric checks, and `Rework` redispatch | Supported by configuration and prompt |
| L3 Event-driven loop | tracker polling, active/inactive/terminal states, Gitea labels, reconciliation, retry, and continuation budget | Supported by the worker |
| L4 Hill-climbing loop | `LEARNINGS.md`, reviewer findings, runtime events, CI failures, and PR review feedback can become harness changes | Partially supported as operator workflow; trace-driven harness improvement is a follow-up |

## Design implication

aiops-platform already satisfies the first three loop levels without adding new
orchestrator phases. Level 4 should not be implemented as a worker-side
post-turn verifier or as orchestrator-owned PR/tracker mutation. Per SPEC and
the project's #76/#557/#561 boundary lessons, the right shape is:

1. Persist or collect useful run evidence: task events, runner item streams,
   reviewer verdicts, CI failures, and Rework comments.
2. Analyze recurring patterns with an agent or operator workflow.
3. Produce a normal issue or draft PR against repo-owned harness surfaces:
   `WORKFLOW.md`, reviewer rubrics, `LEARNINGS.md`, skills, hooks, tests, CI, or
   documentation.
4. Let human or reviewer-agent review decide whether the harness change ships.

This keeps the worker a scheduler/runner/tracker reader while still allowing the
harness to improve over time. The trace-driven L4 follow-up is tracked in
[#931](https://github.com/xrf9268-hue/aiops-platform/issues/931).

Related local docs:

- [`2026-06-16-zach-lloyd-self-improving-skills.md`](2026-06-16-zach-lloyd-self-improving-skills.md)
- [`2026-05-addy-osmani-harness-engineering.md`](2026-05-addy-osmani-harness-engineering.md)
- [`2026-04-27-openai-symphony-blog.md`](2026-04-27-openai-symphony-blog.md)
- [`../runbooks/reviewer-worker.md`](../runbooks/reviewer-worker.md)
- [`../runbooks/workflow-authoring.md`](../runbooks/workflow-authoring.md)
