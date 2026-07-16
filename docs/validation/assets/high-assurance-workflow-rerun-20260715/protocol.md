# High-assurance workflow rerun preregistration and pre-activation amendment

The initial decision rule and stop contract were recorded on 2026-07-15 before
creation of the disposable repository. After repository creation but before
issue activation or worker start, the live permission check produced the
explicit feasibility amendment in item 2 below; no outcome data existed when
that amendment was frozen.

## Question and decision rule

Issue #1117 asks whether the high-assurance GitHub maker/reviewer profile is
operational after #1090 made stable-tuple retries idempotent and #1102 made the
squash commit close the issue natively. This is a fixed public replay, not an
unseen holdout or a statistically universal benchmark.

Compare the result with #1089's valid standard arm. State exactly one verdict:

1. `operationally ready`;
2. `keep disabled pending a named defect`;
3. `remove the profile`.

The valid standard comparison is 2/2 issues accepted, 4,206,604
worker-observed tokens, 1,058.7 seconds worker runtime, 1,166 seconds wall time,
zero rework, and a passing fixed regression suite.

## Fixed source and backlog

| Variable | Fixed value |
| --- | --- |
| Source repository | `zjlgdx/aiops-workflow-bench-high-20260714-1089-v8` |
| Seed commit | `5e6264cdcfd2decb53491e37de4f825878339487` |
| Seed tree | `23e0ec67a9696108ffd32c1b503bfae5a61e7b34` |
| Issue 1 | `feat: persist and list todos` |
| Issue 1 canonical JSON SHA-256 | `b847bfeb94d91ba8a3be59782d6929d553bcaafb2f146cd9b5ee026901dcaf4b` |
| Issue 2 | `feat: complete and filter todos` |
| Issue 2 canonical JSON SHA-256 | `39c75e9c430b83abe49bc323af58c9ace47b834165c857038e340ef0600c80b6` |
| Order | activate issue 2 only after issue 1 is closed |
| CI | required `build-test`: `go test ./...`, then `go vet ./...` |
| Fixed regression suite | adjacent `holdout.sh` |
| Regression suite SHA-256 | `cc389a18573c0687b77ee510f1779750322300a302483293599f6779ed153a21` |

Canonical issue JSON means one compact, key-sorted UTF-8 line containing only
`body`, `number`, and `title`, followed by LF. Task 2 must export the old and
new values and prove equality before activation.

The regression suite is already public in this repository. Agents may know its
contents through source, memory, or search; the report must not call it hidden.

## Fixed runtime

| Variable | Fixed value |
| --- | --- |
| Worker release | official `aiops-platform` v0.1.16 darwin/arm64 asset |
| Release/tag commit | `a7a973cb83c42c60f7f8d9d11c9d7b7dda08159f` |
| Asset SHA-256 | `13e4c0f6830f350f83f6545d47c4beba4efd2dc8aa9d42bff5e6394825f84c0a` |
| Included fixes | #1090 merge `480844b2f60baec5ebe44e82ee5710343d2c044a`; #1102 merge `9702dc07732a88f7547317678baec4e9db178435` |
| Codex CLI | 0.144.4 |
| Model / reasoning | `gpt-5.6-sol` / `high` |
| Sandbox | `danger-full-access` |
| Concurrency | one issue and one agent per worker |
| Poll cadence | worker 30 seconds; supervisor state at most 250 ms, forge at most 5 seconds |
| Maker / reviewer | `xrf-9527` / `zjlgdx` |
| Operator | `zjlgdx`, shared with reviewer/setup as in #1089 |

The v0.1.16 release must be identified by release API, attestation, checksum,
`worker --version`, and state API version. The worker, workflows, workspaces,
mirrors, auth homes, and process IDs are frozen before activation. No process
manager or restart policy may replace either worker.

## Pre-registered deviations and limitations

1. #1089 used Codex 0.144.3. v0.1.16's generated app-server schema and
   real-mode doctor require exactly 0.144.4, so this rerun uses 0.144.4 instead
   of bypassing the version gate.
2. The initial rerun protocol proposed triage-only `bytevane` to separate
   activation from reviewer writes. Before activation, the live GitHub API
   rejected that role on this user-owned repository with HTTP 422
   `RepositoryInvitation.permissions invalid`: personal repositories do not
   expose the organization-style `triage` collaborator role. Granting `push`
   would unnecessarily authorize code mutation. The run therefore restores
   #1089's exact identity contract: `zjlgdx` is setup, reviewer, and operator;
   `xrf-9527` remains the distinct maker. The supervisor owns the only two
   operator activation calls and records their timestamps, while final forge
   events remain necessary to distinguish those calls from legitimate reviewer
   writes. This feasibility correction is recorded before activation.
3. The deleted #1089 high workflow cannot be recovered byte-for-byte. The new
   files start from the v0.1.16 examples and are frozen before activation. They
   preserve the high profile's negative/failure-path probes, mandatory external
   Codex gate, exact tuple, and paginated GraphQL thread audit. Per #1090 they
   intentionally remove the redundant nested/delegated local review layer.
4. #1089 did not inventory host skills, global instructions, memories, MCP
   servers, Apps, connectors, or plugins. This binary run preserves normal
   local Codex environment inheritance as required by `AGENTS.md` and records a
   secret-free manifest before activation; equality with the old unrecorded
   capability surface cannot be claimed.
5. The worker reports the workflow path/source on claim rows but not a loaded
   snapshot digest. The accepted operational proof chain is: exact worker argv,
   doctor, read-only file mode, pre/during/post SHA-256 equality, and the first
   running/ended claim row bound to that real path. This is not a cryptographic
   digest of in-memory prompt bytes.
6. v0.1.16 predates #1116's claim-budget clarification. The two workflow-local
   limits remain above this experiment's cross-role 3.5M stop. The supervisor's
   activation baselines and process-total deltas, not the older internal guard,
   are authoritative for this rerun.

## High-profile workflow contract

The maker keeps its non-closing `Refs #N` PR handoff and never approves,
merges, or closes. The reviewer is read-only locally and must:

- take one bounded snapshot containing exact head/base OIDs/name, all REST
  reviews/comments, all paginated GraphQL `reviewThreads`, required checks,
  merge state, and auto-merge state;
- run configured Go gates and real-process negative/failure-path probes once
  for each unseen tuple;
- write the exact reviewer-owned tuple checkpoint on local PASS;
- wait until GitHub can assign the trigger a strictly later whole-second
  timestamp than that checkpoint, because equal timestamps cannot prove order;
- reuse that checkpoint for unchanged tuples instead of repeating local review;
- trigger external `@codex review` at most once per tuple and never infer clean
  from absence, prose, or reactions;
- approve only the reviewed head and land with `--match-head-commit <HEAD>` and
  squash body `Closes #N`;
- use manual close only for the proven merged-but-open fallback.

The only reliable unattended external completion is a REST review object from
bot numeric id `199175422`, with `commit_id` equal to the current head and
`submitted_at` at or after the exact-tuple trigger. Clean prose and reactions
are captured as advisory evidence but do not unlock merge.

## Hard stop contract

The adjacent `supervisor.py` starts both workers and performs only issue 1 and
issue 2 activation. It must persist the breach snapshot before signalling both
worker process groups.

| Limit | Executable interpretation |
| --- | --- |
| Four worker sessions per issue | Count issue-matching `completed_session_usage + running + blocked` claim rows across both workers; do not deduplicate the thread-turn `session_id`. Abort at `>4`, or at `==4` while a continuation is pending, before claim five. |
| 3,500,000 worker-observed tokens per issue | At activation freeze both process `codex_totals.total_tokens`; sum both deltas while exactly one issue is active and require that delta to equal issue-attributed ended/running/blocked usage. Abort on an accounting mismatch or the first observed value above 3,500,000. Preserve last-below, first-above, and detection-to-TERM latency. |
| Ten minutes for external review | Start at the exact trigger comment's `created_at`; require the reliable exact-head review object above. Abort at 600 seconds without it. |
| Thirty minutes wall per issue | Start a monotonic timer before the activation request. Completion requires a merged PR, GitHub issue closed, and no live/blocked/retrying claim for that issue; a closed issue without a merged PR is a native-close breach. Abort at 1,800 seconds until all completion conditions are true. |

Token updates are discrete runner events, so an individual turn can overshoot
the token threshold. The first observed value is the enforceable boundary and
the final total after termination is a worker-observed lower bound.

State API unavailability, missing required state fields, parse failure, process
exit/replacement, token counter regression, unexpected active issue, workflow
mutation, duplicate trigger, or tuple inconsistency fails closed. Forge reads
run on a daemon observation thread with a five-second overall deadline so they
cannot block the 250 ms local stop loop. The stop sequence is: fsync breach
evidence, send `SIGTERM` to both process groups even if a worker leader already
exited, wait the fixed grace, send `SIGKILL` to surviving groups, record exits.
It never changes a lifecycle label to stop work. If any termination event
cannot be persisted, worker safety takes priority: complete the full signal,
grace, kill, wait, and log-handle cleanup sequence, then fail the run with a
classified evidence error rather than returning success or ordinary abort.
Operator interrupts and other `BaseException` paths after worker start use the
same shutdown sequence; OS `SIGTERM` and `SIGHUP` are converted into that path,
including the spawn-to-worker-tracking window. Signals received immediately
before fork through registration of the returned `Popen` are deferred until
ownership is established. Local cleanup and ownership transfer are
signal-guarded, and a process group not proven absent remains owned for the
outer shutdown retry before completion can be recorded. Once shutdown begins,
additional `SIGINT`, `SIGTERM`, and `SIGHUP` signals cannot interrupt its safety
sequence.
Per-process poll, group-signal, and wait failures are accumulated while cleanup
continues, with direct-process kill as a group-signal fallback. On Linux,
same-namespace, non-hidden `/proc` state for the entire worker session
(session ID equals its initial process-group ID) distinguishes terminal-only
(`Z`/`X`/`x`) sessions from sessions with live members. A terminal-only result
requires the same non-empty PID set in two consecutive complete scans;
incomplete, unreadable, hidden, cross-namespace, unstable, or exhausted scan
state fails closed as live. This proof is used only after TERM, KILL, and wait;
raw process-group existence still controls grace waiting and whether SIGKILL is
sent. Shutdown is complete only after every process group is absent or its
worker session is confirmed terminal-only;
otherwise the supervisor records `workers_stop_incomplete`, reports the live
PIDs, and never marks the run stopped. Every worker log close is attempted;
close failures are accumulated, cannot mask a termination-evidence error, and
cannot trigger a second shutdown after workers are already stopped.

## Operator boundary

Before activation, setup may create/configure the repository, accept role
invitations, run dry-run push, run doctor, and freeze evidence. After activation:

- the supervisor's `zjlgdx` operator client may only perform issue 2 activation
  after issue 1 is confirmed closed;
- read-only state/forge capture and pre-registered worker termination are
  allowed;
- no operator code, branch, PR, issue body, lifecycle-label repair, workflow,
  check, review verdict, merge, or close mutation is allowed;
- any configuration/setup defect invalidates the arm; it is not repaired.

External and otherwise unreported Codex, nested-agent, subagent, or human usage
is unmeasured, not zero, and does not consume the worker-observed token limit.

## Activation gate

The run remains **NO-GO** until all are true:

- fresh repository preserves seed commit/tree and exact two canonical issues;
- required check, protection, merge settings, labels, and initial state match;
- maker has write, reviewer/setup/operator is the repository owner, and the
  supervisor executable exposes no operator mutation other than activation;
- official binaries/checksums/attestation and Codex 0.144.4 doctor pass;
- workflows are complete, read-only, hashed, and prove #1090/#1102 semantics;
- workspaces/mirrors are distinct and empty; workers are fresh and quiescent;
- secret-free capability manifest and preflight JSON are frozen;
- supervisor unit tests and a two-process termination injection pass in under
  one second;
- evidence allowlist and secret scan exclude tokens, auth homes, environment
  files, headers, app-server streams, and raw credentials.
