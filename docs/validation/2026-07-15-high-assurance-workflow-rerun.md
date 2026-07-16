# High-assurance workflow rerun

**Execution date:** 2026-07-16 UTC

**Issue:** #1117

**Verdict:** keep disabled pending a named defect.

## Decision

Keep the high-assurance profile disabled. The fixed arm did not reach reviewer,
external-review, merge, issue-2, or holdout evidence because the one-shot
validation supervisor failed closed on its first PR-present forge snapshot.

The named defect is in the validation evidence path: `forge_snapshot` performs
six sequential `gh` subprocess reads once a PR-link comment exists, while the
approved supervisor gives the entire snapshot one five-second deadline. The
live arm exceeded that deadline. A persisted read-only post-abort invocation
of the same function took 7.355921 seconds while GitHub REST and GraphQL quotas
remained above 97%. This does not prove the profile itself failed, but it
prevents the required operational certification.

## Controlled protocol

The protocol, supervisor, fixed public regression suite, release, capability
manifest, repository, issue bodies, identities, and actual workflows were
frozen before activation. The main evidence is:

- [pre-registered protocol](assets/high-assurance-workflow-rerun-20260715/protocol.md)
- [sanitized preflight](assets/high-assurance-workflow-rerun-20260715/preflight.json)
- [actual maker workflow](assets/high-assurance-workflow-rerun-20260715/maker-WORKFLOW.md)
- [actual reviewer workflow](assets/high-assurance-workflow-rerun-20260715/reviewer-WORKFLOW.md)
- [machine-readable summary](assets/high-assurance-workflow-rerun-20260715/summary.json)
- [forge timeline](assets/high-assurance-workflow-rerun-20260715/forge-timeline.json)
- [post-abort read-only probe](assets/high-assurance-workflow-rerun-20260715/post-abort-probe.json)
- [state manifest](assets/high-assurance-workflow-rerun-20260715/raw-state-manifest.json)
- [supervisor-persisted state payloads](assets/high-assurance-workflow-rerun-20260715/raw-state/)

| Variable | Frozen value |
| --- | --- |
| Worker | official v0.1.16 darwin/arm64 release; commit `a7a973cb83c42c60f7f8d9d11c9d7b7dda08159f` |
| Codex CLI | 0.144.4 |
| Model / reasoning | `gpt-5.6-sol` / high |
| Sandbox / approval | `danger-full-access` / `never` |
| Seed | `5e6264cdcfd2decb53491e37de4f825878339487` |
| Identities | maker `xrf-9527`; reviewer/setup/operator `zjlgdx` |
| Concurrency | one issue and one agent per worker |
| Required check | `build-test` |
| Public regression suite | SHA-256 `cc389a18573c0687b77ee510f1779750322300a302483293599f6779ed153a21` |

The direct app-server probe proved the effective model, high reasoning,
`danger-full-access`, and `never` approval settings. The v0.1.16 release
attestation verified. Both actual workflow files were mode 0444 and were
re-hashed continuously; the reviewer workflow contains the #1090 exact-tuple
fast path and #1102 native-close semantics.

All differences from #1089 were recorded before activation: Codex 0.144.4 was
required by the published v0.1.16 worker instead of #1089's 0.144.3; #1090
removed the redundant nested local-review layer; #1102 changed merge closure
to native `Closes #N`; and this run added the capability-surface manifest that
#1089 lacked. A proposed triage-only `bytevane` operator was rejected by
GitHub's user-owned-repository permission model before activation, so the run
restored #1089's `zjlgdx` setup/reviewer/operator identity without granting
`bytevane` push permission.

## Observed run

Times are UTC. The operator performed only the pre-registered `aiops:todo`
activation for issue 1. No code, PR, label, workflow, check, review, or setting
was repaired after activation.

| Time | Event |
| --- | --- |
| 02:06:33.332 | Supervisor recorded the issue-1 activation request and zero-token baselines. |
| 02:06:35.517 | The sole `aiops:todo` activation completed. |
| 02:07:09.242 | Maker claim 1 started with the frozen workflow path. |
| 02:10:11 | Maker opened PR #3 on head `71cb71a31ecc3fde684ddbc7d9cc848ead99b5fa`. |
| 02:10:26 | Maker linked PR #3 from issue 1. |
| 02:10:34.245 | The first PR-present forge snapshot exceeded five seconds; the supervisor failed closed. |
| 02:10:34.245 | SIGTERM followed 0.261 ms after detection; both workers exited 0 without SIGKILL. |
| 02:10:39 | The already-running `build-test` completed successfully after the abort. |

At termination, maker claim 1 had consumed 580,664 worker-observed tokens and
205.001 seconds of agent runtime. Reviewer had zero claims and zero
worker-observed tokens. Issue 1 remained open with `aiops:todo`; issue 2
remained open, unlabeled, and never activated. PR #3 remained open and
unreviewed.

| Pre-registered bound | Observed | Result |
| --- | ---: | --- |
| Worker claims / issue | 1 / 4 | below limit |
| Worker-observed tokens / issue | 580,664 / 3,500,000 | below limit |
| External exact-tuple wait | not started / 600s | not reached |
| Wall time / issue | 240.913 / 1,800s | below limit |
| Required forge evidence | snapshot exceeded 5.000s | fail closed |

The supervisor produced 46 successful forge snapshots. The last completed at
02:10:26.386, but its issue-comments result did not yet include the same-second
PR-link comment, so it did not discover the PR. The next snapshot
followed the PR-present six-read path and breached the overall deadline. The
required check, exact tuple, zero reviews, and zero review threads were
confirmed only by a read-only GitHub reread after termination and are marked as
post-abort evidence.

## Comparison with the valid #1089 standard arm

The valid standard arm used 4,206,604 worker-observed tokens, 1,058.740 seconds
of agent runtime, and 1,166 seconds wall for two accepted issues. This aborted
arm consumed 13.80%, 19.36%, and 20.66% of those totals respectively, but it
stopped during issue-1 maker delivery. These percentages describe partial cost,
not relative efficiency, and there is no accepted-issue normalization.

The fixed public regression suite was not run: the protocol permits it only
after both issues close, and this backlog was incomplete. External GitHub
Codex and other external reviewers were not invoked; their accounting surfaces
remain unmeasured. Otherwise unreported nested/subagent usage also remains
unmeasured rather than zero.

## Interpretation

This run provides no evidence that SPEC's one-second continuation retry caused
waste: no continuation occurred. Therefore #1117's condition for a separate
continuation-semantics issue was not met.

The next valid experiment must first remove the named evidence-path defect and
then start from a fresh repository and a newly frozen protocol. This stopped
repository must not be resumed or repaired into a valid arm. Until a fresh arm
reaches reviewer, exact-tuple external review, native merge/closure, and the
fixed public regression suite, the high-assurance profile remains disabled.
