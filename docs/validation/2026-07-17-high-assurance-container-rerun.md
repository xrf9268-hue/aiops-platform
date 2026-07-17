# High-assurance Linux container rerun

**Execution date:** 2026-07-17 UTC

**Issue:** #1128

**Verdict:** keep disabled pending a named defect.

## Decision

Keep the high-assurance profile disabled. The fresh Linux-container arm failed
closed on issue 1 after four worker claims, with a fifth continuation pending.
Issue 2 was never activated and the fixed holdout was not run.

The named defect is runner protocol drift, tracked by
[#1129](https://github.com/xrf9268-hue/aiops-platform/issues/1129). The Go
app-server runner still uses a `turn/completed.params.continue` value to decide
whether an active issue receives another turn in the same session. That field
is absent from the pinned Codex 0.144.4
[notification schema](../../internal/runner/testdata/codex_app_server_protocol_v0_144_4.v2.schemas.json).
The live runner therefore ended each app-server session after one normal turn
and promoted remaining work to a new outer claim.

This differs from both [SPEC §7.1 and
§16.5](../research/SPEC.md), which continue an active issue on the same live
thread up to `agent.max_turns`, and the upstream Elixir `AgentRunner`, which
uses refreshed tracker state as the continuation decision and returns normally
at the turn limit. The orchestrator's one-second continuation retry and the
validation controller's four-claim stop were correct and are not the fix.

## Controlled evidence

The protocol, controller, workflows, holdout, image, identities, repository
settings, empty bind sources, container IDs, and runtime settings were frozen
before activation. The committed evidence is:

- [pre-registered protocol](assets/high-assurance-container-rerun-20260717/protocol.md)
- [GO record](assets/high-assurance-container-rerun-20260717/preflight.json)
- [one-run controller](assets/high-assurance-container-rerun-20260717/controller.py)
- [controller tests](assets/high-assurance-container-rerun-20260717/controller_test.py)
- [actual maker workflow](assets/high-assurance-container-rerun-20260717/maker-WORKFLOW.md)
- [actual reviewer workflow](assets/high-assurance-container-rerun-20260717/reviewer-WORKFLOW.md)
- [fixed holdout](assets/high-assurance-container-rerun-20260717/holdout.sh)
- [durable event stream](assets/high-assurance-container-rerun-20260717/events.jsonl)
- [machine-readable terminal summary](assets/high-assurance-container-rerun-20260717/run-summary.json)
- [read-only post-abort probe](assets/high-assurance-container-rerun-20260717/post-abort-probe.json)

The release image was `linux/arm64`
`sha256:c4418af6b37171c413b46b677873f08ca957f0f452f8c3ba1d1b094acbefa752`,
built from v0.1.16 commit
`a7a973cb83c42c60f7f8d9d11c9d7b7dda08159f` with Codex 0.144.4. Maker was
`xrf-9527`; reviewer/setup/operator was `zjlgdx`. Both exact containers
passed the pre-activation mount, write/fsync/read/delete, and
authenticated-state probes. The same frozen controller separately passed the
forge-timeout injection; dedicated disposable containers passed its
TERM-resistant shutdown path before the final run containers were created.

## Observed run

Times are UTC. After activation, the operator performed only the
pre-registered issue-1 label transition and read-only evidence capture.

| Time | Event |
| --- | --- |
| 04:13:58 | Issue 1 activation completed. |
| 04:14:26 | Maker claim 1 started. |
| 04:18:13 | Maker linked PR #3 at head `fbffd32d…`. |
| 04:18:41 | Maker claim 1 completed and handed off. |
| 04:19:14 | Reviewer claim 2 started. |
| 04:26:17 | Reviewer reproduced the embedded separator defect and requested changes. |
| 04:26:26 | Reviewer stopped when the issue moved to `aiops:rework`. |
| 04:29:29 | Maker documented the rework at new head `32b7cf71…`. |
| 04:29:44 | Maker claim 3 completed and handed off. |
| 04:30:05 | Reviewer claim 4 started on the new exact tuple. |
| 04:34:18 | Reviewer wrote the commit-pinned local PASS checkpoint. |
| 04:34:34 | Reviewer posted the tuple's single `@codex review` trigger. |
| 04:34:52.449 | Claim 4 ended `active_success_no_handoff`; worker queued a one-second continuation. |
| 04:34:52.735 | Controller observed four claims plus one pending retry and recorded `claim_five_pending`. |
| 04:34:53.180 | Both exact containers were terminal with `Running=false`, `Pid=0`, and Docker wait exit 0. |

The first reviewer found a real acceptance failure: LF, CR, or tab in a title
could break the required one-record-per-line, three-field output. The maker
added process-level rejection tests and pushed a new head. The second reviewer
then passed the local rubric, but the issue correctly remained active while the
external exact-tuple signal was pending.

| Claim | Role / outcome | Tokens | Runtime |
| ---: | --- | ---: | ---: |
| 1 | maker / completed | 427,738 | 255.379s |
| 2 | reviewer / reconcile ineligible after rework | 339,940 | 432.054s |
| 3 | maker / completed | 399,526 | 182.590s |
| 4 | reviewer / active success without handoff | 380,526 | 287.009s |
| **Total** |  | **1,547,730** | **1,157.033s** |

Accounting matched the two workers' observed token deltas with no counter
regression.

| Pre-registered bound | Observed | Result |
| --- | ---: | --- |
| Worker claims / issue | 4 plus 1 pending / 4 | fail closed |
| Worker-observed tokens / issue | 1,547,730 / 3,500,000 | below limit |
| External exact-tuple wait | 18.735s at abort / 600s | superseded by claim breach |
| Wall time / issue | 1,258.056 / 1,800s | below limit |

Issue 1 remained open at `aiops:human-review`; PR #3 remained open and
unmerged. Issue 2 remained open, unlabeled, and unactivated. No holdout event
was recorded.

## Post-abort external signal

The exact-head Codex review arrived at 04:36:54, 140 seconds after its trigger
and 121.264 seconds after the terminal breach. It carried the required bot ID
and commit, but also opened an unresolved, non-outdated P1 thread about
concurrent `add` processes losing updates. This read-only observation is
preserved in `post-abort-probe.json`; it did not repair, resume, approve, or
change the verdict of the stopped arm.

## Comparison with the valid #1089 standard arm

The valid standard arm completed two accepted issues with 4,206,604
worker-observed tokens, 1,058.7 seconds of agent runtime, 1,166 seconds wall
time, zero rework, and a passing holdout.

This incomplete high-assurance issue consumed 36.79% of the standard arm's
two-issue tokens, but already consumed 109.29% of its agent runtime and 107.90%
of its wall time, with zero accepted issues. These percentages describe the
cost of a stopped partial run, not normalized efficiency.

External GitHub Codex review tokens, any other external reviewer tokens, and
otherwise unreported nested usage remain unmeasured rather than zero.

## Interpretation

This run independently proves the condition for a separate continuation issue:
an active issue needed another turn, the current Codex notification had no
`continue` field, and the Go runner ended the session instead of using its
remaining in-session turn budget. Issue #1129 limits the fix to the runner and
schema-shaped tests.

Do not change the orchestrator's normal one-second continuation retry, raise
the frozen claim limit, parse GitHub state in the orchestrator, or resume this
repository. A future validation may start only after the runner defect is fixed
and must use a fresh repository and newly frozen protocol. No reusable
supervisor or production Docker subsystem is justified by this result.
