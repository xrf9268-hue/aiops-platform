# High-Assurance Workflow Rerun Plan

> **For Codex:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to execute this plan task by task.

**Goal:** Re-run issue #1117's fixed two-issue high-assurance benchmark with
v0.1.16, enforce every pre-registered stop limit outside the workers, and
publish enough immutable evidence for one of the three required verdicts.

**Architecture:** A fresh public GitHub repository reuses the exact #1089 root
commit and issue bodies. Separate maker and reviewer workers run the official
v0.1.16 binary with isolated GitHub auth homes, workspaces, and mirror roots.
A one-shot validation supervisor starts both workers, proves the loaded workflow
paths against read-only files and expected SHA-256 values, performs only the two
allowed label activations as a dedicated operator identity, samples both state
APIs, and terminates both workers
at the first cross-role session, token, external-signal, or wall-time limit.
The supervisor is a committed validation asset, not a worker phase or recurring
service.

**Tech Stack:** Go worker release v0.1.16, Codex CLI 0.144.4, Python 3 standard
library, GitHub CLI/API, Bash holdout, Markdown and JSON evidence.

---

## Task 1: Freeze the protocol and executable stop contract

**Files:**

- Create: `docs/validation/assets/high-assurance-workflow-rerun-20260715/protocol.md`
- Copy unchanged: `docs/validation/assets/high-assurance-workflow-rerun-20260715/holdout.sh`
- Create: `docs/validation/assets/high-assurance-workflow-rerun-20260715/supervisor.py`
- Create: `docs/validation/assets/high-assurance-workflow-rerun-20260715/supervisor_test.py`

### Step 1: Write failing supervisor contract tests

Cover these pure functions with synthetic maker/reviewer state payloads:

1. count one worker claim for every issue-matching ended, running, or blocked
   entry across both roles; do not deduplicate `session_id`, which is a
   thread-turn identifier rather than the claim/session limit's unit;
2. freeze each worker's `codex_totals.total_tokens` at issue activation and
   sum the two process deltas while exactly one issue is active; cross-check
   against issue-attributed ended/running usage and fail closed on regression;
3. stop when tokens exceed 3,500,000, wall time reaches 1,800 seconds, a fifth
   claim appears, or four claims have been consumed and a continuation is
   pending;
4. bind the ten-minute external gate to the current
   `(headRefOid, baseRefOid, baseRefName)` and accept only a review object from
   bot numeric id `199175422` with `commit_id` equal to that head and
   `submitted_at` at/after the one trigger; require the reviewer checkpoint to
   have a strictly earlier GitHub timestamp than the trigger because equal
   whole-second timestamps cannot prove ordering; comments and reactions are
   advisory, not reliable completion;
5. reject stale-head/base signals and keep nested/subagent/external usage marked
   unmeasured.

Run:

```bash
python3 -m unittest docs/validation/assets/high-assurance-workflow-rerun-20260715/supervisor_test.py
```

Expected: FAIL because `supervisor.py` does not exist yet.

### Step 2: Implement the one-shot supervisor

The supervisor must:

- accept two immutable workflow paths and SHA-256 values, two worker commands,
  two loopback ports, the repo, issue numbers `1,2`, and a setup `GH_CONFIG_DIR`;
- start both workers, wait for `/readyz`, require `/api/v1/state` to report
  `workflow_source=file` and the exact real path, and re-hash both read-only
  files before activation and throughout the run. The idle state response has
  no workflow path, so bind the actual state-API path/source on the first
  running/ended claim and fail closed if it differs;
- record a conservative activation timestamp before it adds `aiops:todo` to
  issue 1, activate issue 2 only after issue 1 is closed, and perform no other
  GitHub mutation;
- sample local worker state at most every 250 ms and forge state every 5 s;
- perform forge reads outside the local stop loop with a five-second overall
  request deadline, rejecting non-advancing pagination cursors;
- aggregate sessions/tokens per issue across maker and reviewer;
- cross-check the process-token delta against issue-attributed
  ended/running/blocked usage and reject incomplete state schemas;
- fail closed on an unavailable state API, changed worker PID, counter
  regression, unexpected active issue, workflow change, or tuple/trigger
  inconsistency;
- persist the last-below and first-above/breach snapshots and detection latency,
  then terminate both workers immediately on a stop decision; never repair
  labels, code, PRs, reviews,
  checks, settings, or merges;
- terminate cleanly after both PRs merge natively close their issues and
  preserve JSONL lifecycle data and worker logs under the run root; treat a
  closed issue without a merged PR as a breach rather than success.
- signal both process groups even when a worker leader has already exited, and
  clean up a first worker if the second worker fails to start.

### Step 3: Make the tests pass and perform a dry-run failure injection

Run the unit test above, then run the supervisor against fake loopback state
servers where the combined token value crosses from 3,499,999 to 3,500,001.
Expected: both fake worker processes receive termination and the final event is
`abort:worker_tokens_exceeded`.

### Step 4: Freeze the pre-registered protocol

Record before repository creation or activation:

- seed `5e6264cdcfd2decb53491e37de4f825878339487` and the canonical issue JSON;
- maker `xrf-9527`, reviewer/setup `zjlgdx`, operator `bytevane`;
- v0.1.16 release commit `a7a973cb83c42c60f7f8d9d11c9d7b7dda08159f`;
- Codex 0.144.4, `gpt-5.6-sol`, high reasoning,
  `danger-full-access`, one issue/agent per worker, fixed issue order;
- the four stop limits and exact crossing rules;
- the standard-arm comparison values: 4,206,604 tokens, 1,058.7 seconds
  worker runtime, and 1,166 seconds wall time;
- known unmeasured sources: external GitHub Codex, other external reviewers,
  and otherwise unreported nested/subagent use;
- pre-registered runtime deviation: #1089 used Codex 0.144.3, but v0.1.16's
  generated app-server contract and real-mode doctor require exactly 0.144.4;
  using 0.144.4 is necessary to test the published worker without bypassing
  its version gate;
- pre-registered audit deviation: #1089 used `zjlgdx` for both reviewer and
  operator. The rerun preserves maker/reviewer identities but uses
  `bytevane` only for activation so forge actors can prove that no operator
  mutation is mistaken for reviewer behavior;
- capability-surface limitation: #1089 did not inventory inherited Codex
  skills, plugins, MCP servers, Apps, or global instructions. Preserve the
  normal local binary inheritance required by AGENTS.md, but record a
  secret-free manifest and hashes before activation;
- pre-registered profile changes: #1090 intentionally removes the redundant
  nested/delegated local review layer while retaining unseen-tuple local review,
  mandatory external Codex, negative-path probes, and GraphQL thread audit;
  #1102 uses the squash body `Closes #N` for native closure.

Copy the existing holdout byte-for-byte and prove identical SHA-256 values.
Call it the fixed public regression suite, not a hidden holdout, because it is
already committed.

## Task 2: Freeze release, identities, workflows, and the fresh repository

**Files:**

- Create during execution only: `/tmp/aiops-workflow-benchmark-20260715-1117/**`
- Create later from sanitized capture:
  `docs/validation/assets/high-assurance-workflow-rerun-20260715/preflight.json`

### Step 1: Install and verify the pinned binaries

Download the official darwin/arm64 v0.1.16 release asset, SHA256SUMS, SBOM, and
attestation into the run root. Require:

```text
worker --version = v0.1.16
release/tag commit = a7a973cb83c42c60f7f8d9d11c9d7b7dda08159f
asset SHA256 = 13e4c0f6830f350f83f6545d47c4beba4efd2dc8aa9d42bff5e6394825f84c0a
```

Install `@openai/codex@0.144.4` under the run root and prepend its bin directory
to `PATH`; require `codex --version` to report 0.144.4. Do not use the host's
currently observed 0.144.1 binary.

### Step 2: Create isolated role auth homes

Materialize keyring tokens into mode-0700, ignored run-root auth homes without
printing tokens. Verify:

```text
maker=xrf-9527
reviewer=zjlgdx
setup=zjlgdx
operator=bytevane
maker != reviewer
operator != maker and operator != reviewer
```

Run `gh auth setup-git` for each role. Never commit these homes or tokens.
Capture a secret-free inventory of inherited Codex capability names, versions,
manifest hashes, and effective non-secret model/reasoning settings.

### Step 3: Generate and freeze the actual workflows

Start from the v0.1.16 maker/reviewer examples. Change only:

- repository owner/name/clone URL;
- distinct absolute workspace roots;
- the configured Go gates: `gofmt -l .`, `go test ./...`, `go vet ./...`;
- reviewer high-profile text: mandatory negative/failure-path real-process
  probes, mandatory one-trigger external Codex gate, and complete paginated
  GraphQL `reviewThreads` evidence;
- no nested/delegated local review, as deliberately changed by #1090.

Pin the actual app-server invocation to model `gpt-5.6-sol` and reasoning
`high`; the host config currently says `xhigh`, so the run-root preflight must
prove the workflow command override and record the effective session profile.

Keep the exact-tuple checkpoint, same-tuple fast path, one trigger per tuple,
exact-head approval, `--match-head-commit`, and native `Closes #N` behavior.
Set the files mode 0444, record real paths and SHA-256 values, and save a
sanitized copy of each as validation evidence. Run
`worker --doctor --deploy=binary --mode=real <workflow>` for both roles with
the pinned Codex binary. Record the explicit run argv; `--print-config` accepts
a repository workdir rather than an explicit workflow path, so it is not used
as false proof for these out-of-repo workflow files.

### Step 4: Create the disposable repository without activating issues

Create a fresh public repository named
`zjlgdx/aiops-workflow-bench-high-20260715-1117-v1`. Push the unchanged root
commit as `main`; require its SHA to remain exactly the fixed seed. Create the
two canonical issues, in order, with no active labels. Add the five lifecycle
labels, invite `xrf-9527` with `push` and `bytevane` with `triage`, accept with
their respective identities, require maker `WRITE`, and keep the operator
unable to push or merge.

Configure the same required `build-test` status, one approval, stale-approval
dismissal, last-push approval, conversation resolution, enforced admins, and
no force-push/deletion. Preserve squash support. Wait for the seed check and
prove both identities can read while maker can dry-run push before activation.

Export canonical issue JSON, repo settings, branch protection, role logins,
seed tree, workflow hashes, and the secret-free capability manifest into the
sanitized preflight record.

## Task 3: Execute the fixed two-issue arm

**Files:**

- Runtime only: `/tmp/aiops-workflow-benchmark-20260715-1117/evidence/**`

### Step 1: Start the supervisor

Pass the two role-specific tracker tokens only through process environment,
the two isolated `GH_CONFIG_DIR` values to the corresponding agent processes,
the operator `GH_CONFIG_DIR` only to its activation/read calls, distinct
workspace/mirror roots, and the pinned Codex `PATH`. The supervisor
must prove worker readiness, version, workflow source/path, and file hash before
performing issue 1's sole activation.

### Step 2: Observe issue 1 without intervention

Do not edit code, PRs, issue bodies, labels, workflows, reviews, settings, or
checks. Read-only capture is allowed. Require the supervisor to record every
session/continuation, handoff, tuple, check state, REST review, paginated GraphQL
thread state, external trigger/signal, tokens, and unmeasured source.

If a stop decision occurs, end the arm and proceed directly to reporting. Do
not activate issue 2 or repair issue 1.

### Step 3: Activate and observe issue 2

Only the supervisor may add `aiops:todo`, and only after it has observed issue
1 closed. Apply the identical observation and stop contract. Finish when issue
2 closes or a stop decision occurs.

## Task 4: Holdout, evidence normalization, and verdict

**Files:**

- Create: `docs/validation/2026-07-15-high-assurance-workflow-rerun.md`
- Create: `docs/validation/assets/high-assurance-workflow-rerun-20260715/summary.json`
- Create: `docs/validation/assets/high-assurance-workflow-rerun-20260715/forge-timeline.json`
- Create: `docs/validation/assets/high-assurance-workflow-rerun-20260715/raw-state-manifest.json`
- Create selected sanitized files under:
  `docs/validation/assets/high-assurance-workflow-rerun-20260715/raw-state/`

### Step 1: Run the unchanged holdout when both issues close

Fresh-clone the final default branch and run:

```bash
bash docs/validation/assets/high-assurance-workflow-rerun-20260715/holdout.sh <fresh-clone>
```

If the arm stopped early, record holdout as not eligible/incomplete; do not
patch the disposable repository.

### Step 2: Normalize evidence without repairing source records

Preserve selected full state responses, hash every committed raw file, and
retain raw input/output/total inconsistencies as observed. Strip credentials,
auth homes, app-server streams, and unnecessary machine-specific paths. Record
all excluded token sources as unmeasured rather than zero.

### Step 3: State exactly one required verdict

Compare against the valid #1089 standard arm and choose exactly one:

- `operationally ready`;
- `keep disabled pending a named defect`;
- `remove the profile`.

If the run demonstrates waste caused specifically by SPEC's one-second
continuation after prompt/tracker state was already sufficient, open a separate
evidence-backed issue; otherwise do not create one.

## Task 5: Review, verify, publish, and merge

### Step 1: Run local asset and repository gates

At minimum run:

```bash
python3 -m unittest docs/validation/assets/high-assurance-workflow-rerun-20260715/supervisor_test.py
bash -n docs/validation/assets/high-assurance-workflow-rerun-20260715/holdout.sh
gofmt -l $(git ls-files '*.go')
go mod tidy
git diff --exit-code -- go.mod go.sum
go vet ./...
go test -run '^TestProductionGoFilesStayWithinSizeBudget$' -count=1 ./scripts
go test -race -covermode=atomic ./...
go build ./cmd/worker ./cmd/tui
```

Run the Docker E2E gate because Docker is available:

```bash
go test -tags e2e -race -timeout 15m ./test/e2e/...
```

### Step 2: Commit and review against the fixed base

Use base `b1180b93b8b36d45a8f4a419cbdf0243d7e44f11`. Commit the scoped validation
assets, then run independent Standards and Spec reviews plus an adversarial
stop/evidence audit against that same base. Fix confirmed findings, recommit,
and repeat until all three pass on one exact head.

### Step 3: Open the issue-closing PR and follow through

Push `codex/issue-1117-high-assurance-validation`, open a Conventional Commit
PR with `Closes #1117`, include the required SPEC and size classifications, and
record local/Docker gates. Request `@codex review` once for the exact PR head;
if quota blocks it, record the exact response but do not treat it as local
failure. Re-read live checks, reviews, and unresolved threads for the exact
head. With local gates and review clean, merge and confirm both the PR and #1117
are closed.
