# High-assurance Linux container rerun protocol

This protocol is frozen before either fresh issue is activated. It governs the
single public run for issue #1128. It does not repair or continue #1117 and does
not reuse that issue's repository, supervisor, or protocol.

## 1. Question and allowed verdict

The run asks whether the fixed high-assurance maker/reviewer workflow can
complete two issues inside an auditable Linux container boundary while meeting
its evidence and stop contracts. Compare it with #1089's valid standard arm:
2/2 accepted issues, 4,206,604 worker-observed tokens, 1,058.7 seconds of worker
runtime, 1,166 seconds wall time, zero rework, and a passing fixed holdout.

The final report must state exactly one verdict:

1. `operationally ready`;
2. `keep disabled pending a named defect`;
3. `remove the profile`.

A successful single run is operational evidence, not a universal default or a
statistical claim.

## 2. Fixed inputs and reused baselines

| Input | Frozen value |
| --- | --- |
| Fresh public repository | `zjlgdx/aiops-workflow-bench-high-20260717-1128-v1` |
| Fresh seed commit | `03865606188dbda2161cc1c98c40f454312fcdd4` |
| Seed tree | `23e0ec67a9696108ffd32c1b503bfae5a61e7b34` |
| Historical same-tree commit | `5e6264cdcfd2decb53491e37de4f825878339487` |
| Issue 1 canonical SHA-256 | `b847bfeb94d91ba8a3be59782d6929d553bcaafb2f146cd9b5ee026901dcaf4b` |
| Issue 2 canonical SHA-256 | `39c75e9c430b83abe49bc323af58c9ace47b834165c857038e340ef0600c80b6` |
| Order | activate issue 2 only after issue 1 is closed with a merged PR |
| Required check | strict `build-test` |
| Fixed holdout SHA-256 | `cc389a18573c0687b77ee510f1779750322300a302483293599f6779ed153a21` |
| Maker WORKFLOW SHA-256 | `b4efe35c2dceb6706a36e78e8be275a62d60c19a6fe1d33a08f114d3091a3412` |
| Reviewer WORKFLOW SHA-256 | `ec9228cf559a322913f3bf49fab10a96628633102af80639939c94674c43c548` |

The holdout bytes, canonical issue content/order, seed tree, fixed profile
values, and WORKFLOW prompt semantics are allowed baselines. The two WORKFLOW
files point only at the fresh repository and container-local roots. The GO
record freezes the actual absolute paths and hashes of this protocol, the new
one-run controller, its tests, both WORKFLOW files, and the holdout.

## 3. Runtime profile and preregistered deviations

The worker image is built for `linux/arm64` from the existing `codex-worker`
target at release commit
`a7a973cb83c42c60f7f8d9d11c9d7b7dda08159f`, with worker `v0.1.16` and
Codex `0.144.4`. The image is built with the host UID/GID only so 0600 bind
secrets remain readable by the image's default `aiops` user. The GO record
binds the resulting image digest, release Dockerfile hash, Docker engine and
architecture, and observed in-container versions.

Both workers use `gpt-5.6-sol`, `high` reasoning, `danger-full-access`, the
effective non-interactive `never` approval policy, one concurrent agent,
30-second worker polling, and the frozen claim/runtime settings in their
WORKFLOW files. Maker is `xrf-9527`; reviewer/setup/operator is `zjlgdx`.

Preregistered deviations from #1117/#1089 are:

- Darwin binary execution becomes one dedicated Linux container per worker;
- the fresh commit differs while preserving the exact seed tree;
- each worker receives a separate writable `CODEX_HOME` seeded only with the
  local ChatGPT auth needed by Codex, rather than inheriting an unbounded host
  capability tree;
- the controller polls state every second and starts a staged forge snapshot
  every 15 seconds; each forge stage has a five-second deadline.

These are comparison limitations, not production changes. No Dockerfile,
Compose service, production package, worker/orchestrator phase, generic
supervisor, process scanner, pidfd/`/proc` inventory, restart policy, or
cross-platform abstraction is added.

## 4. GO/NO-GO activation gates

Activation is `NO-GO` until one durable record proves all of the following:

- freeze timestamp and SHA-256 values for protocol/controller/tests,
  WORKFLOW files, and holdout;
- image digest, release commit and Dockerfile hash, worker/Codex versions,
  Docker engine/architecture, and the exact default container user;
- observed maker/reviewer/operator identities and repository permissions;
- complete relevant `main` protection, required check, merge settings,
  lifecycle labels, successful seed check, and both issues open and unlabeled;
- #1090 exact-tuple checkpoint/fast-path and one-trigger semantics, plus #1102
  maker `Refs #N`, reviewer squash-body `Closes #N`, and merged-open fallback;
- every effective runtime setting and both actual WORKFLOW paths/hashes;
- all maker/reviewer workspace and mirror sources exist, are empty, distinct,
  and non-nested before container creation; every per-worker `CODEX_HOME` is
  distinct;
- exact full container IDs, no published state port, and exact bind
  `Source` -> `Destination` with `RW=true` for workspace, mirror, and
  `CODEX_HOME` and each role-isolated home-config directory, plus exact 0600
  host sources and `RW=false` destinations for each GitHub token, state token,
  and state request config;
- using the image's default user, every writable root passes
  create/write/fsync/read/delete and reports `aiops` identity;
- each fresh worker is quiescent with a zero token baseline; its unauthenticated
  loopback state request returns 401, then a bounded `docker exec` with the
  mounted 0600 state request config returns schema-valid state JSON after the
  controller proves that config contains the mounted state token;
- the live controller's fixed forge stages persist start/completion/failure by
  poll/stage ID. A timeout injection fails at `03-pr`, preserves completed
  issue/comments results, and reaps the exact controller-owned process group;
- the live stop function is injected against both containers after starting a
  TERM-resistant `setsid` process. It durably records both stop-request
  timestamps before waiting for either command, then records exact ID,
  `docker wait`, `Running=false`, `Pid=0`, exit status, and terminal timestamp;
- controller tests, a secret-value scan, and an independent simplification and
  correctness review pass.

State is never published on a host port. The state token, GitHub tokens,
role-specific gh config, Codex auth, raw headers, and app-server streams stay
outside committed evidence. The exact container ID is the shutdown proof
boundary; no claim is made that TERM reached every child or that this proves
#1117's Darwin behavior.

## 5. Stop, operator, evidence, and outcome contract

For each issue, fail closed on the first observed breach of:

- more than four worker claims, or four claims with another retry pending;
- more than 3,500,000 worker-observed tokens, with process-total delta equal
  to issue-attributed ended/running/blocked usage;
- 600 seconds after an exact-tuple `@codex review` trigger without a REST review
  from bot numeric ID `199175422` at that head and at/after the trigger;
- 1,800 seconds from activation without a merged PR, closed issue, and no live
  claim for that issue; completion additionally requires the reliable external
  exact-tuple review signal.

Counter regression, attribution mismatch, duplicate exact-tuple trigger,
trigger without its earlier exact checkpoint, unexpected active issue, missing
or malformed state, PR discovery or authorship outside the frozen maker
identity, stage timeout/failure, workflow/hash/container drift, an unconsumed
nested review-comment page, or closed issue without a merged PR also fails
closed. The controller's six sequential forge stages are `01-issue`,
`02-issue-comments`, `03-pr`, `04-pr-comments`, `05-reviews`, and outer-paginated
`06-review-threads`; stage 6 records nested comment page state and rejects any
thread whose first 100 comments are not complete instead of adding a second
pagination subsystem. A missing PR completes after stage 2.

After activation, the operator may only add `aiops:todo` to issue 1, add it to
issue 2 after issue 1 meets completion, read evidence, stop both exact
containers, and run the fixed holdout after both issues close. No repair of
code, configuration, branch, PR, labels, checks, reviews, merge, or issue state
is allowed. A breach first fsyncs evidence, then invokes the same parallel
container stop path used by the successful end.

Persist every changed worker claim/continuation/usage state, lifecycle handoff,
exact head/base tuple, required checks, complete REST review/comments and
GraphQL review threads, reliable external signal, merge/native closure,
activation, breach/stop sequence, holdout result, and all known unmeasured
usage. External Codex/reviewer and otherwise unreported nested/subagent tokens
are `unmeasured`, never zero. Do not change SPEC one-second continuation
semantics unless separate current evidence independently satisfies #1117's
follow-up condition.
