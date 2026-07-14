<!--
Source: https://x.com/zachlloydtweets/status/2066908445425496348
Article URL: https://x.com/zachlloydtweets/article/2066908445425496348
Archived: 2026-06-18 via /Users/yvan/developer/yy-skills/x-content-archiver
Author: Zach Lloyd
Published: 2026-06-16
-->

> **Source:** https://x.com/zachlloydtweets/status/2066908445425496348
> **Article:** https://x.com/zachlloydtweets/article/2066908445425496348
> **Author:** Zach Lloyd
> **Published:** June 16, 2026

---

# Self-Improvement Loops for Skills

**Advisory practitioner account.** This is not authoritative on Symphony SPEC
behavior and must not override the scheduler/runner boundary. It is useful
evidence that feedback can inform ordinary, reviewed repo changes to skills,
workflow prompts, rubrics, hooks, tests, CI, or documentation without becoming
an automatic platform subsystem.

## What the article describes

Zach Lloyd frames self-improvement as two coupled loops over file-based Skills:

1. **Inner loop.** A task-triggered agent applies a Skill, such as triaging a
   newly filed GitHub issue into labels like ready-to-implement, duplicate, or
   needs-info. The run leaves records in a durable place: a file, agent trace,
   Slack/GitHub interaction, or another external system.
2. **Outer loop.** A scheduled agent reviews those records, compares them with
   feedback from humans or an automated grader, and opens a diff against the
   Skill file so future inner-loop runs improve.

The concrete example is issue triage. If a human changes the agent's label and
explains why, the outer loop finds that correction later and edits the triage
Skill. Once that diff merges, the improved Skill feeds the next inner-loop run.

Two follow-up replies narrow the implied product shape:

- Evaluation and result tracking are the next required layer after the first
  loop is working.
- File-based Skills are a good target because coding agents can update them
  directly as normal diffs.

## Mapping to aiops-platform

| Zach/Warp concept | aiops-platform surface | Current status |
| --- | --- | --- |
| Inner task-triggered agent loop | tracker polling, deterministic workspace prep, runner dispatch, agent-owned PR/tracker handoff | Supported |
| Durable records of each run | task events, runner item streams, PR comments, reviewer findings, CI output, and tracker state/history | Available to operators and reviewers; no L4 normalization contract |
| Human or automated feedback | `Human Review`, `Rework`, reviewer-worker verdicts, PR review, CI failures | Supported as workflow/review surfaces |
| Scheduled outer improvement agent | issue/PR-producing harness improvement workflow that edits repo-owned surfaces | Not part of the maintained product |
| File-based Skill updates | `WORKFLOW.md`, reviewer rubrics, `LEARNINGS.md`, skills, hooks, tests, CI, docs | Supported as ordinary repo changes, not automatic hill-climbing |

## Design implication

This article reinforces the same review boundary as LangChain L4: evidence may
inform a normal, explicitly initiated issue or PR against repo-owned harness
files. It does not justify a scheduled outer agent, worker-side post-turn
verifier, automatic PR mutator, tracker writer, or merge gate. The worker
remains the SPEC-aligned scheduler/runner/tracker reader; any improvement diff
remains agent/human-reviewed repo cargo.

This keeps the self-improvement loop compatible with the project's earned-rule
discipline: an observed failure produces evidence, a proposed harness change,
and review before it becomes a durable constraint.

Related local docs:

- [`2026-06-16-langchain-art-of-loop-engineering.md`](2026-06-16-langchain-art-of-loop-engineering.md)
- [`2026-05-addy-osmani-harness-engineering.md`](2026-05-addy-osmani-harness-engineering.md)
- [`../runbooks/reviewer-worker.md`](../runbooks/reviewer-worker.md)
- [`../runbooks/workflow-authoring.md`](../runbooks/workflow-authoring.md)
