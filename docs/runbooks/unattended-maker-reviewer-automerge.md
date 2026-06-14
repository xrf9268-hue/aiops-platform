# Unattended maker + reviewer + CI-gated auto-merge

A deployment for running **multiple incremental tasks unattended**: a *maker*
worker implements each issue and opens a PR, an independent *reviewer* worker
judges it in a fresh context, and clean PRs land on `main` via **CI-gated
auto-merge** — no human in the merge path, while keeping a maker/checker quality
gate.

This builds on the maker/checker split in
[`reviewer-worker.md`](reviewer-worker.md) (read that first) and the bot/branch
posture in
[`gitea-bot-and-branch-protection.md`](gitea-bot-and-branch-protection.md). It
adds the *landing* step those leave to a human.

Example WORKFLOWs (validated through `worker --print-config`):
[`examples/maker-WORKFLOW.md`](../../examples/maker-WORKFLOW.md) and
[`examples/reviewer-automerge-WORKFLOW.md`](../../examples/reviewer-automerge-WORKFLOW.md)
— the latter is the auto-merge variant of the verdict-only
[`examples/reviewer-WORKFLOW.md`](../../examples/reviewer-WORKFLOW.md).

## Why this shape (boundaries you can't design around)

- **The orchestrator never merges, pushes, or opens PRs** — SPEC §1 / #76. The
  worker is a scheduler/runner + tracker *reader*. "Auto-merge" is therefore
  **not** an aiops-platform feature; it is performed by the *agent* + the
  *forge*, never the worker. Upstream Symphony is explicit: *"Symphony is a
  scheduler/runner and tracker reader … Ticket writes … are typically performed
  by the coding agent"*
  ([blog](../research/2026-04-27-openai-symphony-blog.md)).
- **A run may end at a handoff state, not `Done`.** The maker stops at
  `Human Review`; the reviewer issues the `Done`/`Rework` verdict — the
  maker/checker split (a fresh model decides "done", not the one that wrote it).
- **The agent shepherds the merge, gated by CI.** Upstream: *"By the time a
  ticket reaches Merging, … the change will make it into the main branch without
  human babysitting. The system watches CI, rebases when needed, … retries flaky
  checks."* Here the reviewer approves + enables forge auto-merge; the forge
  lands it only when required checks pass.
- **Safety baseline** (gitea-bot-and-branch-protection.md): *"Reviewers must
  remain the only path that lands code on `main`"* and any auto-merge *"must be
  opt-in per repository, must require all status checks green, and must still
  require at least one … review."* The review requirement is satisfied by the
  **reviewer bot's** approval (a distinct account from the maker). Keep a periodic
  human audit of merged work as defense in depth.

## Topology

Two worker processes, one Gitea repo/project, **two bot accounts**:

| | maker worker | reviewer worker |
|---|---|---|
| `WORKFLOW.md` | `examples/maker-WORKFLOW.md` | `examples/reviewer-automerge-WORKFLOW.md` |
| Bot account | `maker-bot` (Write) | `review-bot` (Write) — **must differ** |
| `tracker.active_states` | `[Todo, Rework]` | `[Human Review]` |
| `tracker.inactive_states` | `[Human Review, In Progress]` | `[Todo, In Progress, Rework]` |
| `workspace.root` | `~/aiops-workspaces/maker` | `~/aiops-workspaces/reviewer` — **must differ (hard)** |
| `AIOPS_MIRROR_ROOT` | `~/aiops-mirrors/maker` | `~/aiops-mirrors/reviewer` — **must differ (hard)** |
| agent state writes | flips → `aiops/human-review`; opens PR; comments PR URL | approves + enables auto-merge; **confirms the merge, then** flips → `aiops/done`; stays in `Human Review` (re-checks next poll) if CI is still landing; `aiops/rework` on fail |

```
Todo ──maker──▶ Human Review ──reviewer──▶ approve + enable CI-gated auto-merge
                  ▲   │                              │
                  │   └─(rubric fail)─▶ Rework       forge merges when required CI is green
                  │                       │                 │
   reviewer re-polls Human Review         │        reviewer confirms merge ─▶ Done ─▶ (main)
   until the forge confirms merge         │
   (no terminal flip meanwhile)           │
  ▲                                       │
  └────────── maker re-dispatch on Rework ┘
```

**`Done` is issued only AFTER the forge reports the PR merged** — never on the
reviewer's verdict alone. Otherwise a `Depends on #N` dependent (whose gate keys
off the terminal `aiops/*` label, not the forge merge) could unblock and start
N+1 from a stale `main` while CI is still running or later fails. When CI has not
landed within the reviewer's turn budget, it makes **no** terminal flip and leaves
the issue in `Human Review`; the next poll re-claims it and re-checks (see
[Slow or flaky CI](#slow-or-flaky-ci)) — there is no separate holding state.

The two hard isolation requirements (separate `workspace.root` **and** separate
`AIOPS_MIRROR_ROOT` per worker — same issue resolves to the same `PathFor`
directory and the same `ai/<id>` work-branch, so a shared root or mirror is
deterministically broken) are documented in [`reviewer-worker.md`](reviewer-worker.md);
they apply unchanged here.

## 1. Bots and credentials

Create **two** dedicated low-privilege Gitea users, `maker-bot` and `review-bot`,
each a repo collaborator with **Write** (not Admin), 2FA on. Three secrets,
isolated on purpose:

- `GITEA_TOKEN` — worker-held tracker/API token for polling + the
  `gitea_issue_labels` proxy. Never enters the agent process (denied by
  `internal/workflow/agent_env_policy.go`). Scope it to read issues + write issue
  labels/comments.
- `MAKER_CLONE_URL` = `http://maker-bot:<maker-token>@gitea.local/<owner>/<repo>.git`
  — the maker agent's push + PR credential.
- `REVIEWER_CLONE_URL` = `http://review-bot:<review-token>@gitea.local/<owner>/<repo>.git`
  — the reviewer agent's fetch + approve + auto-merge credential.

Clone-URL userinfo is masked in logs / `--print-config` (`workflow.MaskCloneURL`).

## 2. CI that gates the merge

Auto-merge is only as safe as the checks it waits on. Add a PR-triggered CI
workflow whose **job/status name matches the required check** in branch
protection (step 3), kept in lockstep with the maker's `verify.commands`:

```yaml
# .gitea/workflows/ci.yml  (Gitea Actions)
name: ci
on: { pull_request: { branches: [main] } }
jobs:
  build-test:                 # <-- this name is the required status check
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.25' }
      - run: go build ./...
      - run: go test ./...
```

## 3. Branch protection on `main` (opt-in, per repo)

Gitea → repo **Settings → Branches → Protect `main`**:

- **Enable status check** and require `build-test`. Auto-merge waits on it.
- **Require approvals: 1.** The reviewer bot's `APPROVED` review satisfies it; a
  PR author cannot approve their own PR — which is why maker and reviewer are
  different accounts.
- **Dismiss stale approvals on push** / block merge without all checks green.
- **Restrict direct pushes to `main`** (merge only via PR) — reviewers stay the
  only path to `main`.
- Allow the **squash** merge style and enable the repo's **auto-merge** option.
- Bots stay **Write** — none can edit branch protection.

## 4. Run the two workers

Each process gets its own `AIOPS_WORKFLOW_PATH`, `workspace.root` (in the file),
`AIOPS_MIRROR_ROOT` (env), and clone-URL secret. Validate before launching:

```bash
# --- maker ---
export GITEA_TOKEN=...           AIOPS_MIRROR_ROOT=~/aiops-mirrors/maker
export MAKER_CLONE_URL='http://maker-bot:<tok>@gitea.local/<owner>/<repo>.git'
mkdir -p ~/aiops/maker && cp examples/maker-WORKFLOW.md ~/aiops/maker/WORKFLOW.md
worker --print-config ~/aiops/maker            # confirm: source=file, api_key=***, networkAccess=true
AIOPS_WORKFLOW_PATH=~/aiops/maker/WORKFLOW.md worker --port 4000 &

# --- reviewer (separate shell / unit; distinct roots + bot) ---
export GITEA_TOKEN=...           AIOPS_MIRROR_ROOT=~/aiops-mirrors/reviewer
export REVIEWER_CLONE_URL='http://review-bot:<tok>@gitea.local/<owner>/<repo>.git'
mkdir -p ~/aiops/reviewer && cp examples/reviewer-automerge-WORKFLOW.md ~/aiops/reviewer/WORKFLOW.md
worker --print-config ~/aiops/reviewer
AIOPS_WORKFLOW_PATH=~/aiops/reviewer/WORKFLOW.md worker --port 4001 &
```

Production: run each as its own systemd unit
([`binary-deployment.md`](binary-deployment.md)) with its own `EnvironmentFile`.
Keep `agent.default: mock` until the loop is trusted, then switch to
`codex-app-server`. On a userns-restricted host the `workspace-write` bwrap
sandbox fails `--doctor`; run the worker in a container / AppArmor-allowed env
(binary-deployment.md §1).

## 5. Multiple incremental (dependent) tasks

To make increment N+1 build on N's merged code, encode the order in the tracker
so the orchestrator only dispatches unblocked work (blog: *"Agents only start
working on tasks that aren't blocked … execution unfolds … optimally in parallel
for this DAG"*):

- Put `Depends on #N` in issue N+1's body. The Gitea adapter parses it; the
  Todo-blocker gate (SPEC §8.2) holds N+1 until #N reaches a **terminal** aiops
  state (`Done`/`Canceled`).
- Because the reviewer issues `Done` **only after the forge confirms the merge**,
  N+1 unblocks only once N is actually on `main`, so its maker branches from the
  updated tree. (If `Done` were issued on the verdict alone, N+1 could start from a
  stale `main` while N's CI was still running — this is the bug this guide avoids.)
- Independent issues fan out in parallel up to `max_concurrent_agents`.

## End-to-end flow (one issue, unattended)

1. Maker claims `Todo` → implements → `go build/test` green → pushes `ai/<id>` →
   opens PR referencing the issue with a **non-closing** `Refs #<N>` (NOT
   `Closes` — see below) → comments PR URL → flips `Human Review`. Its run stops
   on the next reconcile (issue left the maker's active set).
2. Reviewer claims `Human Review` → fetches head → runs the rubric incl.
   `build/test`. **PASS**: approves (review-bot), enables `merge_when_checks_succeed`,
   then — while still `Human Review` (so it is not reconcile-cancelled) — confirms
   the merge via `GET …/pulls/<number>` → `merged:true`, flips `Done`, and closes
   the issue (it owns closure, since the maker left it open). If CI is slow/flaky
   and the merge has not landed within its budget, it makes **no** terminal flip
   and leaves the issue in `Human Review`; the next poll re-claims it and re-checks
   until the forge confirms the merge.
   **FAIL**: posts findings, flips `Rework` (maker re-dispatches).
3. CI runs on the PR; when `build-test` is green and the approval is present,
   Gitea auto-merges (squash) and deletes the branch. The merge does **not** close
   the issue (the maker used `Refs #<N>`, not a closing keyword); the reviewer
   flips `Done` and closes it only after confirming the merge, never before.
   **Why not `Closes #<N>`:** the poller lists `Human Review` (a non-terminal
   active state) with `state=open`, so an issue auto-closed at merge-time would
   drop out of the reviewer's poll *before* it could set `Done` — stranding it
   (closed + still `Human Review`) and blocking every `Depends on #N` dependent.

## Best-practice checklist

- [ ] Distinct `workspace.root` **and** distinct `AIOPS_MIRROR_ROOT` per worker (both hard).
- [ ] Distinct bot accounts (`maker-bot` ≠ `review-bot`) so the reviewer's approval is valid.
- [ ] `networkAccess: true` in both `turn_sandbox_policy` blocks (push/fetch/API need it).
- [ ] `GITEA_TOKEN` never in agent env; label writes only via `gitea_issue_labels`.
- [ ] Maker `verify.commands` == the required CI checks (a green local run predicts a green merge).
- [ ] Maker never sets `aiops/done`; reviewer is the only Done/auto-merge path.
- [ ] Reviewer issues `aiops/done` **only after the forge confirms the merge** — never on the verdict alone (else `Depends on #N` dependents unblock from stale `main`).
- [ ] Maker references the issue with a **non-closing** `Refs #<N>` (not `Closes`); the reviewer flips `Done` and closes the issue post-merge. A closing keyword auto-closes the issue at merge, dropping it from the reviewer's `state=open` poll before `Done` is set — stranding it and its dependents.
- [ ] Maker uses the numeric issue number (digits of `{{ issue.identifier }}`, no leading `#`) in every API path / issue reference — `{{ issue.identifier }}` renders as `#N` on Gitea.
- [ ] Branch protection: required checks + 1 approval + no direct push to `main` + squash + auto-merge enabled.
- [ ] One-issue-per-PR; agents file follow-up issues for out-of-scope finds.
- [ ] Periodic human audit of auto-merged work (the bot review is the gate, not a human).

## Slow or flaky CI

For fast, stable CI the reviewer confirms the merge inline (poll → `Done`), so you
need nothing more. When CI is slow or flaky, the merge may not land within the
reviewer's turn budget. The reviewer then makes **no terminal flip** and leaves the
issue in `Human Review`; because that is the reviewer's active state, the next poll
re-claims the issue and re-checks the PR — flipping `Done` only once the forge
reports `merged:true`. This re-poll only works because the maker used a non-closing
`Refs #<N>`: an issue auto-closed at merge would drop out of the poller's
`state=open` listing for `Human Review` before `Done` is set. Dependents stay gated
throughout, since the issue never reaches a terminal state before the merge. (On re-claim the reviewer detects its own
prior approval and skips straight to the merge re-check, so it does not re-run the
full rubric every poll.) This re-poll is **bounded**, not infinite: each clean
exit consumes from `agent.max_continuation_turns` (D34; it defaults to `max_turns`,
so the reviewer WORKFLOW sets it explicitly higher). When the budget is exhausted —
e.g. CI stays red after approval (a local-pass / CI-fail mismatch the reviewer's own
`build/test` did not catch) — the orchestrator parks the issue in local `blocked`
(`continuation_budget`); its dependents stay gated, and an operator must investigate
the mismatch and redrive it (raising the budget does not auto-redrive a blocked
claim) — not force `Done`/`Canceled`, either of which would unblock dependents from a
`main` that never received the change. For CI that is *routinely* slower than the
budget, the dedicated Merging worker (#863) is the right tool, not an ever-larger
number.

Upstream Symphony models this landing phase as a dedicated **Merging** state with an
agent that *"watches CI, rebases when needed, resolves conflicts, retries flaky
checks"* ([blog](../research/2026-04-27-openai-symphony-blog.md); upstream
`elixir/WORKFLOW.md`). The Gitea adapter's state set is currently fixed to the six
`aiops/*` labels in the topology table — `validGiteaStateLabels`
(`internal/runner/gitea_tools.go`) rejects any other label, and
`DefaultStateLabelMappings` (`internal/gitea/label_state.go`) has no `Merging` — so a
native `Merging` state is a tracked **code** enhancement
([#863](https://github.com/xrf9268-hue/aiops-platform/issues/863)), not something you
can configure today. Until it lands, the `Human Review` re-poll above is the slow-CI
path.

## What this is NOT

- Not a worker-side merge step — that would violate #76 and race reconcile-cancel
  (#557). The merge is forge-native + agent-triggered.
- Not blind self-merge — the maker cannot approve or land its own PR; a distinct
  reviewer + CI gate it.
