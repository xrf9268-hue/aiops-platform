# Workflow authoring: the LEARNINGS.md cross-run memory convention

How to give your loop a memory that survives runs — as a repo-owned file the
agent reads before planning and appends to under review, with zero platform
configuration.

This promotes the convention from #745's closing comment into a documented
operator surface. The platform decision behind it (#745, closed not-planned):
the workspace IS a deterministic clone of the target repo, so a
`LEARNINGS.md` at the repo root is already readable, writable, and
PR-committable by the agent through its ordinary file tools — a worker-side
prompt-injection mechanism would be behaviorally identical to one prompt
instruction and has no upstream equivalent (`config/schema.ex` has no prompt
field; SPEC's workflow file is repository-owned by design).

Advisory background (per the AGENTS.md practitioner-accounts clause): the
"sixth piece" of a loop is memory that lives outside the conversation — "The
agent forgets, the repo doesn't" (Osmani). Effective memory follows the
**fail → investigate → verify → distill → consult** progression; the failure
mode of weaker setups is piling up unverified notes that are never read
back, and the fix is task-specific memory *instructions* (Martin). This
repo's own AGENTS.md earned-rules + provenance discipline is the same chain
applied to engineering rules; the convention below packages it for any repo
your worker runs against.

In LangChain's four-level loop-engineering vocabulary, `LEARNINGS.md` is a
memory surface for making reviewed harness improvements durable repo cargo. It
is not an automatic hill-climbing trace analyzer: an agent or reviewer must
verify a durable fact, update the file in a PR, and let normal review prune or
accept the rule. Zach Lloyd's self-improving Skills account lands in the same
place: file-based harness changes are easy for coding agents to propose, but
they still ship as reviewed diffs.

## The prompt section (drop-in)

Append to your implementation `WORKFLOW.md`'s prompt body — the worker that
writes code; the "maker", if you also run the
[reviewer-worker pattern](reviewer-worker.md). (This is the #745 snippet,
verbatim:)

```markdown
## Project memory (LEARNINGS.md)

- Before planning, read `LEARNINGS.md` at the repo root if it exists; treat its
  entries as verified project facts (CI quirks, flaky areas, prior root causes).
- When this run verifies a NEW durable fact (symptom → root cause → how you
  verified it → rule), append it to `LEARNINGS.md` and include the change in
  your PR. Every entry MUST have a `verified:` line describing the reproduction
  or measurement; never record unverified guesses.
- Keep entries general (rules, not run logs); fold duplicates instead of
  appending repeats.
```

The `verified:`-required guard is the load-bearing line. Without it the file
degrades into a sediment of guesses that no future run trusts or consults —
the exact failure mode the memory-instructions prescription exists to
prevent. Bare hunches are banned; an entry earns its place by carrying its
own evidence.

## Entry template (keep entries rules, not run logs)

Each entry is one *generalized rule* with the evidence chain that produced
it — four fields, mapping 1:1 onto fail → investigate → verify → distill:

```markdown
## <one-line rule>
- symptom: <what was observed>
- root-cause: <the diagnosed cause>
- verified: <how it was verified — reproduction or measurement>
- rule: <the generalized rule future runs should follow>
```

A real-shaped example:

```markdown
## Run new integration tests under a 1-CPU constraint before merging
- symptom: integration tests pass locally but time out intermittently on CI
- root-cause: the CI container defaults to 1 CPU; t.Parallel tests starve
  each other under that budget
- verified: local `docker run --cpus=1` reproduced the timeout 3/3; raising
  to 2 CPUs gave 0 failures in 10 runs
- rule: any new integration test must pass locally under --cpus=1 before it
  ships
```

What does NOT belong: per-run narration ("run #57 fixed the login bug"),
restated repo documentation, or anything whose `verified:` line would be
empty. If two entries state overlapping rules, fold them into one instead of
appending a near-duplicate.

## Division of responsibilities

| Surface | Holds | Owner / lifecycle |
|---|---|---|
| Tracker | **state** — which issue is in which stage (SPEC §14.3: orchestrator state is rebuilt from the tracker, no DB) | tracker writes are agent-side; the worker only reads |
| `WORKFLOW.md` | **instructions** — config front matter + the prompt that teaches the agent how to work | repo-owned, operator-edited |
| `LEARNINGS.md` | **verified project experience** — durable, evidence-backed rules | repo-owned cargo: the agent appends, every change rides a PR through human review |

`LEARNINGS.md` is deliberately NOT a third config surface: the platform
never parses it. It exists entirely inside the agent's workspace and the
repo's review flow.

## Loop-engineering level map

| Level | Surface in this project | Notes |
| --- | --- | --- |
| L1 Agent loop | worker + runner + deterministic workspace | Built into the core worker path. |
| L2 Verification loop | reviewer worker, in-run grader sub-agent, `Rework` | Configured through `WORKFLOW.md` and tracker states. |
| L3 Event-driven loop | tracker polling, labels/states, reconcile cancel | Built into scheduler behavior. |
| L4 Hill-climbing loop | `LEARNINGS.md`, rubric edits, prompt/tool/CI/hook changes | Operator-reviewed changes; no automatic trace-driven loop. |

## Cost and growth

- Every entry is prompt-budget the agent spends reading on each run — which
  is exactly why entries must be generalized rules, not run logs. A handful
  of high-value rules pays for itself; a diary does not.
- Review pressure is the eviction policy: the file changes only inside PRs,
  so reviewers prune bloat the same way they prune code. A worker-side size
  cap would be a policy gate of the class removed in #561/D33 — not added.
