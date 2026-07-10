<!--
Source: https://x.com/addyosmani/status/2074927530482835916
Archived: 2026-07-10
Author: Addy Osmani (@addyosmani)
Published: 2026-07-08
-->

# Own the Outer Loop: evidence, responsibility, and aiops-platform's boundary

**Advisory practitioner account.** This article is not an authority on the
Symphony protocol. Its value here is as a governance vocabulary for a system
that can dispatch more work than an operator can inspect line by line. It
validates existing boundaries; it does not justify a new worker-owned quality
gate.

## The useful distinction

Osmani distinguishes the inner capability loop from the outer responsibility
loop. A model plus its harness can investigate, implement, verify, and repeat;
the person or organization accountable for the dependent system must decide
what evidence is sufficient and own the decision to ship, block, redirect, or
add a constraint.

The article names three connected ideas:

- **Quality:** the checks and constraints that generate evidence.
- **Verdict:** the production decision made from that evidence.
- **Answerability:** being able to explain why the decision was defensible.

It also identifies four places where human judgment remains valuable without
putting a person in every inner-loop action: setting constraints, choosing what
to sample, retaining useful audit evidence, and owning the production boundary.

## What it validates here

This project's core boundary already matches the useful part of that model.
Symphony makes the worker a scheduler/runner and tracker reader; the coding
agent owns ticket writes, branch pushes, and PR handoff through the workflow
and runtime tools. A human or existing repository governance owns the decision
at the production boundary. The worker must not impersonate either role.

The article is especially useful when evaluating proposals to add a post-run
worker verifier. It is right that a model's unsupported success claim is not
enough, but the correct home for preventive verification in this project is the
agent-owned, pre-push `WORKFLOW.md` contract and normal repository checks.
Adding a worker phase after the agent has handed work off would cross the
scheduler/runner boundary and can race reconciliation. External CI and human
review can supply independent evidence without moving ticket or PR authority
into the worker.

The practical takeaway is to keep the emitted evidence legible: validation
commands, failures, logs, artifacts, and the reason for the chosen handoff
state should allow the accountable reviewer to form a verdict. Which evidence
is required remains repository policy, earned from actual failures rather than
a generic platform rule.

## Non-conclusion

This account does not establish a universal approval model, an audit-database
requirement, or a mandatory human review for every run. Those would exceed the
SPEC and the project's existing policy envelope. It gives us concise language
for judging future proposals: a claimed control should either improve evidence
or answerability at the right boundary, or it does not belong.

## Related material

- [Symphony SPEC](SPEC.md)
- [OpenAI Symphony announcement](2026-04-27-openai-symphony-blog.md)
- [Addy Osmani: Agent Harness Engineering](2026-05-addy-osmani-harness-engineering.md)
- [Addy Osmani: Agentic Autonomy Levels](2026-07-03-addy-osmani-agentic-autonomy-levels.md)
