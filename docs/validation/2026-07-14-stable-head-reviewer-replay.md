# Stable-head reviewer replay

**Date:** 2026-07-14

**Issue:** #1090

**Verdict:** PASS. Exact-tuple reviewer checkpoints made merged-head
continuations idempotent, while a changed head still forced a complete review.

## Scope

This replay reused the #1089 standard-profile seed, issue bodies, ordering,
worker release, model, CI gate, and preregistered holdout. The disposable
repository was
<https://github.com/zjlgdx/aiops-workflow-bench-standard-20260714-1090-v2>.
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
| Reviewer | 6,892 | 6,406 | -7.1% |
| Combined | 11,233 | 8,845 | -21.3% |

The two workflows and two governance/E2E runbooks contain 988 lines, down from
1,015. Tests are excluded from that net-negative count.

## Replay result

Both issues closed after their PRs merged, and a fresh clone at final main
`9b1ae3e979ca386acec6bab2ab76e236af06f935` passed the #1089 holdout.

| Issue | PR / final head | Merged | Closed | Rework |
| --- | --- | --- | --- | ---: |
| #1 | #3 / `6567e95d4f549402973090d7c5d13e1f8bafdc99` | 06:57:57Z | 07:00:37Z | 0 |
| #2 | #4 / `2b75db6667cabc432a9e61946ef6633d29d8393c` | 07:09:17Z | 07:11:32Z | 0 |

Issue 1 exposed one operational prompt failure: the reviewer delegated a review
flow and returned its clean result without writing a checkpoint. The worker
correctly retried the unchanged tuple, but that retry had to repeat the full
review. The prompt was then tightened with an earned rule requiring the
reviewer to complete the checkpoint and handoff itself. Issue 2, run with that
then-current prompt from its first reviewer claim, completed the full review and
handoff without the extra no-handoff outcome.

Fixed-base and exact-head review after the replay further hardened shared tuple
guards, conditional verification, and approval/auto-merge recovery. Those
changes are contract-tested and included in the current prompt-byte totals;
the replay cost is evidence for the stable-tuple behavior, not a claim that the
post-review wording itself was replayed.

Every passing tuple received one reviewer-owned `COMMENTED` checkpoint with
the exact `(headRefOid, baseRefOid, baseRefName)` and `local-rubric=PASS`, plus
one exact-head approval. After GitHub merged each PR, the next invocation reused
the checkpoint, skipped checkout/configured verification/semantic review, took
one external snapshot, and closed the issue without another review or trigger.

## Tokens, runtime, and retries

Worker-observed totals include all app-server sessions, including the
no-handoff review and merged-head continuations.

| Worker | Tokens | Agent runtime |
| --- | ---: | ---: |
| Maker | 1,804,337 | 409.5s |
| Reviewer | 2,456,281 | 884.1s |
| Combined | 4,260,618 | 1,293.6s |

End-to-end wall time was 1,394 seconds, from issue 1 activation at 06:48:18Z to
issue 2 closure at 07:11:32Z. There were no code rework cycles or confirmed
review findings. Aggregate reviewer cost includes issue 1's disclosed
pre-final-prompt no-handoff session.

The stable-tuple comparison is within this replay, so it is not confounded by
the changed implementation:

| Reviewer path | Tokens | Agent runtime |
| --- | ---: | ---: |
| Two full PASS reviews | 1,248,550 | 442.7s |
| Two same-tuple merge confirmations | 706,560 | 267.4s |
| Reduction | 43.4% | 39.6% |

The full replay was close to #1089's standard arm in tokens (+1.3%) but used
22.2% more agent runtime and 19.6% more wall time. Issue 1's extra full-review
no-handoff is included, so this aggregate is deliberately not presented as a
best-case final-prompt result. The within-run same-tuple comparison is the
cleaner evidence for retry savings.

Machine-readable values are in
[the replay summary](assets/stable-head-reviewer-replay-20260714/summary.json).
