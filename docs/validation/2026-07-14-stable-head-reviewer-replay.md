# Stable-head reviewer replay

**Date:** 2026-07-14

**Issue:** #1090

**Verdict:** PASS. Exact-tuple reviewer checkpoints made merged-head
continuations idempotent, while a changed head still forced a complete review.

## Scope

This replay reused the #1089 standard-profile seed, issue bodies, ordering,
worker release, model, CI gate, and preregistered holdout. The disposable
repository was
<https://github.com/zjlgdx/aiops-workflow-bench-standard-20260714-1090-v1>.
Issue 2 was activated only after issue 1 closed.

| Pin | Value |
| --- | --- |
| Seed | `5e6264cdcfd2decb53491e37de4f825878339487` |
| Worker | official v0.1.15 darwin/arm64 release |
| Codex CLI | 0.144.3 |
| Model / reasoning | `gpt-5.6-sol` / high |
| CI | `build-test`: `go test ./...`, `go vet ./...` |
| Holdout | #1089 preregistered real-process shell suite |

The operator performed repository setup, sequential activation, and read-only
state capture. After activation, the maker and reviewer owned code, PR,
review, merge, and issue-state writes.

## Prompt and document budgets

Prompt bytes exclude YAML front matter.

| Prompt | #1089 baseline | #1090 | Change |
| --- | ---: | ---: | ---: |
| Maker | 4,341 | 2,439 | -43.8% |
| Reviewer | 6,892 | 5,344 | -22.5% |
| Combined | 11,233 | 7,783 | -30.7% |

The two workflows and two governance/E2E runbooks contain 970 lines, down from
1,015. Tests are excluded from that net-negative count.

## Replay result

Both issues closed after their PRs merged, and a fresh clone at final main
`7e56f3e34132042f8d812b6acb45677153c74d1a` passed the #1089 holdout.

| Issue | PR / final head | Merged | Closed | Rework |
| --- | --- | --- | --- | ---: |
| #1 | #3 / `f14a70fd8151b38eccca720aafb6e9ddb145a2aa` | 06:16:21Z | 06:18:10Z | 1 |
| #2 | #4 / `56fa3e45d1c2fdb9300862cc0554c66054708f53` | 06:26:33Z | 06:28:29Z | 0 |

On issue 1, the first reviewer pass found two confirmed defects: missing
`TODO_DB` changed invalid-invocation behavior, and embedded newlines violated
the one-record-per-line contract. The maker pushed a new head; that tuple
change invalidated the prior review and caused a complete new review. No
operator manufactured the findings or rework.

Every passing tuple received one reviewer-owned `COMMENTED` checkpoint with
the exact `(headRefOid, baseRefOid, baseRefName)` and `local-rubric=PASS`, plus
one exact-head approval. After GitHub merged each PR, the next invocation reused
the checkpoint, skipped checkout/configured verification/semantic review, took
one external snapshot, and closed the issue without another review or trigger.

## Tokens, runtime, and retries

Worker-observed totals include all app-server sessions, including the failed
review, maker rework, and merged-head continuations.

| Worker | Tokens | Agent runtime |
| --- | ---: | ---: |
| Maker | 2,967,013 | 740.8s |
| Reviewer | 2,667,510 | 765.0s |
| Combined | 5,634,523 | 1,505.8s |

End-to-end wall time was 1,605 seconds, from issue 1 activation at 06:01:44Z to
issue 2 closure at 06:28:29Z. There was one real rework cycle and two reviewer
findings.

The stable-tuple comparison is within this replay, so it is not confounded by
the changed implementation:

| Reviewer path | Tokens | Agent runtime |
| --- | ---: | ---: |
| Two full PASS reviews | 1,432,268 | 391.0s |
| Two same-tuple merge confirmations | 702,083 | 199.2s |
| Reduction | 51.0% | 49.0% |

The full replay cost more than #1089's standard arm: +33.9% tokens, +42.2%
agent runtime, and +37.7% wall time. The principal reason is the newly observed
defect/rework cycle; the #1089 standard arm reported zero findings and zero
rework. Therefore this run demonstrates retry idempotence and prompt shrinkage,
not a claim that stronger review always reduces total delivery cost.

Machine-readable values are in
[the replay summary](assets/stable-head-reviewer-replay-20260714/summary.json).
