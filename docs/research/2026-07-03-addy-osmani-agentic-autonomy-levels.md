<!--
Source: https://x.com/addyosmani/status/2072885435312042327
Archived: 2026-07-10
Author: Addy Osmani (@addyosmani)
Published: 2026-07-03
-->

# Agentic Autonomy Levels: a two-axis lens for aiops-platform

**Advisory practitioner account.** This article is not authoritative on the
Symphony SPEC or its Elixir reference. It is useful because its separation of
an individual agent's latitude from a system's coordination capability makes
the boundary of this project easier to describe. It proposes no new worker
phase, configuration key, or rule.

## The useful model

Osmani separates two concepts that are often collapsed into a single
"autonomy level":

- **Agency** is how far a particular agent may proceed without a person
  continuously directing it: assist, supervised action, bounded delegation,
  and goal-driven work.
- **Orchestration** is how many agents are coordinated: one interactive agent,
  several isolated agents, or a system that continuously turns a queue into
  work and reports exceptions.

The article's practical consequence is that a team can increase one dimension
without claiming the other. A more capable coding agent does not itself create
a safe multi-agent service; conversely, a scheduler does not make every task
safe to execute with high agency. The verification evidence needed to justify
the chosen latitude is part of the decision.

## What it says about this project

`aiops-platform` deliberately occupies the high-orchestration end of this
model: it polls a tracker, prepares one deterministic workspace per issue,
enforces bounded concurrency, reconciles eligibility, and keeps concurrent
claims isolated. That is the scheduler/runner role specified by Symphony.

It does **not** turn the worker into the owner of every agent decision. Agency
remains task- and repository-defined through `WORKFLOW.md`, the issue's
acceptance criteria, the agent runtime, and the available verification
environment. In particular, "management by exception" is not a reason for the
worker to create PRs, push branches, edit tickets, approve output, or merge:
those actions remain agent- or human-owned at the documented boundary.

The article reinforces three existing operating choices:

1. Scale concurrency with earned confidence, rather than treating a larger
   agent fleet as an unconditional improvement.
2. Keep per-issue workspaces and bounded concurrency; false parallelism and
   overlapping ownership are coordination failures, not model failures.
3. For goal-driven work, make success conditions concrete and observable in
   the issue and `WORKFLOW.md`. Evidence such as tests, types, lint, screenshots
   and reproduction steps is what makes delegation defensible.

## Non-conclusion

This is a vocabulary and review lens, not a proposal to add an `autonomy_level`
setting or a worker-owned approval/verification loop. Such a mechanism would
need an observed failure and an upstream-compatible home before it belongs in
the product.

## Related material

- [Symphony SPEC](SPEC.md)
- [OpenAI Symphony announcement](2026-04-27-openai-symphony-blog.md)
- [Addy Osmani: Agent Harness Engineering](2026-05-addy-osmani-harness-engineering.md)
- [Addy Osmani: Own the Outer Loop](2026-07-08-addy-osmani-own-the-outer-loop.md)
