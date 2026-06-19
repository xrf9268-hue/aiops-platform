# Trace harness proposal follow-through

Use this runbook after an operator approves an actionable cluster from
[`trace-harness-report.md`](trace-harness-report.md). It implements the
Milestone 3 handoff from
[`docs/design/trace-driven-harness-improvement.md`](../design/trace-driven-harness-improvement.md):
an approved proposal becomes ordinary reviewed repo cargo through a normal
coding-agent workflow.

This is not a worker command. The worker does not open issues or PRs, mutate
tracker state, rewrite prompts, merge branches, or create evaluator gates. The
coding agent acts through the same branch, review, test, and PR flow it uses for
any other issue.

## Inputs

- source report JSON using schema `trace-harness-report/v3`
- approved cluster id
- generated `proposals.github_issue.body` or `proposals.draft_pr.plan`
- operator approval record, such as an issue comment, chat instruction, or
  review note naming the cluster and intended harness surface

The proposal text is evidence and planning context. Do not parse arbitrary
report, review, log, prompt, or agent text in the worker to make the decision.

## Operator approval points

Approve only after checking the cluster's evidence references, redaction note,
suspected harness surface, proposed acceptance criteria, and target repo-owned
surface. Valid follow-through surfaces include `WORKFLOW.md`, reviewer rubrics,
`LEARNINGS.md`, skills, hooks, tests, CI, and docs.

Approval gives the coding agent permission to start a normal implementation
run. It does not authorize unattended merge, worker-side PR creation, worker
tracker writeback, automatic prompt/rubric/skill rewrite, or evaluator gate
promotion.

## Preferred path: create a tracking issue first

Create or reuse a tracker issue before asking the coding agent to implement the
proposal. This keeps the final PR compatible with the repository's required
`Closes #N` metadata and gives no-op decisions a durable place to land.

```bash
report=./trace-report.json
cluster_id=runner-timeout

jq -r --arg id "$cluster_id" \
  '.clusters[] | select(.id == $id).proposals.github_issue.body' \
  "$report" > /tmp/trace-harness-issue.md

title="$(jq -r --arg id "$cluster_id" \
  '.clusters[] | select(.id == $id).proposals.github_issue.title' \
  "$report")"

gh issue create --title "$title" --body-file /tmp/trace-harness-issue.md
```

Then hand the issue number, report path, cluster id, and extracted draft-PR plan
to the normal issue workflow. In this repository that means
[`.claude/skills/handle-issue/SKILL.md`](../../.claude/skills/handle-issue/SKILL.md)
for issue-to-PR work, followed by
[`.claude/skills/handle-pr/SKILL.md`](../../.claude/skills/handle-pr/SKILL.md)
and [`pr-review-merge-protocol.md`](pr-review-merge-protocol.md) for review
follow-through.

## Agent handoff prompt

Use this prompt shape when the outside coding agent needs a concrete entrypoint:

```text
Follow through the approved trace harness proposal.

Inputs:
- tracking issue: #<issue>
- source report: <path or artifact URL>
- cluster id: <cluster-id>
- proposal plan: <path containing proposals.draft_pr.plan>
- operator approval: <comment, review note, or instruction>

Before editing, read the source report cluster, the generated proposal, and
docs/design/trace-driven-harness-improvement.md. Implement only the smallest
repo-owned harness change needed for the approved proposal. Use an owned branch
from the current base, run the appropriate verification, and open or update a
normal PR that closes the tracking issue.

If the proposal is already satisfied or should not change the harness, record a
closed no-op with evidence on the issue or draft PR instead of landing empty
churn.

Do not add worker-side PR/tracker writeback, post-turn verifier phases,
automatic prompt/rubric/skill rewrites, unattended merge, or evaluator gates.
```

## PR ledger

The implementation PR body remains the public ledger. Include:

- `Closes #N`
- source report path or artifact URL, schema, input digest when available, and
  cluster id
- proposal source: `proposals.github_issue.body` or `proposals.draft_pr.plan`
- evidence references and redaction note copied from the report, not raw logs
- operator approval reference
- exact harness surface changed
- verification commands and results
- size-gate classification from `pr-review-merge-protocol.md`
- statement that the change adds no worker-side writeback, unattended merge, or
  evaluator gate

Use the shared PR protocol for local gates, dual review, `@codex review`, CI,
unresolved thread handling, and merge readiness. Trace harness follow-through
stops at merge-ready; no unattended merge is allowed in this workflow.

## Closed no-op with evidence

A no-op is valid only when it is recorded where reviewers can audit it. If the
agent proves no repo change is needed, comment on and close the tracking issue,
or close the draft PR if one already exists. The no-op record must name:

- source report and cluster id
- original proposal title or plan path
- operator approval reference
- evidence checked
- reason no repo-owned harness change is warranted
- verification performed, or why verification was not applicable
- recurrence signal that would justify reopening or filing a new proposal

Do not merge an empty PR just to satisfy the loop.

## Failure reporting

If the agent cannot produce a merge-ready PR or a justified no-op, leave a
failure report on the issue or PR and keep the tracker state consistent with the
normal workflow. Include the failing command, blocked dependency, missing
evidence, or rejected scope, plus the source report, cluster id, and operator
approval reference.

That issue or PR comment is the L4 evidence trail for this milestone. Do not
write worker state, mutate the report JSON, or silently promote the failure into
prompt, rubric, skill, hook, CI, or evaluator changes. A future trace report may
consume tracker or PR state when that input source is explicitly supported, but
the current handoff stays in the normal forge review flow.
