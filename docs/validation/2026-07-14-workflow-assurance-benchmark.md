# Workflow assurance benchmark

**Date:** 2026-07-14

**Issue:** #1089

**Verdict:** directional result; lean is the default, standard is a governance
option, and the tested high profile is not operationally ready.

## Recommendation

Use the lean single-agent profile for small, low-risk changes with required CI.
It completed both issues and was the cheapest successful arm: 4,108,322
worker-observed tokens, 880.4 seconds of agent runtime, and 904 seconds wall
time.

Use maker/reviewer separation only when independent identity or approval is a
repository requirement. It found no extra defect in this sample and cost 2.4%
more tokens, 20.3% more agent runtime, and 29.0% more wall time than lean.

Do not use the tested high-assurance profile as an end-to-end default. Its
negative-path review found one real delimiter defect missed by the holdout and
both lower profiles, but the reviewer returned without a GitHub verdict three
times and had started a fourth retry when the arm was stopped. Keep the
targeted negative checks for high-risk input boundaries; simplify the
overlapping review layers before another full-profile trial.

## Controlled protocol

The protocol and holdout were written before repository creation or issue
activation. Committed evidence:

- [preregistered protocol](assets/workflow-assurance-benchmark-20260714/protocol.md)
- [common holdout](assets/workflow-assurance-benchmark-20260714/holdout.sh)
- [machine-readable summary](assets/workflow-assurance-benchmark-20260714/summary.json)
- [forge timeline](assets/workflow-assurance-benchmark-20260714/forge-timeline.json)
- [raw state manifest](assets/workflow-assurance-benchmark-20260714/raw-state-manifest.json)
- [exact raw state responses](assets/workflow-assurance-benchmark-20260714/raw-state/)

All valid arms used seed commit
`5e6264cdcfd2decb53491e37de4f825878339487`, the same two issue bodies in the
same order, and issue 2 activation only after issue 1 closed.

| Variable | Pinned value |
| --- | --- |
| Worker | official `aiops-platform` v0.1.15 darwin/arm64 release |
| Codex CLI | 0.144.3 |
| Model / reasoning | `gpt-5.6-sol` / high |
| Sandbox | `danger-full-access` |
| Concurrency | one issue and one agent per worker |
| CI | `build-test`: `go test ./...`, `go vet ./...` |
| Holdout | one pre-written real-process shell suite, hidden from agents |
| Cache | fresh workspace and explicit mirror roots for every valid worker |

The worker doctor passed its app-server, auth, model, reasoning, and sandbox
checks. It warned that the release's vendored Codex schema is 0.142.0 while the
host CLI is 0.144.3; no run failure was attributed to that mismatch.

## Profile differences

| Profile | Delivery and assurance shape |
| --- | --- |
| Lean | One maker implements, verifies, opens a closing PR, waits for CI, and squash-merges. |
| Standard | Maker opens a non-closing PR; an independent reviewer reruns gates, approves, merges, and closes. |
| High | Standard plus negative-path checks, nested exact-head review, one external `@codex review`, and unresolved-thread audit. |

The operator performed setup, sequential activation, read-only capture, and
invalid-attempt aborts. No operator changed code, PR content, review verdicts,
or lifecycle labels after a valid activation. The valid high arm was stopped
after three completed no-handoff outcomes; this post hoc limit is disclosed
below.

## Aggregate results

Worker totals include only app-server sessions visible in `/api/v1/state`.
The high total is a lower bound because nested review, review-subagent, and
external `@codex review` tokens were not exposed. The raw input and output
components exceed the reported total by 1,320 tokens for lean, 144 for
standard, and 6 for high; all raw fields are preserved without repair.

| Profile | Accepted | PR result | Holdout | Worker total tokens | Runtime | Wall | Rework | Findings | Intervention |
| --- | ---: | --- | --- | ---: | ---: | ---: | ---: | ---: | --- |
| Lean | 2/2 | 2 merged | PASS | 4,108,322 | 880.4s | 904s | 0 | 0 | activation/capture only |
| Standard | 2/2 | 2 merged | PASS | 4,206,604 | 1,058.7s | 1,166s | 0 | 0 | activation/capture only |
| High | 0/2 | 1 open | FAIL: incomplete backlog | >=4,184,711 | >=939.5s | 965.2s to abort | 0 | 1 | stopped after 3 no-handoff outcomes |

Normalized across accepted issues:

| Profile | Tokens / accepted issue | Runtime / accepted issue | Wall / accepted issue |
| --- | ---: | ---: | ---: |
| Lean | 2,054,161 | 440.2s | 452s |
| Standard | 2,103,302 | 529.4s | 583s |
| High | n/a | n/a | n/a |

## Per-issue and forge evidence

Times are UTC. Every recorded PR head had a successful `build-test` check.

### Lean

Repository:
<https://github.com/zjlgdx/aiops-workflow-bench-lean-20260714-1089-v3>

| Issue | Tokens | Runtime | PR head | Merged | Closed | Reviews |
| --- | ---: | ---: | --- | --- | --- | ---: |
| #1 | 1,433,182 | 303.9s | `b17af3f0a4563de50fe26be8c7f57ada9af4b961` | PR #3 04:17:36 | 04:17:37 | 0 |
| #2 | 2,675,140 | 576.5s | `c56353b01db45fc94897274e63e91e259968f325` | PR #4 04:27:50 | 04:27:51 | 0 |

Issue 2's first session completed as `active_success_no_handoff`; its
continuation finished delivery. The final state records one such outcome and
two terminal reconcile stops.

### Standard

Repository:
<https://github.com/zjlgdx/aiops-workflow-bench-standard-20260714-1089-v5>

| Issue | Maker + reviewer tokens | Runtime | PR head | Merged | Closed | Reviews |
| --- | ---: | ---: | --- | --- | --- | ---: |
| #1 | 1,106,837 + 1,193,075 | 558.9s | `39ee0806fc6013ff41ca6391bec6db0c51ce692f` | PR #3 04:38:50 | 04:39:16 | 1 approval |
| #2 | 1,160,869 + 745,823 | 499.8s | `1fdae6ee5445aa4a617d17c93dfdea17bf211c94` | PR #4 04:48:36 | 04:48:57 | 1 approval |

Issue 1's first reviewer session completed as `active_success_no_handoff`; a
continuation approved and landed the unchanged head. No code rework occurred.

### High

Repository:
<https://github.com/zjlgdx/aiops-workflow-bench-high-20260714-1089-v8>

Issue 1 maker opened PR #3 on
`2510d3385e90a430fa826f0a015c466f3adabfb3`; CI passed at 05:22:09. The
reviewer reproduced one defect: embedded newline or tab delimiters violate the
required one-record-per-line, three-field output contract. The common holdout
did not cover that input boundary.

One external `@codex review` was invoked at 05:29:17 and submitted a
current-head review at 05:32:08, 171 seconds later. It added one inline
suggestion to filter completed records, but that behavior belonged to the
still-unactivated issue 2 and was not counted as an issue 1 defect. Its token
usage is unmeasured.

The high reviewer did not publish its own GitHub verdict. Three sessions ended
as `active_success_no_handoff`; a fourth fixed-head retry was running at the
abort capture. The exact final state shows:

- maker: 906,287 tokens and 199.2 runtime seconds;
- reviewer: 3,278,424 tokens and 740.3 runtime seconds, including 26,111 tokens
  and 30.3 seconds from the in-progress fourth retry;
- issue 1 still open with `aiops:human-review`, PR #3 open, issue 2 inactive.

A fresh clone at the PR head passed `go test ./...` but failed the common
holdout at the missing `done` command, correctly showing that the two-issue
backlog was incomplete.

## Invalid and excluded attempts

| Attempt | Reason excluded | Result before abort |
| --- | --- | --- |
| Initial lean v2 / standard v4 / high v6 round | State evidence was retained as transcript-derived projections rather than exact full API responses. | Superseded by fresh exact-capture reruns; old totals and findings excluded. |
| Initial owner repositories | Branch protection and Actions unavailable due account plan/billing state. | Discarded before a valid arm. |
| Standard v2 | Shortened maker prompt ended before PR handoff. | Two no-handoff loops; no PR. |
| Standard v3 | Maker invoked an overlapping final-review skill. | One no-handoff; no PR. |
| High v4 | `AIOPS_MIRROR_ROOT` omitted. | Stopped before a PR. |
| High v5 | Maker collaborator invitation not accepted. | Push rejected 403; no PR. |
| High v7 | Distinct maker/reviewer mirror roots were accidentally omitted, so both roles contended for one worktree after rework. | Stopped and all findings/results excluded. |

Every replacement used a fresh repository, worker process, workspace, and
explicit mirror root. No invalid attempt was resumed into a valid arm.

## Interpretation and limitations

- This is one two-issue Go CLI backlog, not a statistical ranking.
- Standard demonstrated governance separation, not extra defect prevention.
- High found a real escaped input-boundary defect, but did not deliver an
  accepted issue and has no normalized accepted-issue cost.
- High's worker-observed cost excludes nested/subagent and external review
  tokens, so its reported cost is only a lower bound.
- The high stop after three no-handoff completions was not preregistered. It
  bounds the observation but does not prove a later retry could never finish.
- Exact sampled `/api/v1/state` responses are committed with SHA-256 hashes.
  Auth homes, mirrors, app-server streams, and secrets are intentionally not
  committed. Forge state was re-read live from GitHub for this report.
